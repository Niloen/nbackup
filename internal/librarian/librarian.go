// Package librarian operates a medium's changer for the engine: it turns operator-
// level intents — make a volume writable, mount the one holding an archive, advance to
// the next writable volume, (re)label, load, inventory — into the right positioning
// calls, and runs the label protocol (pool/epoch/appendable/auto-label) on top.
//
// It is the single place that knows a medium's physical shape, so the rest of the
// engine stays shape-agnostic — everything above the librarian handles only Volumes.
// A tape medium is a media.Changer: a set of drives fed from a set of slots. The
// changer's Manual() says who loads it — a robot (advance by loading the next slot,
// unattended) or a human (prompt the operator over the slots, then load their choice).
// A directly-addressed medium (disk, s3) is wrapped in a trivial one-drive changer so
// the librarian has a single shape for all of them.
//
// The librarian is a shared service: dump, copy/sync, restore, rebuild, label, and
// load all bottom out in "present the right volume on medium X". It depends only on
// media, the catalog, and retention — never on the engine — so it is the seam future
// sub-engines (a dump run, a catalog refresher, a copier) will each consume.
package librarian

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// Operator handles physical actions software cannot perform itself — chiefly
// swapping the reel in a single-drive (manual) station when the loaded tape won't
// do. The CLI implements it interactively over stdin; an unattended run leaves it
// nil, so a manual swap degrades to an actionable error instead of blocking forever
// waiting for a human.
type Operator interface {
	// Swap asks the operator to load a volume for the stated need and returns the
	// chosen volume's id (from req.Shelf), or ok=false to abort (leaving the drive
	// unchanged). (The CLI presents this as a tape/reel swap; the seam itself is
	// medium-neutral so a future removable medium can reuse it.)
	Swap(req SwapRequest) (volume string, ok bool)
}

// SwapRequest describes why a manual tape swap is needed and what is available.
type SwapRequest struct {
	Medium string               // medium name (also the label pool — a medium owns one pool)
	Reason string               // why the loaded reel won't do (the underlying error)
	Need   string               // a specific volume label wanted (read); "" means any writable (write)
	Expect string               // on a write, the label the run expects to (re)use (the oldest reusable volume); "" when a fresh tape is expected
	Loaded media.VolumeStatus   // what is in the drive now (zero value when empty)
	Shelf  []media.VolumeStatus // reels available to load
}

// Logf is an optional progress logger.
type Logf func(format string, args ...any)

func (l Logf) log(format string, args ...any) {
	if l != nil {
		l(format, args...)
	}
}

// ErrAllVolumesProtected marks the fail-loud case where a write needs a fresh volume
// but every volume in the pool is still within retention and none is blank — the
// rotation's safety backstop (recoverability outranks capacity). It is a sentinel so the
// write path can surface this actionable verdict in preference to the loaded volume's
// bare "won't do" reason, while still preferring that reason for a blank/foreign tape.
var ErrAllVolumesProtected = errors.New("all volumes in the pool hold protected runs")

// ErrVolumeUnavailable marks a read mount that could not place the needed volume in the
// drive: a robotic library that does not hold it, or a single-drive station where no
// operator loaded it. It wraps the actionable message so callers (the drill) can classify
// "missing copy" via errors.Is rather than matching message text.
var ErrVolumeUnavailable = errors.New("needed volume is not available")

// reloadable wraps a write-eligibility failure that loading a different volume
// could fix (so a single drive can prompt for a swap), as opposed to a hard failure
// (stale catalog, I/O) that swapping would not help.
type reloadable struct{ error }

func (r reloadable) Unwrap() error { return r.error }

func reloadableErr(format string, a ...any) error { return reloadable{fmt.Errorf(format, a...)} }

func isReloadable(err error) bool { r := reloadable{}; return errors.As(err, &r) }

// errBlankNeedsLabel marks the one reloadable reason a fresh swap cannot resolve on
// a single-drive station: a blank reel with auto_label off. Loading yet another blank
// only repeats the rejection, so the spanning loop fails fast on it (errors.Is) with
// the actionable message below rather than re-prompting forever.
var errBlankNeedsLabel = errors.New("run `nb label` on it first, or set auto_label: true to label fresh reels automatically")

