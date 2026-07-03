// Package media is NBackup's storage abstraction. A Volume is a linear medium:
// an ordered sequence of self-describing files, each a record.Header followed by
// a payload, addressed by position (file number). This one shape maps to a local
// directory, an object store, or a tape (file marks + fast-forward). The medium
// owns its physical layout — callers never construct filenames — so runs can be
// streamed between volumes (disk <-> tape) uniformly. Implementations register
// themselves, so selecting a medium is a registry lookup. The on-medium artifact
// format (headers, labels, seals) lives in package record; this package is the
// device side that reads and writes it.
package media

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/record"
)

// Labeled is implemented by media that identify themselves on the medium (tape).
// The librarian (and the catalog's volume scan) type-assert this to decide whether
// to run the label-verify protocol before writing or reading; the engine never sees
// a Volume's shape. Media that don't implement it are trusted by address.
type Labeled interface {
	// ReadLabel returns the volume's label. ok is false only when the volume is
	// blank (no files). A non-empty volume whose file 0 is not a valid NBackup
	// label is reported as ErrForeignVolume — it must not be silently overwritten.
	ReadLabel() (lbl record.Label, ok bool, err error)
	// WriteLabel resets the volume to empty and writes lbl as file 0. This is the
	// (re)labeling operation and destroys any existing contents; the caller owns
	// the policy decision of whether that is allowed.
	WriteLabel(lbl record.Label) error
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

// ErrManualLoad reports that a changer cannot move media itself: a real standalone
// drive (no robot) where only a human loads a cartridge. Changer.Load returns it so
// the librarian prompts the operator and re-reads the drive, rather than treating an
// unmovable drive as a hard failure. Callers test it with errors.Is.
var ErrManualLoad = fmt.Errorf("changer cannot load media itself; a human must load the drive")

// ErrNoFileRemoval reports that a medium cannot delete an individual file: space
// is reclaimed by reusing a whole volume (relabel), not by removing files. Object
// stores (disk, cloud) support per-file removal; tape does not. Callers test it
// with errors.Is and fall back to whole-volume reuse — e.g. reclaiming an unsealed
// leftover means leaving it for the next relabel rather than failing. The engine
// never reaches it for capacity pruning (tape defers all reclamation to relabel),
// only when tidying a failed write's partial.
var ErrNoFileRemoval = fmt.Errorf("per-file removal unsupported; reuse is whole-volume (relabel)")

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
	// Loaded reports the cartridge currently in the drive (its barcode, label, fill);
	// ok is false when the drive is empty.
	Loaded() (VolumeStatus, bool)
}

// Changer is a tape library's logistics: a set of drives (data-transfer elements)
// fed from a set of slots (storage elements) by a robot — or, for a lone drive, by a
// human. It moves cartridges and reports inventory; it never does byte I/O (that is a
// Drive/Volume) and never reads an on-tape label (that needs a load — a real library
// knows only the physical barcode its scanner reads). One Changer models every shape:
// a robotic library (many slots, one or more drives, software loads), and a single
// drive (no addressable slots, one drive, a human loads — see Manual).
//
// The shape is one assertion: a Volume that is also a Changer is a library of
// removable cartridges; anything else is a single, directly-addressed volume (disk,
// s3). The librarian holds the Changer and hands Volumes upward, so nothing above it
// addresses slots or drives.
type Changer interface {
	// Slots inventories the storage elements: each cartridge the library can see by
	// barcode, WITHOUT loading it. A slot whose cartridge is currently in a drive
	// reports empty. A single drive has no addressable slots (its shelf is offline,
	// invisible to software) and returns none.
	Slots() ([]SlotStatus, error)
	// Drives inventories the data-transfer elements: each drive and what it holds.
	Drives() ([]DriveStatus, error)
	// Drive returns the stable byte handle for drive i. Its Volume operations act on
	// whatever cartridge is loaded there now (ErrNoVolume when empty); a later Load
	// rebinds the bytes under the same handle, so a write sink spans cartridges
	// without being re-pointed.
	Drive(i int) Drive
	// Load places the cartridge in slot into drive (a robot move). A Manual changer
	// (a single drive a human loads) returns ErrManualLoad — the librarian then
	// prompts the operator instead of loading itself.
	Load(slot, drive int) error
	// Unload returns the cartridge in drive to a slot (its home, or any free one).
	Unload(drive int) error
	// Manual reports whether loading needs a human: true for a real standalone drive
	// (and a file-backed changer configured to simulate one), false for a robot that
	// loads unattended. It is the librarian's cue to prompt an operator rather than
	// pick a slot and Load it itself.
	Manual() bool
}

