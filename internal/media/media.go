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
	Level     int       `json:"level,omitempty"`
	BaseSlot  string    `json:"base_slot,omitempty"`
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
// capacity, e.g. a tape). The partial file is discarded; without spanning, the
// whole archive must be rewritten on another volume. Callers test it with errors.Is.
var ErrVolumeFull = fmt.Errorf("volume full: end of volume reached")

// ErrNoVolume reports that an operation needs a volume mounted in the drive, but
// the drive is empty — a changer (tape library, removable-disk tray, …) with
// nothing loaded. The engine wraps this with medium-specific guidance.
var ErrNoVolume = fmt.Errorf("no volume loaded in the drive")

// Library is a robotic, bay-addressed changer: a tape library / autochanger whose
// robot mounts any of many physical positions (bays) into the one drive. It is
// deliberately label-AGNOSTIC — like a real robot it addresses bays and never reads
// the magnetic label itself; the label is read from the medium only after a bay is
// mounted (via Labeled.ReadLabel). The engine resolves labels↔bays on top of this
// seam. Every id reported by Bays()/Loaded() is a valid Mount() target.
//
// A Library IS a Volume — the one currently mounted in the drive: the embedded
// Volume operations act on the mounted bay, and Mount changes which bay that is.
// A Station (single-drive station) is also a Volume but is NOT a Library: it has no
// robot and no addressable bays, so the two are siblings, not a subtype. ("Siblings,
// not subtype" is about Library vs Station — both are Volumes.)
type Library interface {
	Volume
	// Mount loads the named bay into the drive (error if the bay does not exist).
	// Subsequent Volume/Labeled operations act on the mounted bay.
	Mount(bay string) error
	// Loaded returns the bay currently in the drive; ok is false when empty.
	Loaded() (bay string, ok bool)
	// Bays lists the library's physical positions and what each holds.
	Bays() ([]VolumeStatus, error)
}

// Station is a single-drive medium whose loaded volume an operator changes by hand:
// a standalone tape drive, or its disk-emulated equivalent. Unlike a Library it has
// no robot and no addressable bays — the software sees only the one volume in the
// drive, never an inventory of the others. The engine prompts the operator to swap
// rather than mounting automatically.
//
// A Station IS a Volume — the one currently in the drive: the embedded Volume
// operations act on the loaded reel.
type Station interface {
	Volume
	// LoadedVolume reports the volume currently in the drive; ok is false when the
	// drive is empty.
	LoadedVolume() (VolumeStatus, bool)
}

// ShelfStation is a Station whose off-drive volumes the software can itself
// enumerate and load — the disk-emulated single-drive station, where the reels are
// directories on a shelf. A real standalone tape drive is a Station but NOT a
// ShelfStation: its reels are invisible to software and its swaps are purely
// physical (the operator loads a tape by hand; the software only re-reads the
// drive). Insert effects the swap in software, displacing whatever is loaded back
// to the shelf.
type ShelfStation interface {
	Station
	// Shelf lists the reels in the room but not currently in the drive.
	Shelf() ([]VolumeStatus, error)
	// Insert swaps the named shelf reel into the single drive. id is a reel id
	// reported by Shelf.
	Insert(id string) error
}

// VolumeStatus is one volume's physical state: a bay in a Library, or the reel
// in (or available to) a Station's drive. Label is the volume label written on the
// cartridge ("" when blank) — for the disk emulator it stands in for the barcode a
// real library's reader would report without a drive read.
type VolumeStatus struct {
	ID       string // bay id (Library) or reel id (Station shelf); "" for a real drive
	Label    string
	Blank    bool
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
		return nil, fmt.Errorf("unknown medium %q (known: %v)", typ, VolumeTypes())
	}
	return f(opts)
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

// CopySlot streams every file of a slot from src to dst, in position order
// (archives then the seal record), so the slot lands sealed on dst. Slot metadata
// is position-free, so dst assigns its own positions — this is the one mechanism
// for moving slots between any two media (disk <-> tape). It returns the copied
// files with their headers and their new positions on dst, so the caller can
// record where each archive landed.
func CopySlot(dst, src Volume, slotID string) ([]FileInfo, error) {
	files, err := src.Files()
	if err != nil {
		return nil, err
	}
	var copied []FileInfo
	for _, f := range files {
		if f.Header.Slot != slotID {
			continue
		}
		pos, err := copyOne(dst, src, f)
		if err != nil {
			return copied, err
		}
		copied = append(copied, FileInfo{Pos: pos, Header: f.Header})
	}
	if len(copied) == 0 {
		return nil, fmt.Errorf("slot %s not found on the source volume", slotID)
	}
	return copied, nil
}

func copyOne(dst, src Volume, f FileInfo) (int, error) {
	_, rc, err := src.ReadFile(f.Pos)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	return dst.AppendFile(f.Header, func(w io.Writer) error {
		_, e := io.Copy(w, rc)
		return e
	})
}