// Librarian wraps one medium's volume and drives its changer. It is constructed per
// medium the engine touches; it caches the medium's resolved shape so callers never
// type-assert a Volume themselves. A directly-addressed medium (disk, s3) is wrapped
// in a trivial changer adapter so the librarian has one shape for everything; the
// engine above only ever sees the Volume.
type Librarian struct {
	vol       media.Volume
	medium    string
	cat       *catalog.Catalog
	op        Operator
	autoLabel bool
	minAge    time.Duration // this medium's retention minimum age; gates which volumes recycle

	changer   media.Changer // the tape changer, or directChanger for disk/s3
	isChanger bool          // vol is a real media.Changer (tape) — not the adapter, so it can span
	manual    bool          // changer.Manual(): a human loads (prompt the operator) vs a robot loads

	// drive is which data-transfer element this librarian handle drives. A base librarian
	// starts at drive 0 for reads, labels, and single-drive writes; forDrive(i) returns a
	// sibling bound to drive i for concurrent multi-drive writing. A read may rebind it:
	// MountForRead points the handle at whichever drive already holds the wanted tape.
	// All hardcoded "drive 0" in the write path reads l.drive instead.
	drive int
	// reserved is the run-shared set of volume labels a drive has selected to write this
	// run. Siblings from forDrive share the one map (it survives the shallow copy), so a
	// tape one drive is writing is excluded from another drive's selection — no two drives
	// ever pick the same cartridge. Access is serialised by the spool's single orchestrator
	// (every roll/select crosses it), so the map needs no lock.
	reserved map[string]bool

	// fill is the fill arithmetic for the volume this handle's drive last accepted
	// for writing — how Remaining() is answered, a tape being unable to see its own
	// fill. Per-drive by construction: forDrive's shallow copy gives each sibling
	// its own value. Like reserved, access is serialised (one writer per drive;
	// spool crossings order it), so no lock.
	fill volumeFill
}

// volumeFill tracks one accepted volume's fill: the catalog's stored figure
// (VolumeRecord.Used, maintained at record time with the medium's own cost rule)
// when the label protocol accepted the reel (verifyWritable → accept), plus every
// file the allocator lands on it after (countedVol → land, at the same prices).
// The snapshot-plus-count split is what closes the mid-run gap — the catalog only
// learns of files at archive commit, but rolls are decided between parts — while
// never double-counting an archive once it commits (the next accept's snapshot
// replaces the count).
type volumeFill struct {
	label  string // the accepted volume's label; guards against a stale/foreign mount
	base   int64  // the stored fill at accept (catalog VolumeRecord.Used)
	landed int64  // file costs landed on it since accept, counted by the allocator
}

// accept restarts the arithmetic for a newly accepted volume.
func (f *volumeFill) accept(label string, base int64) { *f = volumeFill{label: label, base: base} }

// land counts bytes placed on the accepted volume.
func (f *volumeFill) land(n int64) { f.landed += n }

// used reports the accepted volume's fill. ok is false when vol is not the volume
// this arithmetic was accepted for — nothing accepted yet, or the drive was
// remounted since — so the caller treats the fill as unknowable.
func (f *volumeFill) used(vol string) (int64, bool) {
	if vol == "" || vol != f.label {
		return 0, false
	}
	return f.base + f.landed, true
}

// New constructs a librarian for a medium's open volume. cat is the catalog the
// label protocol consults and records into; op handles manual swaps (nil =
// unattended); autoLabel allows labeling a blank volume during a write; minAge is the
// medium's retention minimum age, which gates whether an aged-out volume may be
// recycled on write (the retention Floor is the safety gate of the label rotation).
//
// The medium's shape is read once, here: a media.Changer is a tape library; anything
// else (disk, s3) is a single directly-addressed volume, wrapped in a directChanger
// so the rest of the librarian has one shape. A tape changer's Manual() says whether a
// human loads it (prompt the operator) or a robot does (load unattended).
func New(vol media.Volume, medium string, cat *catalog.Catalog, op Operator, autoLabel bool, minAge time.Duration) *Librarian {
	l := &Librarian{vol: vol, medium: medium, cat: cat, op: op, autoLabel: autoLabel, minAge: minAge,
		reserved: map[string]bool{}}
	if ch, ok := vol.(media.Changer); ok {
		l.changer, l.isChanger, l.manual = ch, true, ch.Manual()
	} else {
		l.changer = directChanger{vol}
	}
	return l
}

