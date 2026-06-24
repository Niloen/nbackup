// Package media is NBackup's storage abstraction, analogous to Amanda's Device
// API. A Volume is a linear medium: an ordered sequence of self-describing files,
// each a Header followed by a payload, addressed by position (file number). This
// one shape maps to a local directory, an object store, or a tape (file marks +
// fast-forward). The medium owns its physical layout — callers never construct
// filenames — so slots can be streamed between volumes (disk <-> tape) uniformly.
// Implementations register themselves, so selecting a medium is a registry lookup.
package media

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// File kinds carried in a Header.
const (
	KindArchive = "archive" // one DLE dump
	KindSeal    = "seal"    // the per-slot metadata record, written last
	KindLabel   = "label"   // a volume label (first file); not part of any slot
)

// Header is the self-describing block at the start of every file on a volume
// (Amanda's dumpfile_t). It carries only identity — what is known before the
// payload is streamed. Measured data (sizes, checksum, member listing) lives in
// the per-slot seal record, not here. A volume is therefore recoverable on its
// own: scanning headers reconstructs the catalog.
type Header struct {
	Slot      string    `json:"slot"`
	Kind      string    `json:"kind"`
	DLE       string    `json:"dle,omitempty"`
	Host      string    `json:"host,omitempty"`
	Path      string    `json:"path,omitempty"`
	Method    string    `json:"method,omitempty"`
	Codec     string    `json:"codec,omitempty"`
	Encrypt   string    `json:"encrypt,omitempty"` // encryption scheme name (gpg); reversed on restore. "" = plaintext. The key is never recorded — gpg resolves it from the ciphertext + keyring.
	Level     int       `json:"level,omitempty"`
	BaseSlot  string    `json:"base_slot,omitempty"`
	Part      int       `json:"part,omitempty"` // 0-based index of this part within its archive (0 = first/only); the archive's total part count lives in the seal (slot.Archive.Parts), not here
	CreatedAt time.Time `json:"created_at"`
}

// FileInfo is a file's position and header, as returned by Files().
type FileInfo struct {
	Pos    int
	Header Header
}

// LabelMagic marks a label record as NBackup's, so a foreign or blank volume is
// never mistaken for one of ours (and so never silently overwritten).
const LabelMagic = "nbackup"

// Label is a volume's self-describing identity, stored as the payload of the
// file-0 label record. It is to a volume what a seal record is to a slot: the
// on-medium fact a catalog caches. Only media whose physical mount is ambiguous
// (tape — the reel behind the drive can be swapped) carry one; address-identified
// media (disk, s3) do not.
type Label struct {
	Magic     string    `json:"magic"`              // LabelMagic — proves the volume is ours
	Name      string    `json:"name"`               // unique, human-facing (e.g. "lto-0007")
	Pool      string    `json:"pool"`               // the medium/pool name; blocks cross-pool clobber
	Sequence  int       `json:"sequence,omitempty"` // ordinal within the pool (optional)
	Epoch     int       `json:"epoch"`              // bumped on every (re)label; detects a stale catalog
	WrittenAt time.Time `json:"written_at"`
}

// Labeled is implemented by media that identify themselves on the medium (tape).
// The engine type-asserts this to decide whether to run the label-verify protocol
// before writing or reading; media that don't implement it are trusted by address.
type Labeled interface {
	// ReadLabel returns the volume's label. ok is false only when the volume is
	// blank (no files). A non-empty volume whose file 0 is not a valid NBackup
	// label is reported as ErrForeignVolume — it must not be silently overwritten.
	ReadLabel() (lbl Label, ok bool, err error)
	// WriteLabel resets the volume to empty and writes lbl as file 0. This is the
	// (re)labeling operation and destroys any existing contents; the caller owns
	// the policy decision of whether that is allowed.
	WriteLabel(lbl Label) error
}

// ErrForeignVolume reports a non-empty volume whose file 0 is not an NBackup
// label (someone else's tape, or non-NBackup data).
var ErrForeignVolume = fmt.Errorf("foreign volume: file 0 is not an NBackup label")

// ErrVolumeFull reports that a write hit the end of the volume (a finite volume's
// capacity, e.g. a tape). The partial file is discarded (left unsealed, so a scan
// ignores it). Spanning is PROACTIVE: the writer sizes each archive part to fit the
// loaded volume's known remaining capacity and rolls onto the next volume between
// parts, so this error is the backstop for an estimate that came up short (or a
// volume whose remaining capacity software cannot see ahead) — the caller fails with
// an actionable message rather than recovering. Callers test it with errors.Is.
var ErrVolumeFull = fmt.Errorf("volume full: end of volume reached")