// SlotStatus is one storage element's state: its address and the barcode of the
// cartridge it holds. The barcode is the physical VolumeTag the library scanner
// reads without loading — the cheap identity used to pick which slot to load; the
// on-tape Label is read only after the cartridge reaches a drive.
type SlotStatus struct {
	Slot         int    // storage-element address (1-based, as the library numbers them)
	Barcode      string // physical VolumeTag; "" when the slot is empty or has no scanner
	Full         bool
	ImportExport bool // a mailslot (import/export element), not a normal storage slot
}

// DriveStatus is one data-transfer element's state: its address, what cartridge is
// loaded (by barcode and home slot), and the loaded volume's physical fill.
type DriveStatus struct {
	Drive    int          // data-transfer-element address (0-based)
	Loaded   bool         // a cartridge is in the drive
	FromSlot int          // the slot it came from, for Unload; -1 when unknown
	Volume   VolumeStatus // the loaded cartridge's barcode/label/fill (zero when empty)
}

// VolumeStatus is one cartridge's physical state, as seen once loaded (or scanned).
// Barcode is the physical VolumeTag; Label is the on-tape identity ("" when blank or
// unread). For the file-backed changer the barcode is a stable simulated tag, distinct
// from the label, so the barcode-vs-label split is exercised without hardware.
type VolumeStatus struct {
	ID       string // slot/drive id for display; "" when not applicable
	Barcode  string // physical VolumeTag (scanner identity), distinct from the on-tape Label
	Label    string
	Pool     string // the label's pool (the owning medium); "" when blank/foreign/unread
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

// RejectPartSize returns an error when opts sets part_size on an unbounded medium
// (disk) that never splits an archive into parts — so the knob is refused with one
// shared message rather than silently ignored. Spanning media (tape) and part-splitting
// object stores (cloud) accept and honor part_size instead.
func RejectPartSize(opts Options, mediumType string) error {
	if opts.Get("part_size") != "" {
		return fmt.Errorf("%s medium does not support part_size (it is unbounded and never splits archives)", mediumType)
	}
	return nil
}

// PartSizePolicy is a medium type's posture toward the part_size knob: the default
// applied when the operator leaves it unset, and an optional upper bound (with a
// medium-specific note) that guards a value the write path cannot honor. A type with
// no registered policy (disk, tape) has neither — part_size is unset-means-unbounded
// and bounded only by the shared lower limit the engine enforces.
type PartSizePolicy struct {
	Default int64  // part_size applied when unset (0 = none: a single unbounded part)
	Max     int64  // upper bound for an explicit part_size (0 = no upper bound)
	MaxNote string // appended to the over-max error to explain the medium's limit
}

// PartSizeFor returns a medium type's PartSizePolicy (the zero policy when none is
// registered). The engine consults it to default and bound a medium's part_size. The
// cloud medium declares one to default to a moderate object size and cap the object
// below the object store's multipart-upload ceiling; the generic layer never hardcodes
// a type.
func PartSizeFor(typ string) PartSizePolicy { return specs[typ].PartSize }

// FileWriter is the payload writer AppendFile hands back: write the payload, then Close to commit
// (or cancel the AppendFile ctx before Close to abort). Pos reports the file's on-volume position and
// is valid only after a successful Close.
type FileWriter interface {
	io.WriteCloser
	Pos() int
}

// Volume is a medium holding an ordered sequence of header-framed files.
//
// Contract: opening a Volume must be cheap (no reading every file), and
// AppendFile/ReadFile must not scan — they seek by position. Only Files() is a
// full pass over the volume; it is the catalog-rebuild path (on tape, a literal
// scan from the start). Normal backup/restore/copy
// resolve positions from the catalog and call ReadFile, never Files().
type Volume interface {
	// AppendFile begins a header-framed file for h and returns a writer for its payload. The caller
	// writes the payload and Closes the writer to commit the file; FileWriter.Pos then reports where
	// it landed. To abort — leave no committed file — the caller cancels ctx before Close: Close then
	// discards the partial (a cloud upload is abandoned; a disk payload is left a sidecar-less orphan a
	// scan ignores). The Volume owns concurrency and position assignment (disk allows concurrent
	// appends; tape serializes).
	AppendFile(ctx context.Context, h record.Header) (FileWriter, error)
	// ReadFile positions to pos and returns its header and a payload stream the
	// caller must close.
	ReadFile(pos int) (record.Header, io.ReadCloser, error)
	// Files returns every file's position and header in order — the volume's
	// self-index, used to rebuild the catalog. May be O(volume) (a full scan).
	//
	// Files enumerates only committed files and must NOT fail on a partial artifact
	// left by an interrupted append (a hard kill or power loss mid-write). An
	// artifact that is absent, truncated, or whose header will not parse is treated
	// as uncommitted and skipped, so the rebuild always completes. What "committed"
	// means at the file layer is medium-specific (fslike: a payload paired with its
	// later-written header sidecar; tape: a fully-framed, decodable record);
	// archive-level commit is the commit footer above this. Integrity of files the
	// footer *does* commit (bit-rot) is verify's job, not enumeration's — Files
	// never asserts it.
	Files() ([]record.FileInfo, error)
	// RemoveFile reclaims the file at pos — the positional peer of ReadFile. It is how
	// every reclaimer (per-archive pruning, orphan tidy-up, the drill WORM probe)
	// deletes: callers resolve the positions they want gone (from the catalog or a
	// Files() scan) and remove them one by one, so the Volume seam stays purely
	// positional and never names a higher-level grouping. Removing a missing position
	// is a no-op (idempotent). A medium grouping files on disk (fslike's per-slot
	// directory) reclaims an emptied group itself. Whole-volume media (tape) that
	// cannot delete an individual file return ErrNoFileRemoval.
	RemoveFile(pos int) error
}

// IncompleteEnumerator is implemented by per-file media (fslike: disk, cloud) that
// track files an interrupted append left half-written — a payload with no header
// sidecar, or the reverse. Such a fragment belongs to no archive: a catalog scan and
// Files() both skip it, so it is invisible to retention yet still holds capacity. A
// prune sweep reaps it by position via RemoveFile, which deletes whichever half is
// present. Whole-volume media (tape) reclaim by relabel and do not implement this.
type IncompleteEnumerator interface {
	// IncompleteFiles returns the positions of files missing one of their
	// payload/header halves. It excludes both well-formed files (which Files()
	// reports) and not-yet-written in-flight reservations (neither half present).
	IncompleteFiles() ([]int, error)
}

// WalkReadable visits every readable cartridge reachable from vol in turn, calling fn
// for each one loaded in a drive. It is the medium-shape primitive the catalog rebuild
// scan needs, kept here next to the shape interfaces it asserts on (Changer) so the
// catalog never type-asserts a Volume itself:
//
//   - a robotic library (a Changer) visits whatever is already in its drives, then
//     loads each occupied slot into drive 0 in turn, restoring drive 0 when done;
//   - a single drive a human loads (a Manual Changer) can reach only the cartridge
//     already in it — the rest sit offline, unloadable unattended — so fn sees that
//     one volume, or nothing when the drive is empty;
//   - a plain address-identified volume (disk, s3) is visited directly.
//
// It positions only — it never reads labels — so it is a pure shape walk, distinct
// from the librarian's label-aware advance.
func WalkReadable(vol Volume, fn func(Volume) error) error {
	ch, isChanger := vol.(Changer)
	if !isChanger {
		return fn(vol) // a single directly-addressed volume (disk, s3)
	}
	drives, err := ch.Drives()
	if err != nil {
		return err
	}
	// Visit cartridges already loaded in drives, and note drive 0's cartridge so it
	// can be restored after the slot scan borrows that drive.
	restore := -1
	for _, d := range drives {
		if !d.Loaded {
			continue
		}
		if d.Drive == 0 {
			restore = d.FromSlot
		}
		if err := fn(ch.Drive(d.Drive)); err != nil {
			return err
		}
	}
	if ch.Manual() {
		return nil // a human-loaded drive reaches only what is already in it
	}
	slots, err := ch.Slots()
	if err != nil {
		return err
	}
	scanned := false
	for _, s := range slots {
		if !s.Full || s.ImportExport {
			continue
		}
		if err := ch.Load(s.Slot, 0); err != nil {
			// A cartridge this drive cannot load (wrong generation, stuck, a dud) holds
			// nothing readable for the catalog — skip it rather than abort the scan.
			continue
		}
		scanned = true
		if err := fn(ch.Drive(0)); err != nil {
			return err
		}
	}
	if restore >= 0 {
		if err := ch.Load(restore, 0); err != nil {
			return err
		}
	} else if scanned {
		// The drive started empty; leave it that way rather than holding
		// whichever slot the scan happened to visit last.
		if err := ch.Unload(0); err != nil {
			return err
		}
	}
	return nil
}

// VolumeFactory constructs a Volume from options.
type VolumeFactory func(Options) (Volume, error)

// Spec is everything the media layer knows about one medium type, declared in a single
// registration. Bundling the facts — the Volume constructor, the capacity Profile and
// pricing Cost factories, the accepted inline Params, the part-size policy, and the
// concurrent-write capability — keeps a medium's registration cohesive: all of its
// properties live in one struct literal next to the constructor, and a capability like
// ConcurrentWrite is a named field with a visible default rather than a separate call a
// medium can silently forget. Media built on a shared layout (disk and cloud over
// fslike) start from that layer's base Spec (fslike.Spec) and fill in only what is
// distinctive to the backing store.
type Spec struct {
	Type string // the config `type:` name (e.g. "disk", "cloud", "tape")

	// New constructs the Volume from its options. Required.
	New VolumeFactory
	// Profile builds the capacity/reclamation model. Nil is treated as unbounded.
	Profile ProfileFactory
	// Cost builds the pricing model. Nil means unpriced (local disk, tape).
	Cost CostFactory

	// Params are the inline option keys the type accepts. The common fields (type,
	// capacity, minimum_age, appendable) are struct fields, not inline params, so they
	// are not listed. Keys a type recognizes but rejects (part_size on an unbounded
	// medium) should still be listed, so the factory's specific error is what the
	// operator sees, not a blanket "unknown option".
	Params []string
	// PartSize is the type's posture toward the part_size knob (the zero policy means
	// unset-means-unbounded, bounded only by the shared lower limit).
	PartSize PartSizePolicy
	// ConcurrentWrite marks a medium safe for concurrent appends and per-file reclaim —
	// the capability a holding disk requires (disk, cloud). The false default is a
	// serial, whole-volume medium (tape) that shares one rolling volume.
	ConcurrentWrite bool
}

// specs is the single medium registry: one Spec per type, populated by Register from each
// medium's init(). It replaces the former per-property maps so a type's facts live in one
// place rather than being scattered across several registration calls.
var specs = map[string]Spec{}

// Register records a medium type's Spec. Each medium calls it once from init().
func Register(s Spec) { specs[s.Type] = s }

// ConcurrentWrite reports whether a medium type accepts concurrent writes and per-file
// reclaim — the property a holding disk needs: parallel dumpers share its write sink, and
// the taper reclaims each archive as it drains to the landing. A serial, whole-volume
// medium returns false.
func ConcurrentWrite(typ string) bool { return specs[typ].ConcurrentWrite }

// OpenVolume constructs the Volume registered for the given medium type.
func OpenVolume(typ string, opts Options) (Volume, error) {
	s, ok := specs[typ]
	if !ok || s.New == nil {
		return nil, fmt.Errorf("unknown medium type %q (known: %v)", typ, VolumeTypes())
	}
	return s.New(opts)
}

// ValidateParams checks a medium's inline params against the keys its type accepts,
// returning an error naming the unknown key(s) and the accepted ones. A type with
// no registered keys is not validated (lenient, like OpenProfile for an unknown type).
func ValidateParams(typ string, params map[string]string) error {
	s, ok := specs[typ]
	if !ok || len(s.Params) == 0 {
		return nil
	}
	known := make(map[string]bool, len(s.Params))
	for _, k := range s.Params {
		known[k] = true
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
	allowed := append([]string(nil), s.Params...)
	sort.Strings(allowed)
	// capacity/minimum_age are common struct fields on every medium (not inline
	// params), so name them too — a typo'd `capacity` otherwise sees a list that
	// omits the very key it meant.
	return fmt.Errorf("unknown %s option(s) %s; accepted options: %s (plus the common medium fields capacity, minimum_age)",
		typ, strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

// KnownVolumeType reports whether a medium type is registered — a config-validity
// check distinct from runtime readiness, so `nb check` can fail an unknown type
// (a config error) rather than treating it as a transient reachability warning.
func KnownVolumeType(typ string) bool {
	s, ok := specs[typ]
	return ok && s.New != nil
}

// VolumeTypes lists registered (constructable) medium types.
func VolumeTypes() []string {
	out := make([]string, 0, len(specs))
	for k, s := range specs {
		if s.New != nil {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