// loaded reports the cartridge in this handle's drive.
func (l *Librarian) loaded() (media.VolumeStatus, bool) { return l.changer.Drive(l.drive).Loaded() }

// fileCost prices one file against the loaded volume's declared capacity, in the
// medium's own currency (media.FileCoster — only media with finite labeled reels
// implement it). ok is false for a costless medium (disk, cloud): there is no
// reel to fill, so no fill arithmetic applies.
func (l *Librarian) fileCost(kind string, payload int64) (int64, bool) {
	fc, ok := l.driveVol().(media.FileCoster)
	if !ok {
		return 0, false
	}
	return fc.FileCost(kind, payload), true
}

// driveVol is the volume in this handle's drive — what the write path reads a label from,
// verifies, and rolls. For the base handle (drive 0) it is the same cartridge l.vol proxies;
// forDrive(i) points it at drive i so two drives write independent tapes.
func (l *Librarian) driveVol() media.Volume { return l.changer.Drive(l.drive) }

// Drives reports how many data-transfer elements the medium's changer has — the write
// parallelism width for a serial medium (one archive writer per drive). It is 1 for a
// single-drive changer, a directly-addressed medium, or on any inventory error.
func (l *Librarian) Drives() int {
	ds, err := l.changer.Drives()
	if err != nil || len(ds) == 0 {
		return 1
	}
	return len(ds)
}

// Parallel reports whether this medium can host several archive writers at once on
// distinct drives: a robotic (non-manual) changer with more than one drive. A manual
// single drive and a directly-addressed medium are not (a directly-addressed medium is
// concurrent by other means — independent files — handled above the librarian).
func (l *Librarian) Parallel() bool { return l.isChanger && !l.manual && l.Drives() > 1 }

// forDrive returns a sibling librarian bound to drive i for concurrent writing: a shallow
// copy sharing the changer, catalog, and reservation set, differing only in which drive
// its write path loads and reads. The base handle starts at drive 0 for reads and labels
// (a read mount may rebind it to whichever drive holds the wanted volume).
func (l *Librarian) forDrive(i int) *Librarian {
	c := *l
	c.drive = i
	return &c
}

// reserve marks a volume label as claimed by a drive for this run, so another drive's
// selection (oldestReusable, the advance scan) skips it. Shared across forDrive siblings.
func (l *Librarian) reserve(name string) {
	if name != "" {
		l.reserved[name] = true
	}
}

// loadHint phrases "get a volume into the drive" for this medium's shape, for
// error guidance: a robotic library — and the file-backed manual sim, whose
// addressable slots make `nb load` the operator's hands — loads by slot or
// label; a REAL hand-fed drive has no slots software can address, so telling its
// operator to `nb load <slot>` is an instruction they cannot run — they insert
// the tape physically. label names a specific wanted volume; "" means any.
func (l *Librarian) loadHint(label string) string {
	if l.manual {
		if slots, _ := l.changer.Slots(); len(slots) == 0 { // a real drive: no addressable slots
			if label == "" {
				return "insert a tape into the drive"
			}
			return fmt.Sprintf("insert the tape labeled %q into the drive", label)
		}
	}
	if label == "" {
		return fmt.Sprintf("load one with `nb load %s <slot>`", l.medium)
	}
	return fmt.Sprintf("load it with `nb load --label %s %s`", l.medium, label)
}

// directChanger adapts a directly-addressed Volume (disk, s3) to media.Changer: one
// drive permanently loaded with the one volume, no slots, no robot. Load/Unload are
// no-ops (there is nothing to move), so the librarian's changer paths collapse to the
// single-volume case without a special branch. It is librarian-internal: the media
// layer still sees the bare Volume (WalkReadable visits it directly).
type directChanger struct{ media.Volume }