// ErrNoVolume reports that an operation needs a volume mounted in the drive, but
// the drive is empty — a changer (tape library, removable-disk tray, …) with
// nothing loaded. The engine wraps this with medium-specific guidance.
var ErrNoVolume = fmt.Errorf("no volume loaded in the drive")

// ErrNoPerSlotRemoval reports that a medium cannot delete an individual slot:
// space is reclaimed by reusing a whole volume (relabel), not by removing one
// slot's files. Object stores (disk, cloud) support per-slot removal; tape does
// not. Callers test it with errors.Is and fall back to whole-volume reuse — e.g.
// reclaiming an unsealed leftover means leaving it for the next relabel rather
// than failing. The engine never reaches it for capacity pruning (tape defers all
// reclamation to relabel), only when tidying a failed write's partial.
var ErrNoPerSlotRemoval = fmt.Errorf("per-slot removal unsupported; reuse is whole-volume (relabel)")

// Drive is a medium with one mounted volume at a time: it can report what is loaded.
// Both a robotic library (the volume the robot has in the drive) and a single-drive
// station (the loaded reel) are Drives; address-identified media (disk, s3) are not.
// It is the device read every changer shape shares, kept minimal so callers that
// only need "what's loaded" — capacity pre-checks, inventory — don't depend on the
// positioning or room interfaces.
//
// A Drive IS a Volume — the one currently in the drive: the embedded Volume
// operations act on the loaded volume.
type Drive interface {
	Volume
	// Loaded reports the volume currently in the drive; ok is false when empty.
	Loaded() (VolumeStatus, bool)
}

// Changer is the robotic-library device: a drive fed by a robot that mounts any of
// many physical bays. It is what distinguishes a robotic library from a single-drive
// station — the library reaches every tape itself (Bays + Mount), the station cannot
// and so does NOT implement Changer (it is a Drive plus a Shelf instead). Holding a
// Volume, the librarian reads the shape from this one assertion: a Changer is a
// robotic library; anything else is a single drive or a plain volume.
//
// It is deliberately label-AGNOSTIC: like a real robot it addresses bays and never
// reads the magnetic label itself; the label is read only after a bay is mounted
// (via Labeled.ReadLabel). It carries ONLY what real hardware's software can do —
// position the robot among bays it can reach.
type Changer interface {
	Drive
	// Bays lists the physical positions the robot can mount. Every reported id is a
	// valid Mount target.
	Bays() ([]VolumeStatus, error)
	// Mount loads the named bay into the drive (a robot move).
	Mount(bay string) error
}

// Shelf is the operator-managed environment of a single-drive station — the reels in
// the room and the act of loading one into the one drive. Loading a reel a human
// keeps on a shelf is a physical act with no device API, so it lives here, apart
// from the Drive/Changer device seams. The librarian consults it only to actually do
// a swap (prompt over the room, then Insert the operator's choice); it is never a
// general shape marker.
//
// A real standalone drive implements Shelf degenerately: an empty room (software
// cannot see the reels) and an Insert that errors (only a human loads it). The disk
// emulator implements it functionally — its reels are directories it can enumerate
// and load — so the manual-swap UX is exercisable in one process.
type Shelf interface {
	// Shelf lists the reels in the room but not currently in the drive. Empty for a
	// real drive (software cannot see the room).
	Shelf() ([]VolumeStatus, error)
	// Insert loads the named room reel into the single drive, displacing whatever is
	// loaded back to the room. A real drive returns an error (only a human can load
	// it); the emulator effects the swap in software.
	Insert(reel string) error
}

// VolumeStatus is one volume's physical state: a bay in a Library, or the reel
// in (or available to) a Station's drive. Label is the volume label written on the
// cartridge ("" when blank) — for the disk emulator it stands in for the barcode a
// real library's reader would report without a drive read.
type VolumeStatus struct {
	ID       string // bay id (Library) or reel id (Station shelf); "" for a real drive
	Label    string
	Blank    bool
	Foreign  bool // holds non-NBackup data: not writable without a forced relabel
	Used     int64
	Capacity int64
	Files    int
}

// Options carries medium-specific configuration to a factory as generic
// key/value parameters (e.g. "path" for disk, "bucket" for s3).
type Options map[string]string

// Get returns the value for a parameter key, or "".
func (o Options) Get(key string) string { return o[key] }

// Volume is a medium holding an ordered sequence of header-framed files.
//
// Contract: opening a Volume must be cheap (no reading every file), and
// AppendFile/ReadFile must not scan — they seek by position. Only Files() is a
// full pass over the volume; it is the catalog-rebuild path (on tape, a literal
// scan from the start, as Amanda re-reads a tape). Normal backup/restore/copy
// resolve positions from the catalog and call ReadFile, never Files().
type Volume interface {
	// AppendFile writes h, then the payload produced by write, and returns the
	// file's position. The Volume owns concurrency and position assignment
	// (disk allows concurrent appends; tape serializes).
	AppendFile(h Header, write func(w io.Writer) error) (pos int, err error)
	// ReadFile positions to pos and returns its header and a payload stream the
	// caller must close.
	ReadFile(pos int) (Header, io.ReadCloser, error)
	// Files returns every file's position and header in order — the volume's
	// self-index, used to rebuild the catalog. May be O(volume) (a full scan).
	Files() ([]FileInfo, error)
	// RemoveSlot reclaims every file belonging to a slot.
	RemoveSlot(slot string) error
}

// HeaderBlock is the fixed size of the leading header block on every file. A
// fixed block (as on Amanda tapes) makes payload extraction uniform across media
// and keeps stock-tool recovery simple: `dd bs=32k skip=1 < file | zstd -dc`.
const HeaderBlock = 32 * 1024

// EncodeHeader writes h as a fixed-size, newline-terminated JSON block — the
// framing every Volume implementation puts at the start of a file.
func EncodeHeader(w io.Writer, h Header) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > HeaderBlock {
		return fmt.Errorf("header too large (%d > %d)", len(b), HeaderBlock)
	}
	block := make([]byte, HeaderBlock)
	copy(block, b)
	_, err = w.Write(block)
	return err
}

// DecodeHeader reads the fixed header block from r, leaving r positioned at the
// payload.
func DecodeHeader(r io.Reader) (Header, error) {
	block := make([]byte, HeaderBlock)
	if _, err := io.ReadFull(r, block); err != nil {
		return Header{}, err
	}
	line := block
	if i := indexByte(block, '\n'); i >= 0 {
		line = block[:i]
	}
	var h Header
	if err := json.Unmarshal(line, &h); err != nil {
		return Header{}, fmt.Errorf("parse file header: %w", err)
	}
	return h, nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// VolumeFactory constructs a Volume from options.
type VolumeFactory func(Options) (Volume, error)

var volumeFactories = map[string]VolumeFactory{}

// RegisterVolume registers a Volume implementation under a medium type name.
func RegisterVolume(typ string, f VolumeFactory) { volumeFactories[typ] = f }

// OpenVolume constructs the Volume registered for the given medium type.
func OpenVolume(typ string, opts Options) (Volume, error) {
	f, ok := volumeFactories[typ]
	if !ok {
		return nil, fmt.Errorf("unknown medium type %q (known: %v)", typ, VolumeTypes())
	}
	return f(opts)
}

// paramKeys records the inline option keys each medium type accepts. Each medium
// implementation declares its own (next to RegisterVolume), so the source of truth
// for a type's options lives with that type — and a typo'd or unknown key is
// rejected at config load instead of being silently ignored.
var paramKeys = map[string]map[string]bool{}

// RegisterParams records the inline option keys a medium type accepts. The common
// fields (type, capacity, minimum_age, appendable) are struct fields, not inline
// params, so they are not listed here. Keys a type recognizes but rejects (e.g.
// part_size on an unbounded medium) should still be listed, so the factory's
// specific error is what the operator sees, not a blanket "unknown option".
func RegisterParams(typ string, keys ...string) {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	paramKeys[typ] = set
}

// ValidateParams checks a medium's inline params against the keys its type accepts,
// returning an error naming the unknown key(s) and the accepted ones. A type with
// no registered keys is not validated (lenient, like OpenProfile for an unknown type).
func ValidateParams(typ string, params map[string]string) error {
	known, ok := paramKeys[typ]
	if !ok {
		return nil
	}
	var unknown []string
	for k := range params {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	allowed := make([]string, 0, len(known))
	for k := range known {
		allowed = append(allowed, k)
	}
	sort.Strings(allowed)
	return fmt.Errorf("unknown %s option(s) %s; accepted options: %s",
		typ, strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

// VolumeTypes lists registered medium types.
func VolumeTypes() []string {
	out := make([]string, 0, len(volumeFactories))
	for k := range volumeFactories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrNotImplemented is returned by registered-but-incomplete media.
var ErrNotImplemented = fmt.Errorf("not implemented in this version")