func (d directChanger) Slots() ([]media.SlotStatus, error) { return nil, nil }
func (d directChanger) Drives() ([]media.DriveStatus, error) {
	return []media.DriveStatus{{Drive: 0, Loaded: true, FromSlot: -1}}, nil
}
func (d directChanger) Drive(int) media.Drive { return directDrive{d.Volume} }
func (d directChanger) Load(int, int) error   { return nil }
func (d directChanger) Unload(int) error      { return nil }
func (d directChanger) Manual() bool          { return false }

// directDrive presents a directly-addressed Volume as an always-loaded media.Drive.
type directDrive struct{ media.Volume }

func (d directDrive) Loaded() (media.VolumeStatus, bool) { return media.VolumeStatus{}, true }

// Volume returns the underlying volume handle.
func (l *Librarian) Volume() media.Volume { return l.vol }

// Labeled reports whether the medium identifies itself with on-volume labels
// (tape) rather than by address (disk, s3). It lets callers decide whether a label
// protocol applies without type-asserting the Volume themselves.
func (l *Librarian) Labeled() bool { _, ok := l.vol.(media.Labeled); return ok }

// AppendOnly reports whether the medium is immutable by construction: a labeled
// (tape) medium only ever appends — a written file cannot be rewritten or
// individually deleted. The drill's WORM probe uses this to report such a medium as
// immutable without writing a probe, so the engine asks through the librarian rather
// than type-asserting the Volume's shape itself.
//
// Today this is exactly Labeled() — labeled media are precisely the append-only ones —
// but the two names answer different questions (does a label protocol apply? vs is the
// medium immutable?), so callers state their intent and the equivalence lives here only.
func (l *Librarian) AppendOnly() bool { return l.Labeled() }

// ReadFileAt mounts the volume holding a part (verifying its identity) and reads the
// rng slice of the file at the given position (media.Range{} = the whole file; a
// volume that cannot serve a genuine sub-range — tape — answers
// media.ErrRangeUnsupported, the caller's cue to fall back to the whole stream). It
// keeps the mount-then-read sequence behind the librarian seam so callers never hold
// the media.Volume to seek it directly.
func (l *Librarian) ReadFileAt(volume string, epoch, pos int, rng media.Range) (record.Header, io.ReadCloser, error) {
	if err := l.MountForRead(volume, epoch); err != nil {
		return record.Header{}, nil, err
	}
	return l.driveVol().ReadFile(pos, rng) // MountForRead set l.drive to whichever drive holds it
}

// eachLoadableSlot is the changer's occupied-slot scan, shared by every path that must
// discover what the library holds by loading cartridges (the changer reports only
// barcodes; a label is truly read only once its cartridge is in a drive): the advance
// scan, findSlot, the operator's room listing, and chooseSlot. It visits every full,
// non-import/export slot — skipping any the optional skip predicate rejects before the
// robot moves — loads it into the given drive, reads its label, and hands the outcome
// to visit; each caller keeps only its policy. A failed Load is reported as loadErr
// (name/labeled/readErr are then meaningless). visit returns true to stop the scan,
// leaving the last-visited cartridge in the drive.
func (l *Librarian) eachLoadableSlot(drive int, skip func(media.SlotStatus) bool,
	visit func(s media.SlotStatus, loadErr error, name string, labeled bool, readErr error) (stop bool)) error {
	slots, err := l.changer.Slots()
	if err != nil {
		return err
	}
	for _, s := range slots {
		if !s.Full || s.ImportExport {
			continue
		}
		if skip != nil && skip(s) {
			continue
		}
		if err := l.changer.Load(s.Slot, drive); err != nil {
			if visit(s, err, "", false, nil) {
				return nil
			}
			continue
		}
		var name string
		var labeled bool
		var readErr error
		if lv, ok := l.changer.Drive(drive).(media.Labeled); ok {
			var lbl record.Label
			lbl, labeled, readErr = lv.ReadLabel()
			name = lbl.Name
		}
		if visit(s, nil, name, labeled, readErr) {
			return nil
		}
	}
	return nil
}
