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
	"sort"
	"strconv"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/retention"
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
var ErrAllVolumesProtected = errors.New("all volumes still within retention")

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
	// (drive 0) does reads, labels, and single-drive writes; forDrive(i) returns a sibling
	// bound to drive i for concurrent multi-drive writing. All hardcoded "drive 0" in the
	// write path reads l.drive instead.
	drive int
	// reserved is the run-shared set of volume labels a drive has selected to write this
	// run. Siblings from forDrive share the one map (it survives the shallow copy), so a
	// tape one drive is writing is excluded from another drive's selection — no two drives
	// ever pick the same cartridge. Access is serialised by the spool's single orchestrator
	// (every roll/select crosses it), so the map needs no lock.
	reserved map[string]bool
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
// its write path loads and reads. The base handle stays drive 0 for reads and labels.
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
func (l *Librarian) AppendOnly() bool { return l.Labeled() }

// ReadFileAt mounts the volume holding a part (verifying its identity) and reads
// the file at the given position. It keeps the mount-then-read sequence behind the
// librarian seam so callers never hold the media.Volume to seek it directly.
func (l *Librarian) ReadFileAt(volume string, epoch, pos int) (record.Header, io.ReadCloser, error) {
	if err := l.MountForRead(volume, epoch); err != nil {
		return record.Header{}, nil, err
	}
	return l.driveVol().ReadFile(pos) // MountForRead set l.drive to whichever drive holds it
}

// PrepareWrite enforces the label protocol on the loaded volume before writing,
// prompting a swap when the medium is a single drive whose loaded reel won't do.
// Robotic libraries and unattended runs fall straight through to the underlying
// error. It returns the accepted volume's identity to record in a placement.
func (l *Librarian) PrepareWrite(appendable bool, expect string, now time.Time, logf Logf) (string, int, error) {
	name, epoch, err := l.verifyWritable(appendable, now)
	if err == nil {
		l.reserve(name) // the loaded tape is writable; claim it against concurrent recycling
		return name, epoch, nil
	}
	if !isReloadable(err) {
		return "", 0, err
	}
	// The loaded volume won't do, but loading another might. A robotic library rolls
	// to its next writable bay on its own (auto-labeling a blank if enabled) — the
	// same selection Advance does mid-span, applied here so a run can also *start* on
	// a blank/empty bay (e.g. nothing loaded yet, or the loaded reel already holds a
	// run on a one-run-per-tape medium) rather than failing with "no tape loaded".
	if l.isChanger && !l.manual {
		name, epoch, _, aerr := l.Advance(appendable, map[string]bool{}, expect, now, logf)
		if aerr != nil {
			// A full pool of still-protected tapes is the rotation's fail-loud verdict —
			// more actionable than the loaded volume's bare "won't do" reason, so surface
			// it. Any other advance failure (out of bays) keeps the original reason, which
			// is more specific for a blank/foreign loaded tape ("label it first").
			if errors.Is(aerr, ErrAllVolumesProtected) {
				return "", 0, aerr
			}
			return "", 0, err
		}
		return name, epoch, nil
	}
	// A manual (hand-loaded) drive prompts the operator to load a writable cartridge.
	// When the loaded cartridge turns out to be an aged-out in-pool tape (the rotation's
	// oldest reusable volume), it is recycled in place rather than refused — the same
	// swap loop the spanning roll uses.
	if l.manual {
		tried := map[string]bool{}
		for {
			if expect == "" {
				if rec, ok := l.oldestReusable(tried, now); ok {
					expect = rec.Label.Name
				}
			}
			switch reel, out, serr := l.requestSwap("", expect, err, logf); {
			case serr != nil:
				return "", 0, serr
			case out == swapUnattended:
				return "", 0, fmt.Errorf("%v (load a writable volume into the drive and retry)", err)
			case out == swapAborted:
				return "", 0, fmt.Errorf("%v (no volume loaded)", err)
			default:
				tried[reel] = true
			}
			name, epoch, _, rerr := l.acceptOrRecycle(appendable, tried, now, logf)
			if rerr == nil {
				l.reserve(name)
				return name, epoch, nil
			}
			if !isReloadable(rerr) {
				return "", 0, rerr
			}
			err = rerr // surface the latest "why the loaded reel won't do" if we give up
		}
	}
	return "", 0, err
}

// resolveLabel reads the loaded labeled volume's label, auto-labeling a blank one when
// allowed, and returns the resolved label. It isolates the read-and-maybe-write step
// (the cases a swap can fix become a reloadable error: no volume, foreign data, blank
// without auto-label) from the pool/epoch/appendable policy that verifyWritable layers
// on top.
func (l *Librarian) resolveLabel(lv media.Labeled, now time.Time) (record.Label, error) {
	lbl, labeled, err := lv.ReadLabel()
	switch {
	case errors.Is(err, media.ErrNoVolume):
		return record.Label{}, reloadableErr("medium %q has no volume loaded; load one with `nb load %s <slot>` or label a blank one with `nb label %s <name>`", l.medium, l.medium, l.medium)
	case errors.Is(err, media.ErrForeignVolume):
		return record.Label{}, reloadableErr("medium %q holds non-NBackup data; refusing to overwrite — relabel it explicitly with `nb label --force %s <name>`", l.medium, l.medium)
	case err != nil:
		// Unparseable/corrupt header (e.g. "parse file header: invalid character …"):
		// the volume is not a recognizable NBackup tape, so treat it like foreign data —
		// a clear refusal a swap can resolve, not the raw decoder error.
		return record.Label{}, reloadableErr("medium %q holds unrecognized or corrupt data (%v); refusing to overwrite — relabel it explicitly with `nb label --force %s <name>`", l.medium, err, l.medium)
	case !labeled: // blank volume
		if !l.autoLabel {
			return record.Label{}, reloadable{fmt.Errorf("medium %q has a blank/unlabeled reel loaded: %w", l.medium, errBlankNeedsLabel)}
		}
		lbl = record.Label{Name: l.autoLabelName(now), Pool: l.medium, Epoch: 1, WrittenAt: now}
		if err := lv.WriteLabel(lbl); err != nil {
			return record.Label{}, err
		}
	}
	return lbl, nil
}

// verifyWritable enforces the label protocol before writing to a medium. Address-
// identified media (disk, s3) are trusted by their path/bucket and return that name
// with epoch 0. For labeled (tape) media it refuses a foreign, blank (unless
// autoLabel), wrong-pool, wrong, or relabeled-since volume — the overwrite and
// wrong-tape protection — records the accepted label, and returns the volume
// identity to record in a placement.
func (l *Librarian) verifyWritable(appendable bool, now time.Time) (volName string, epoch int, err error) {
	lv, ok := l.driveVol().(media.Labeled)
	if !ok {
		return "", 0, nil // address-identified: no label — the medium is its own volume
	}
	lbl, err := l.resolveLabel(lv, now)
	if err != nil {
		return "", 0, err
	}
	if lbl.Pool != "" && lbl.Pool != l.medium {
		return "", 0, reloadableErr("mounted volume %q belongs to pool %q, not %q — wrong volume", lbl.Name, lbl.Pool, l.medium)
	}
	// Relabeled-since check: a tape we know whose epoch advanced means the catalog
	// is stale for it. (A genuinely different tape is not an error — that is what
	// loading another tape in the pool is for.)
	if known, ok := l.cat.Volume(lbl.Name); ok && known.Label.Epoch != lbl.Epoch {
		return "", 0, fmt.Errorf("volume %q was relabeled since the catalog was updated (epoch %d mounted vs %d cached); run `nb rebuild`", lbl.Name, lbl.Epoch, known.Label.Epoch)
	}
	// One-run-per-tape media refuse to append onto a tape that already holds a run.
	if !appendable {
		if held := l.cat.RunsOnLabel(lbl.Name); len(held) > 0 {
			return "", 0, reloadableErr("medium %q is not appendable and volume %q already holds %d run(s); load a fresh volume", l.medium, lbl.Name, len(held))
		}
	}
	if err := l.cat.RecordVolume(lbl); err != nil {
		return "", 0, err
	}
	l.learnLoadedBarcode(lbl.Name)
	return lbl.Name, lbl.Epoch, nil
}

// autoLabelName picks a unique auto-label for a blank volume: medium-date, or
// medium-date-N when an earlier name is taken — so a single run that rolls across
// several blank tapes (a filling library) does not stamp every fresh tape with the
// same name (which would collide in the catalog, keyed by label name).
func (l *Librarian) autoLabelName(now time.Time) string {
	base := fmt.Sprintf("%s-%s", l.medium, record.DateString(now))
	name := base
	for n := 2; ; n++ {
		if _, ok := l.cat.Volume(name); !ok {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
}

// Advance rolls a medium to its next writable volume after the loaded one filled (or
// cannot hold the next archive), so a multi-volume copy/sync keeps going. It first
// tries any mountable bay the run has not yet attempted — a robotic library's other
// bays — then, on a single drive with a room, prompts the operator to load another
// reel. A plain volume with no changer returns an actionable error. `tried`
// accumulates the volumes already attempted so the loop terminates; `wasEmpty`
// reports whether the new volume started with no runs (so the caller can tell "archive
// too big for any volume" from "the previous volume was nearly full").
func (l *Librarian) Advance(appendable bool, tried map[string]bool, expect string, now time.Time, logf Logf) (volName string, epoch int, wasEmpty bool, err error) {
	switch {
	case !l.isChanger:
		return "", 0, false, fmt.Errorf("medium %q is a single volume with no changer; it cannot span volumes", l.medium)
	case l.manual:
		// A hand-loaded drive prompts the operator to load another cartridge.
		return l.advanceViaShelf(appendable, tried, expect, now, logf)
	default:
		// A robot rolls itself onto its next writable slot.
		return l.advanceViaLibrary(appendable, tried, now, logf)
	}
}

// advanceViaLibrary rolls a robotic library onto its next writable bay after the
// loaded one filled: it marks the filled bay tried, then mounts each not-yet-tried
// bay until one verifies writable (skipping wrong-pool / occupied bays), or reports
// that no further bay can be written. Labeled volumes are preferred over blank reels
// — a roll spends the pool's existing (labeled, writable) capacity before consuming
// a fresh cartridge, so blanks are deferred to a second pass (where auto_label may
// stamp one; with auto_label off each is refused, never written).
func (l *Librarian) advanceViaLibrary(appendable bool, tried map[string]bool, now time.Time, logf Logf) (string, int, bool, error) {
	// The cartridge that just filled must not be rolled back to; mark it tried by label.
	if st, ok := l.loaded(); ok && st.Label != "" {
		tried[st.Label] = true
	}
	slots, err := l.changer.Slots()
	if err != nil {
		return "", 0, false, err
	}
	var lastErr error
	// accept verifies the cartridge just loaded in the drive and claims it for this
	// run. The label check runs BEFORE any data could touch the reel — a refused
	// cartridge (wrong pool, blank without auto_label) is never written to.
	accept := func(slot int) (string, int, bool, bool) {
		name, epoch, verr := l.verifyWritable(appendable, now)
		if verr != nil {
			lastErr = verr // wrong pool / holds runs / blank with autoLabel off: try the next slot
			return "", 0, false, false
		}
		if tried[name] || l.reserved[name] {
			return "", 0, false, false // already used this run, or another drive is writing it right now
		}
		tried[name] = true // never re-select this volume by name during a multi-volume write
		l.reserve(name)    // claim it so a concurrent drive's selection skips it
		empty := len(l.cat.RunsOnLabel(name)) == 0
		logf.log("medium %q: rolled to slot %d (volume %q)", l.medium, slot, name)
		return name, epoch, empty, true
	}
	var blanks []int
	for _, s := range slots {
		if !s.Full || s.ImportExport {
			continue
		}
		key := "slot:" + strconv.Itoa(s.Slot)
		if tried[key] {
			continue
		}
		if err := l.changer.Load(s.Slot, l.drive); err != nil {
			tried[key] = true
			lastErr = err // a cartridge this drive can't load (wrong generation, dud): try the next slot
			continue
		}
		if _, labeled, lerr := l.readVolumeLabel(); !labeled && lerr == nil {
			blanks = append(blanks, s.Slot) // a fresh reel: wanted only if no labeled volume will do
			continue
		}
		tried[key] = true
		if name, epoch, empty, ok := accept(s.Slot); ok {
			return name, epoch, empty, nil
		}
	}
	// No labeled bay verified writable: fall back to the blank reels seen above.
	for _, slot := range blanks {
		tried["slot:"+strconv.Itoa(slot)] = true
		if err := l.changer.Load(slot, l.drive); err != nil {
			lastErr = err
			continue
		}
		if name, epoch, empty, ok := accept(slot); ok { // verifyWritable auto-labels when allowed
			return name, epoch, empty, nil
		}
	}
	// No blank or empty in-pool bay left. Rather than refuse, recycle the oldest tape
	// whose every run the retention Floor leaves unprotected — the label rotation. The
	// Floor is the safety gate (a tape holding any kept archive is never reusable); a
	// recycle keeps the same label name, advancing only the epoch (and physically wiping
	// the tape via WriteLabel's reset).
	if rec, ok := l.oldestReusable(tried, now); ok {
		l.reserve(rec.Label.Name) // claim before the robot move so a concurrent drive skips it
		if err := l.recycleViaLibrary(rec, now, logf); err != nil {
			return "", 0, false, err
		}
		tried[rec.Label.Name] = true
		name, epoch, verr := l.verifyWritable(appendable, now)
		if verr != nil {
			return "", 0, false, verr
		}
		return name, epoch, true, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all %d slots are already loaded or tried", len(slots))
	}
	return "", 0, false, l.noReusableErr(tried, now, lastErr)
}

// advanceViaShelf prompts the operator to load another reel into a single-drive
// station's drive after the loaded one filled, looping until a writable reel is in
// the drive or the operator aborts. Unattended (no operator) it returns an
// actionable error rather than blocking.
func (l *Librarian) advanceViaShelf(appendable bool, tried map[string]bool, expect string, now time.Time, logf Logf) (string, int, bool, error) {
	for {
		// Suggest the oldest reusable tape so the operator is told which reel to load
		// (Amanda's "load tape X"); a blank reel is equally accepted.
		if expect == "" {
			if rec, ok := l.oldestReusable(tried, now); ok {
				expect = rec.Label.Name
			}
		}
		reel, out, err := l.requestSwap("", expect, fmt.Errorf("volume full; another volume is needed"), logf)
		switch {
		case err != nil:
			return "", 0, false, err
		case out == swapUnattended:
			return "", 0, false, fmt.Errorf("medium %q drive is full; label a blank volume and load it, then re-run", l.medium)
		case out == swapAborted:
			return "", 0, false, fmt.Errorf("medium %q drive is full and no further volume was loaded", l.medium)
		}
		if tried[reel] {
			return "", 0, false, fmt.Errorf("medium %q: volume %q was already used and is full", l.medium, reel)
		}
		tried[reel] = true
		name, epoch, empty, verr := l.acceptOrRecycle(appendable, tried, now, logf)
		if verr == nil {
			tried[name] = true // also track by label name (oldestReusable keys on names)
			return name, epoch, empty, nil
		}
		if errors.Is(verr, errBlankNeedsLabel) {
			// auto_label is off and the operator loaded an unlabeled reel. Spanning needs
			// a fresh writable volume, but NBackup may not label a blank without auto_label,
			// and re-prompting only offers more blanks it must reject too — eventually
			// looping back to a reel already used and full. Fail fast with the actionable
			// reason instead of looping; pre-label the reels or set auto_label: true.
			return "", 0, false, verr
		}
		if !isReloadable(verr) {
			return "", 0, false, verr
		}
		expect = "" // the loaded reel won't do; recompute the suggestion for the next prompt
		// reloadable (blank without autoLabel, wrong pool, still-protected …): prompt again
	}
}

// oldestReusable returns the catalog record of the oldest in-pool volume the rotation
// may recycle: the one written longest ago whose every archive the retention Floor
// leaves unprotected, skipping any already used this run (by label name). It is the
// execution-time peer of the engine's volume expectation, applying the identical rule —
// retention.Compute over this medium's own archives (so a copy elsewhere never makes a
// volume reusable), pool ordered oldest-WrittenAt first — so the tape a run actually
// recycles is the one `nb plan` announced it would. ok is false when every volume is
// still protected (or already used): the caller then needs a blank, or fails loud.
func (l *Librarian) oldestReusable(tried map[string]bool, now time.Time) (catalog.VolumeRecord, bool) {
	var pool []catalog.VolumeRecord
	for _, v := range l.cat.Volumes() {
		if v.Label.Pool == l.medium && !tried[v.Label.Name] && !l.reserved[v.Label.Name] {
			pool = append(pool, v) // skip a tape another drive is already writing this run
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].Label.WrittenAt.Before(pool[j].Label.WrittenAt) })
	floor := retention.Compute(l.cat.ArchivesOn(l.medium), l.minAge, now)
	for _, v := range pool {
		if _, _, kept := floor.First(l.cat.RunIDsOnLabel(v.Label.Name)); kept {
			continue // some archive on this tape is still within retention — not reusable
		}
		return v, true
	}
	return catalog.VolumeRecord{}, false
}

// acceptOrRecycle verifies the loaded volume is writable and, if it is rejected only
// because it is an aged-out in-pool tape that already holds runs (the one-run-per-tape
// case), recycles it in place when the retention Floor clears its every archive. It is
// the per-volume accept used by the single-drive station, where the operator chooses
// which reel to load: a blank reel is auto-labeled and accepted as before; an aged-out
// reel the rotation may reuse is recycled rather than refused. Any other rejection
// (wrong pool, blank without auto_label, still-protected) is returned unchanged so the
// caller prompts for another reel. tried lists volumes already written this run, which
// must never be recycled (their fresh content is not yet in the catalog).
func (l *Librarian) acceptOrRecycle(appendable bool, tried map[string]bool, now time.Time, logf Logf) (string, int, bool, error) {
	name, epoch, verr := l.verifyWritable(appendable, now)
	if verr == nil {
		empty := len(l.cat.RunsOnLabel(name)) == 0
		return name, epoch, empty, nil
	}
	if !isReloadable(verr) {
		return "", 0, false, verr
	}
	lbl, labeled, lerr := l.readLoadedLabel()
	if lerr != nil || !labeled || lbl.Pool != l.medium || tried[lbl.Name] {
		return "", 0, false, verr // not an in-pool tape we may recycle this run
	}
	floor := retention.Compute(l.cat.ArchivesOn(l.medium), l.minAge, now)
	if _, _, kept := floor.First(l.cat.RunIDsOnLabel(lbl.Name)); kept {
		return "", 0, false, verr // still within retention — not reusable
	}
	if err := l.recycle(lbl, now, logf); err != nil {
		return "", 0, false, err
	}
	name, epoch, verr = l.verifyWritable(appendable, now)
	if verr != nil {
		return "", 0, false, verr
	}
	return name, epoch, true, nil
}

// recycleViaLibrary mounts the bay holding the reusable volume rec and recycles it in
// place — the robotic-library half of the rotation, where the software (not an operator)
// loads the aged-out tape. The caller has already confirmed rec is Floor-cleared.
func (l *Librarian) recycleViaLibrary(rec catalog.VolumeRecord, now time.Time, logf Logf) error {
	slot, drive, ok, err := l.findSlot(rec.Label.Name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("medium %q: volume %q (the oldest reusable tape) is not in the library; load it with `nb load %s <slot>` or relabel a blank one", l.medium, rec.Label.Name, l.medium)
	}
	if drive != l.drive {
		// A prior run left this reusable tape in another drive (a tape this run is writing is
		// reserved, so oldestReusable never returns one). Move it into this write drive to
		// recycle it here. Selection is orchestrator-serialised, so the other drive is idle.
		if err := l.changer.Unload(drive); err != nil {
			return err
		}
		if err := l.changer.Load(slot, l.drive); err != nil {
			return err
		}
	}
	return l.recycle(rec.Label, now, logf) // now loaded in this write drive
}

// findSlot locates the cartridge labeled name and reports the slot it came from and the
// drive it is now in — already loaded in some drive (a multi-drive run leaves tapes in
// their drives), or loaded into this handle's drive by scanning each occupied slot and
// reading its label. The drive it returns is where a reader must read from; a real library
// would map barcode→label from the catalog to skip the scan. ok is false when no cartridge
// holds that label.
func (l *Librarian) findSlot(name string) (slot, drive int, ok bool, err error) {
	// The wanted cartridge may already be in a drive — its slot then reports empty, so the
	// scan below would miss it. Check the drives' loaded labels first, and read from wherever
	// it is (drive 0 for a single-drive medium; any drive after a multi-drive dump).
	drives, err := l.changer.Drives()
	if err != nil {
		return 0, 0, false, err
	}
	for _, d := range drives {
		if d.Loaded && d.Volume.Label == name {
			l.learnBarcode(name, d.Volume.Barcode)
			return d.FromSlot, d.Drive, true, nil
		}
	}
	slots, err := l.changer.Slots()
	if err != nil {
		return 0, 0, false, err
	}
	for _, s := range slots {
		if !s.Full || s.ImportExport {
			continue
		}
		if err := l.changer.Load(s.Slot, l.drive); err != nil {
			continue // a cartridge this drive can't load holds no label we can read
		}
		if n, labeled, _ := l.readVolumeLabel(); labeled {
			l.learnBarcode(n, s.Barcode) // remember every label the scan read, not just the hit
			if n == name {
				return s.Slot, l.drive, true, nil
			}
		}
	}
	return 0, 0, false, nil
}

// recycle rewrites the loaded volume's label in place for reuse: same name and pool,
// epoch+1, fresh WrittenAt. WriteLabel resets the volume first, so the aged-out tape is
// physically wiped before its identity is re-stamped — a reuse, not a rename. It then
// reconciles the catalog (drop the now-dead prior-epoch placements; a run that loses
// its last copy leaves the catalog), reusing the same path `nb label --relabel` does.
// The caller owns the safety gate (the tape must be Floor-cleared) and having it loaded.
func (l *Librarian) recycle(prev record.Label, now time.Time, logf Logf) error {
	lv, ok := l.driveVol().(media.Labeled)
	if !ok {
		return fmt.Errorf("medium %q is address-identified and cannot recycle volumes", l.medium)
	}
	recycled := len(l.cat.RunsOnLabel(prev.Name))
	next := record.Label{Name: prev.Name, Pool: l.medium, Epoch: prev.Epoch + 1, WrittenAt: now}
	if err := lv.WriteLabel(next); err != nil {
		return err
	}
	logf.log("medium %q: recycling volume %q (epoch %d -> %d, %d aged-out run(s) past retention)", l.medium, prev.Name, prev.Epoch, next.Epoch, recycled)
	return l.reconcileRelabel(prev.Name, next)
}

// learnBarcode records which cartridge (barcode) a volume's label was just read
// from — the catalog memory behind slot-inventory display. Best-effort cache
// upkeep: failures are ignored (the pairing is re-learned at the next read).
func (l *Librarian) learnBarcode(name, barcode string) {
	if name != "" && barcode != "" {
		_ = l.cat.SetVolumeBarcode(name, barcode)
	}
}

// learnLoadedBarcode learns the pairing for the cartridge this handle's drive holds.
func (l *Librarian) learnLoadedBarcode(name string) {
	if !l.isChanger {
		return
	}
	if st, ok := l.loaded(); ok {
		l.learnBarcode(name, st.Barcode)
	}
}

// readLoadedLabel reads the loaded volume's full label (name, pool, epoch), or
// labeled=false for a blank or address-identified medium.
func (l *Librarian) readLoadedLabel() (record.Label, bool, error) {
	lv, ok := l.driveVol().(media.Labeled)
	if !ok {
		return record.Label{}, false, nil
	}
	return lv.ReadLabel()
}

// noReusableErr crafts the fail-loud refusal when no blank bay is left and no volume can
// be recycled — never an overwrite (recoverability outranks capacity). It separates two
// causes: the rotation is *full* (every in-pool volume is still within retention), which
// names the soonest a volume ages out so the operator knows when the rotation frees a
// tape; or the medium is simply *out of bays/volumes* (the pre-existing failure), which
// keeps the original "no further writable bay" wording. tried excludes volumes already
// written this run (their fresh content is not yet in the catalog).
func (l *Librarian) noReusableErr(tried map[string]bool, now time.Time, lastErr error) error {
	floor := retention.Compute(l.cat.ArchivesOn(l.medium), l.minAge, now)
	protected := false
	for _, v := range l.cat.Volumes() {
		if v.Label.Pool != l.medium || tried[v.Label.Name] {
			continue
		}
		if _, _, kept := floor.First(l.cat.RunIDsOnLabel(v.Label.Name)); kept {
			protected = true
			break
		}
	}
	if !protected {
		return fmt.Errorf("medium %q has no further writable bay (load or relabel more volumes): %w", l.medium, lastErr)
	}
	msg := fmt.Sprintf("medium %q: no writable volume — every volume in the pool still holds runs within retention", l.medium)
	if l.minAge > 0 {
		var soonest time.Time
		for _, s := range l.cat.RunsOn(l.medium) {
			d, err := record.ParseDateField(s.Date())
			if err != nil {
				continue
			}
			if out := d.Add(l.minAge); out.After(now) && (soonest.IsZero() || out.Before(soonest)) {
				soonest = out
			}
		}
		if !soonest.IsZero() {
			msg += fmt.Sprintf("; the oldest ages out on %s", record.DateString(soonest))
		}
	}
	return fmt.Errorf("%s — load a blank volume, add volumes to the pool, or recycle the oldest now with `nb label --relabel %s <name>`: %w", msg, l.medium, ErrAllVolumesProtected)
}

// Remaining reports the writable bytes left on the volume currently in the drive,
// when that is knowable: a finite bay/reel. ok is false for an unbounded volume, a
// medium whose remaining capacity software cannot see (a real drive reports EOT only
// by hitting it), or a non-changer medium — the caller then relies on the reactive
// media.ErrVolumeFull path instead of pre-checking.
func (l *Librarian) Remaining() (int64, bool) {
	st, ok := l.loaded()
	if !ok || st.Capacity <= 0 {
		return 0, false
	}
	if st.Used >= st.Capacity {
		return 0, true
	}
	return st.Capacity - st.Used, true
}

// WriteSink drives a multi-part, possibly multi-volume write for the archiveio writer:
// it sizes each part to the loaded volume's remaining capacity (capped by part_size),
// rolls onto a fresh volume when the loaded one fills, and places the seal record.
//
// A sink is bound to its librarian handle's drive (l.drive), so LazyDriveSinks vends one
// per drive for concurrent writing. It starts either eagerly — over the volume PrepareWrite
// already accepted (the serial path) — or lazily: started=false, so its first NextPart runs
// PrepareWrite itself, loading a writable tape into its drive. The lazy start is what lets
// the initial per-drive load cross the spool's orchestrator on the same path as a roll,
// keeping the robot single-writer without a separate mount step.
type WriteSink struct {
	l          *Librarian
	appendable bool
	partSize   int64
	now        time.Time
	logf       Logf
	tried      map[string]bool
	volume     string
	epoch      int
	started    bool   // the first volume has been accepted (PrepareWrite has run)
	expect     string // the label the run expects to (re)use, for a lazy first load
}

// WriteSink builds an eager sink starting on the volume PrepareWrite accepted (its label
// and epoch). partSize (0 = none) caps each part for media whose remaining capacity is
// unknowable or to bound part size deliberately.
func (l *Librarian) WriteSink(volume string, epoch int, appendable bool, partSize int64, now time.Time, logf Logf) *WriteSink {
	s := &WriteSink{l: l, appendable: appendable, partSize: partSize, now: now, logf: logf,
		tried: map[string]bool{}, volume: volume, epoch: epoch, started: true}
	s.seed(volume)
	return s
}

// LazyDriveSinks vends one lazy WriteSink per drive, each bound to its own drive and
// sharing the run reservation set — the concurrent multi-drive write source. len == Drives().
// Each sink loads its first tape on its first NextPart, so the spool can lease a drive per
// writer and the initial loads serialise on its orchestrator like any roll.
func (l *Librarian) LazyDriveSinks(appendable bool, expect string, partSize int64, now time.Time, logf Logf) []*WriteSink {
	sinks := make([]*WriteSink, l.Drives())
	for i := range sinks {
		li := l.forDrive(i)
		sinks[i] = &WriteSink{l: li, appendable: appendable, partSize: partSize, now: now, logf: logf,
			tried: map[string]bool{}, expect: expect}
	}
	return sinks
}

// seed records the starting volume as tried and reserved so a spanning roll never recycles
// the tape this write is already on (its fresh content is not yet in the catalog) and a
// concurrent drive never selects it.
func (s *WriteSink) seed(volume string) {
	if volume != "" {
		s.tried[volume] = true
		s.l.reserve(volume)
	}
}

// ensureStarted runs the lazy first load: PrepareWrite selects and loads a writable tape
// into this sink's drive, then the starting volume is seeded. A no-op once started.
func (s *WriteSink) ensureStarted() error {
	if s.started {
		return nil
	}
	name, epoch, err := s.l.PrepareWrite(s.appendable, s.expect, s.now, s.logf)
	if err != nil {
		return err
	}
	s.volume, s.epoch, s.started = name, epoch, true
	s.seed(name)
	return nil
}

// maxPart is the payload bytes the next part may carry on the loaded volume: its
// remaining capacity minus a header, capped by part_size; -1 when unbounded.
func (s *WriteSink) maxPart() int64 {
	room, known := s.l.Remaining()
	if !known {
		if s.partSize > 0 {
			return s.partSize - record.HeaderBlock
		}
		return -1
	}
	avail := room - record.HeaderBlock
	if avail < 0 {
		avail = 0
	}
	if s.partSize > 0 {
		if cap := s.partSize - record.HeaderBlock; cap < avail {
			avail = cap
		}
	}
	return avail
}

func (s *WriteSink) advance() error {
	volName, epoch, _, err := s.l.Advance(s.appendable, s.tried, "", s.now, s.logf)
	if err != nil {
		// A failed roll can leave an unverified cartridge in the drive (the scan's last
		// candidate — possibly a blank reel). Drop back to unstarted so any further write
		// on this sink re-runs PrepareWrite's label check instead of trusting the drive:
		// writing unverified would stamp archive data onto an unlabeled reel (poisoning
		// it as foreign) while the placement claims the previous volume.
		s.started = false
		return err
	}
	s.volume, s.epoch = volName, epoch
	return nil
}

// NextPart implements archiveio.VolumeSink: it rolls onto a fresh volume if the loaded
// one cannot hold a header plus a byte, then returns the volume and the part's byte cap.
func (s *WriteSink) NextPart() (media.Volume, int64, string, int, error) {
	if err := s.ensureStarted(); err != nil {
		return nil, 0, "", 0, err
	}
	for max := s.maxPart(); max >= 0 && max < 1; max = s.maxPart() {
		if err := s.advance(); err != nil {
			return nil, 0, "", 0, err
		}
	}
	return s.l.driveVol(), s.maxPart(), s.volume, s.epoch, nil
}

// PlaceRecord implements archiveio.VolumeSink: it rolls first if the record (one whole file
// of the given payload size — an archive's index or commit footer) will not fit the loaded
// volume.
func (s *WriteSink) PlaceRecord(size int64) (media.Volume, string, int, error) {
	if err := s.ensureStarted(); err != nil {
		return nil, "", 0, err
	}
	if room, known := s.l.Remaining(); known && room-record.HeaderBlock < size {
		if err := s.advance(); err != nil {
			return nil, "", 0, err
		}
	}
	return s.l.driveVol(), s.volume, s.epoch, nil
}

// Bounded implements archiveio.VolumeSink: it reports whether a part's size is ever capped —
// by a configured part_size or by a finite volume's knowable remaining capacity — so an archive
// may land as several parts (cloud splitting, or a reel spanning). The writer marks each such
// part a slice (Header.Split). False only for an unbounded medium (disk: no part_size, no
// software-visible capacity), where every archive is a single standalone part.
func (s *WriteSink) Bounded() bool {
	if s.partSize > 0 {
		return true
	}
	_, known := s.l.Remaining()
	return known
}

// promptSwap asks the operator (via l.op) to pick a reel to load on a single-drive
// station. need is the specific volume label wanted (reads) or "" (writes); expect
// is the volume a write would prefer (the oldest reusable tape) or "".
func (l *Librarian) promptSwap(need, expect string, cause error) (string, bool) {
	room, err := l.room()
	if err != nil {
		return "", false
	}
	var loaded media.VolumeStatus
	if st, ok := l.loaded(); ok {
		loaded = st
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	return l.op.Swap(SwapRequest{Medium: l.medium, Reason: reason, Need: need, Expect: expect, Loaded: loaded, Shelf: room})
}

// room lists the cartridges available to load into the drive — every occupied slot
// not currently in a drive — each by slot id and its barcode, for the operator
// prompt. A real drive has no addressable slots, so the room is empty (the operator
// loads from their own physical shelf and the librarian re-reads the drive).
func (l *Librarian) room() ([]media.VolumeStatus, error) {
	slots, err := l.changer.Slots()
	if err != nil {
		return nil, err
	}
	var out []media.VolumeStatus
	for _, s := range slots {
		if !s.Full || s.ImportExport {
			continue
		}
		st := media.VolumeStatus{ID: strconv.Itoa(s.Slot), Barcode: s.Barcode}
		// Read the slot's label so the operator can choose by label or blankness — a
		// file-backed changer can (it loads the slot to read it); a real drive has no
		// addressable slots, so the room is empty and this never runs.
		if err := l.changer.Load(s.Slot, 0); err == nil {
			if name, labeled, lerr := l.readVolumeLabel(); labeled {
				st.Label = name
			} else if lerr == nil {
				st.Blank = true
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// insertChoice effects the operator's swap choice: on a file-backed changer it loads
// the chosen slot (the simulated hands); on a real drive (no addressable slots) it is
// a no-op — the human already inserted the cartridge and the caller re-reads it.
func (l *Librarian) insertChoice(choice string) error {
	slots, err := l.changer.Slots()
	if err != nil {
		return err
	}
	if len(slots) == 0 {
		return nil
	}
	slot, err := strconv.Atoi(choice)
	if err != nil {
		return fmt.Errorf("invalid slot %q", choice)
	}
	return l.changer.Load(slot, 0)
}

// swapOutcome is the result of one single-drive swap step.
type swapOutcome int

const (
	swapInserted   swapOutcome = iota // a reel was loaded into the drive
	swapUnattended                    // no operator is attached (l.op == nil)
	swapAborted                       // the operator declined to load a reel
)

// requestSwap runs the single-drive swap step shared by every prompt loop: with no
// operator it reports swapUnattended; otherwise it asks the operator to load a reel for
// the stated need and, on a choice, inserts it (logging the load) and returns the reel.
// It centralizes the nil-op guard, the prompt, the abort, and the Insert; callers map
// swapUnattended/swapAborted to their own actionable message and re-check their own
// writable/mounted condition after swapInserted.
func (l *Librarian) requestSwap(need, expect string, cause error, logf Logf) (reel string, out swapOutcome, err error) {
	if l.op == nil {
		return "", swapUnattended, nil
	}
	reel, ok := l.promptSwap(need, expect, cause)
	if !ok {
		return "", swapAborted, nil
	}
	logf.log("loading %s into the %q drive", reel, l.medium)
	if err := l.insertChoice(reel); err != nil {
		return "", swapInserted, err
	}
	return reel, swapInserted, nil
}

// MountForRead loads the named volume (a part's volume) and verifies its identity so
// the reader can seek it. A robotic library mounts the bay automatically; a
// single-drive station prompts the operator to swap the reel in; non-changer media
// are a no-op (a single addressable volume). Reading an archive that spans volumes
// calls this once per part, in order — a single drive holds only one tape at a time.
func (l *Librarian) MountForRead(volume string, epoch int) error {
	if err := l.mount(volume); err != nil {
		return err
	}
	return l.assertVolume(volume, epoch)
}

func (l *Librarian) mount(volume string) error {
	if !l.isChanger {
		return nil // address-identified: a single volume, nothing to mount
	}
	if l.mountedMatches(volume) {
		return nil // the right tape is already in the drive
	}
	// A hand-loaded drive prompts the operator to swap the needed cartridge in.
	if l.manual {
		return l.mountViaShelf(volume)
	}
	// A robot loads the slot holding the needed label, into whichever drive holds it. The
	// read drive is set so the read accessors (driveVol) act on that cartridge — a
	// multi-drive dump can leave the wanted tape in a drive other than 0.
	_, drive, ok, err := l.findSlot(volume)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: volume %q (holding a copy of an archive on %q) is not in the library; load it with `nb load %s <slot>`", ErrVolumeUnavailable, volume, l.medium, l.medium)
	}
	l.drive = drive
	return nil
}

// mountViaShelf loads the named reel on a single-drive station: if it is not already
// in the drive, it prompts the operator to swap it in, looping until the right tape
// is loaded or the operator aborts. Unattended, it returns an actionable error rather
// than blocking.
func (l *Librarian) mountViaShelf(volume string) error {
	for {
		if l.mountedMatches(volume) {
			return nil
		}
		switch _, out, err := l.requestSwap(volume, "", fmt.Errorf("need volume %q", volume), nil); {
		case err != nil:
			return err
		case out == swapUnattended:
			return fmt.Errorf("%w: medium %q needs volume %q in the drive (a copy of the archive is on it); load it and retry", ErrVolumeUnavailable, l.medium, volume)
		case out == swapAborted:
			return fmt.Errorf("%w: volume %q was not loaded into the %q drive", ErrVolumeUnavailable, volume, l.medium)
		}
	}
}

// assertVolume confirms the volume mounted on the medium matches the recorded
// identity (label name + epoch) of a part to read, before reading from it.
func (l *Librarian) assertVolume(volume string, epoch int) error {
	lv, ok := l.driveVol().(media.Labeled)
	if !ok {
		return nil // address-identified: identity is the medium itself
	}
	lbl, labeled, err := lv.ReadLabel()
	if err != nil {
		return err
	}
	if !labeled || lbl.Name != volume || lbl.Epoch != epoch {
		return fmt.Errorf("medium %q has volume %q (epoch %d) mounted, but an archive part is on %q (epoch %d) — mount it or run `nb rebuild`",
			l.medium, lbl.Name, lbl.Epoch, volume, epoch)
	}
	return nil
}

// readVolumeLabel reads the loaded volume's label name, if any. It is a no-op for
// address-identified media that carry no label.
func (l *Librarian) readVolumeLabel() (name string, labeled bool, err error) {
	lv, ok := l.driveVol().(media.Labeled)
	if !ok {
		return "", false, nil
	}
	lbl, ok, err := lv.ReadLabel()
	return lbl.Name, ok, err
}

// mountedMatches reports whether the volume currently in the drive carries the given
// label. A read error, an empty drive, or address-identified media all count as no
// match — the caller then mounts the right bay or prompts for a swap.
func (l *Librarian) mountedMatches(label string) bool {
	name, labeled, err := l.readVolumeLabel()
	return err == nil && labeled && name == label
}

// Label writes (or rewrites) the identity label of a medium's volume — the
// deliberate operator act that makes a tape writable. It refuses to overwrite
// foreign data, or (without --force) a tape that still holds protected archives;
// relabeling an NBackup volume requires relabel and bumps the epoch. A relabel
// wipes the tape, so it then drops the catalog placements that referenced the old
// volume — for any medium, landing or offsite — and records the new identity, so
// the catalog stops reporting a copy that no longer exists.
func (l *Librarian) Label(name string, relabel, force bool, minAge time.Duration, now time.Time, logf Logf) error {
	lv, ok := l.vol.(media.Labeled)
	if !ok {
		return fmt.Errorf("medium %q is address-identified and does not use labels", l.medium)
	}

	// A new label may never duplicate a volume the catalog already knows — two tapes
	// carrying the same name would be indistinguishable to every placement. Checked
	// here, before any slot is chosen or loaded, so a refusal leaves the drive alone.
	// (A relabel is checked after the target's current label is read: restamping the
	// tape that already carries the name is the one legitimate reuse.)
	if !relabel {
		if err := l.duplicateLabelErr(name, "", force); err != nil {
			return err
		}
	}

	// On a robot, pick the slot this label belongs to and load it — an existing tape
	// for relabel, a blank one for a new label. On a hand-loaded drive there is no slot
	// to choose: labeling acts on whatever cartridge the operator loaded into the drive.
	chosenSlot := -1
	if l.isChanger && !l.manual {
		slot, err := l.chooseSlot(name, relabel)
		if err != nil {
			return err
		}
		// chooseSlot leaves the chosen slot loaded in drive 0.
		chosenSlot = slot
	}

	cur, labeled, err := lv.ReadLabel()
	epoch := 1
	wiped := "" // the volume a relabel overwrites; its placements become stale
	switch {
	case errors.Is(err, media.ErrNoVolume):
		// Empty drive: there is nothing to label, and --force cannot conjure a tape.
		// Surface the real condition rather than burying it in the foreign/corrupt
		// "use --force" refusal below (which a swap, not a force, would resolve).
		return fmt.Errorf("medium %q has no volume loaded; load one first with `nb load %s <slot>`", l.medium, l.medium)
	case errors.Is(err, media.ErrForeignVolume):
		if !force {
			return fmt.Errorf("volume holds non-NBackup data; refusing to overwrite (use --force)")
		}
	case err != nil:
		// The existing label could not be parsed — a corrupt or truncated header
		// (e.g. "unexpected EOF"). --force is the documented escape hatch for
		// reclaiming foreign data, so honor it here too rather than letting the raw
		// parse error escape and leave the tape unreclaimable; without --force, say
		// so clearly. A fresh epoch is correct since no prior identity is readable.
		if !force {
			return fmt.Errorf("volume holds unrecognized or corrupt data (%v); refusing to overwrite (use --force)", err)
		}
	case labeled:
		// A readable NBackup label from a different pool is a foreign reel — the same thing
		// the dump write-path refuses (verifyWritable) and inventory flags `wrong-pool`. It
		// parses cleanly, so it slips past the non-NBackup/corrupt guards above and (belonging
		// to another pool) holds no protected archive in this medium's catalog; refuse it here so
		// `nb label --relabel` cannot silently clobber it, honoring the same --force escape hatch.
		if cur.Pool != l.medium && !force {
			return fmt.Errorf("volume %q belongs to pool %q, not %q — refusing to overwrite a foreign reel (use --force)", cur.Name, cur.Pool, l.medium)
		}
		if !relabel {
			return fmt.Errorf("volume is already labeled %q (epoch %d); use --relabel to reuse it", cur.Name, cur.Epoch)
		}
		// Reuse the prune/recycle retention test: judge protection over the
		// medium's own archives (so "a newer full exists" is medium-wide), then
		// refuse if the tape being relabeled still holds a protected run. Reading
		// the catalog rather than scanning the mounted reel correctly attributes a
		// spanned archive to every tape it touches — even the head tape, whose seal
		// record lives only on the last tape of the span.
		floor := retention.Compute(l.cat.ArchivesOn(l.medium), minAge, now)
		if id, reason, ok := floor.First(l.cat.RunIDsOnLabel(cur.Name)); ok && !force {
			return fmt.Errorf("volume %q still holds protected run %s (%s); refusing to relabel (use --force)", cur.Name, id, reason)
		}
		epoch = cur.Epoch + 1
		wiped = cur.Name
	}
	// The relabel half of the duplicate-name guard: renaming this tape (or stamping a
	// blank/foreign one under --relabel) must not collide with a volume the catalog
	// already knows. Restamping the tape that carries the name (cur.Name == name, the
	// in-place recycle) is the legitimate reuse and passes.
	if err := l.duplicateLabelErr(name, wiped, force); err != nil {
		return err
	}

	lbl := record.Label{Name: name, Pool: l.medium, Epoch: epoch, WrittenAt: now}
	if err := lv.WriteLabel(lbl); err != nil {
		return err
	}
	if got, ok, err := lv.ReadLabel(); err != nil || !ok || got.Name != name {
		return fmt.Errorf("label write could not be confirmed (read back %q, ok=%v, err=%v)", got.Name, ok, err)
	}
	// Name the bay on a robotic library: a new label grabs a blank bay and mounts it,
	// which can move the mount away from a bay the operator just loaded — say so rather
	// than switching silently. A relabel names the label it overwrote, so the operator
	// can tell at a glance whether the right tape was recycled.
	switch {
	case chosenSlot >= 0 && wiped != "":
		logf.log("relabeled %q (slot %d of %q) as %q (epoch %d) and loaded it", wiped, chosenSlot, l.medium, name, epoch)
	case chosenSlot >= 0:
		logf.log("labeled slot %d of %q as %q (epoch %d) and loaded it", chosenSlot, l.medium, name, epoch)
	case wiped != "":
		logf.log("relabeled %q of %q as %q (epoch %d)", wiped, l.medium, name, epoch)
	default:
		logf.log("labeled %q as %q (epoch %d)", l.medium, name, epoch)
	}

	return l.reconcileRelabel(wiped, lbl)
}

// duplicateLabelErr is the duplicate-name guard every label path runs before writing
// a label: it refuses to stamp name onto a tape when the catalog already records a
// volume by that name, unless that record IS the tape being restamped (current — the
// in-place relabel/recycle). Two cartridges carrying one name would make every
// placement on it ambiguous. --force is the stale-catalog escape hatch (e.g. the
// recorded tape was physically destroyed).
func (l *Librarian) duplicateLabelErr(name, current string, force bool) error {
	known, ok := l.cat.Volume(name)
	if !ok || current == name || force {
		return nil
	}
	return fmt.Errorf("a volume labeled %q already exists (pool %q, epoch %d); labeling another tape %q would create a duplicate — pick a different name, or recycle the existing tape with `nb label --relabel %s %s` (--force overrides if that volume no longer exists)",
		name, known.Label.Pool, known.Label.Epoch, name, l.medium, name)
}

// reconcileRelabel updates the catalog after a (re)label wrote lbl. If wiped names
// the volume the relabel overwrote, it drops the catalog placements that pointed at
// it so it stops reporting copies that no longer exist: a spanned archive crossing the
// wiped tape has its whole medium copy removed (its other parts are now orphaned,
// reclaimable bytes). This is targeted — unlike a full Rebuild it leaves every other
// medium and every intact tape on this one untouched, and it runs for any relabeled
// medium, not just the landing one. It then registers the volume's new identity
// (empty, fresh epoch) so the catalog reflects the relabel immediately rather than
// learning it lazily at the next write.
func (l *Librarian) reconcileRelabel(wiped string, lbl record.Label) error {
	if wiped != "" {
		for _, s := range l.cat.RunsOnLabel(wiped) {
			if _, err := l.cat.RemovePlacement(s.ID, l.medium); err != nil {
				return fmt.Errorf("drop placements on relabeled volume %q: %w", wiped, err)
			}
		}
		// Drop the overwritten identity so the old name stops counting as a live
		// volume (the same physical tape now carries the new label recorded below).
		if wiped != lbl.Name {
			if err := l.cat.RemoveVolume(wiped); err != nil {
				return fmt.Errorf("drop relabeled volume %q: %w", wiped, err)
			}
		}
	}
	if err := l.cat.RecordVolume(lbl); err != nil {
		return fmt.Errorf("record relabeled volume %q: %w", lbl.Name, err)
	}
	l.learnLoadedBarcode(lbl.Name)
	return nil
}

// chooseSlot selects which slot a label operation targets on a robot, leaving it
// loaded in drive 0. A slot explicitly loaded (`nb load <slot>`) is the target — label
// and relabel act on whatever it holds, so loading a slot then labeling it does what
// it says, and `--relabel` on a loaded tape recycles it to the new name. With nothing
// loaded, a relabel finds the tape by its current name (an in-place re-stamp) and a
// new label grabs a blank slot. Because the changer reports only barcodes, finding a
// named or blank slot means loading each occupied slot and reading its label; the scan
// also refuses to stamp a name a different slot already carries (a duplicate label).
func (l *Librarian) chooseSlot(name string, relabel bool) (int, error) {
	// A loaded slot is the explicit target for a relabel (recycle whatever it holds),
	// or for a new label only when it is blank; a loaded non-blank slot is left alone
	// and a new label takes a fresh blank slot instead.
	if drs, _ := l.changer.Drives(); len(drs) > 0 && drs[0].Loaded {
		if relabel {
			return drs[0].FromSlot, nil
		}
		if _, labeled, _ := l.readVolumeLabel(); !labeled {
			return drs[0].FromSlot, nil
		}
	}
	// The scan below borrows the drive to read each slot's label, so remember what the
	// operator had loaded (or that the drive was empty) and put it back on every path
	// that does not deliberately mount a target — a failed `nb label` must never leave
	// a different tape in the drive than the one the operator loaded (a following
	// `nb label --relabel` acts on the loaded tape, so a silent switch wipes the wrong
	// one).
	orig := -1
	if drs, _ := l.changer.Drives(); len(drs) > 0 && drs[0].Loaded {
		orig = drs[0].FromSlot
	}
	restore := func() {
		if orig >= 0 {
			l.changer.Load(orig, 0) //nolint:errcheck — best-effort restore on an error path
		} else {
			l.changer.Unload(0) //nolint:errcheck — best-effort restore on an error path
		}
	}

	// Nothing relevant loaded: scan the slots, reading each label, to find the named
	// tape (for a relabel) or a blank slot (for a new label), and to detect a duplicate
	// name already stamped on a tape in the library.
	slots, err := l.changer.Slots()
	if err != nil {
		return -1, err
	}
	named, blank := -1, -1
	for _, s := range slots {
		if !s.Full || s.ImportExport {
			continue
		}
		if err := l.changer.Load(s.Slot, 0); err != nil {
			continue // a cartridge this drive can't load is not a candidate for labeling
		}
		n, labeled, lerr := l.readVolumeLabel()
		switch {
		case labeled && n == name && named < 0:
			named = s.Slot
		case !labeled && lerr == nil && blank < 0:
			blank = s.Slot
		}
	}
	if relabel {
		if named < 0 {
			restore()
			return -1, fmt.Errorf("no tape loaded and none labeled %q; run `nb load %s <slot>` to pick the tape to recycle", name, l.medium)
		}
		if err := l.changer.Load(named, 0); err != nil {
			return -1, err
		}
		return named, nil
	}
	if named >= 0 {
		restore()
		return -1, fmt.Errorf("a tape labeled %q already exists in slot %d; use --relabel to reuse it", name, named)
	}
	if blank < 0 {
		restore()
		return -1, fmt.Errorf("no blank slot available — load a slot to recycle and relabel it with `nb label --relabel`")
	}
	if err := l.changer.Load(blank, 0); err != nil {
		return -1, err
	}
	return blank, nil
}

// View is a tape medium's physical inventory for `nb medium <name>`: its slots (each
// by barcode) and its drives (each with what is loaded). Manual reports whether a
// human loads it (so the display can say "drive" with a shelf the operator stocks)
// rather than a robot. SlotLabels maps a slot to the volume last seen on its
// cartridge — the catalog's learned barcode↔label memory, not a fresh read (a label
// is only truly read once the cartridge is in a drive). A slot absent from the map
// holds a cartridge never yet loaded, or the changer has no barcode scanner.
type View struct {
	Manual     bool
	Slots      []media.SlotStatus
	Drives     []media.DriveStatus
	SlotLabels map[int]string
}

// View inventories the medium's changer for display.
func (l *Librarian) View() (View, error) {
	if !l.isChanger {
		return View{}, fmt.Errorf("medium %q has no changer to inventory (it is addressed directly, not by loading volumes)", l.medium)
	}
	slots, err := l.changer.Slots()
	if err != nil {
		return View{}, err
	}
	drives, err := l.changer.Drives()
	if err != nil {
		return View{}, err
	}
	v := View{Manual: l.manual, Slots: slots, Drives: drives, SlotLabels: map[int]string{}}
	byBarcode := map[string]string{}
	for _, rec := range l.cat.Volumes() {
		if rec.Barcode != "" {
			byBarcode[rec.Barcode] = rec.Label.Name
		}
	}
	for _, s := range slots {
		if !s.Full || s.ImportExport || s.Barcode == "" {
			continue
		}
		if name, ok := byBarcode[s.Barcode]; ok {
			v.SlotLabels[s.Slot] = name
		}
	}
	return v, nil
}

// Load loads a cartridge into the drive on a changer medium, addressed by slot
// number, or by label when byLabel is set (the "load the volume labeled X" helper).
func (l *Librarian) Load(target string, byLabel bool, logf Logf) error {
	if !l.isChanger {
		return fmt.Errorf("medium %q has no changer to load (it is addressed directly, not by loading volumes)", l.medium)
	}
	if byLabel {
		slot, _, ok, err := l.findSlot(target)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no tape labeled %q in the %q library", target, l.medium)
		}
		logf.log("loaded %q: slot %d holds %q", l.medium, slot, target) // findSlot left it loaded
		return nil
	}
	slot, err := strconv.Atoi(target)
	if err != nil {
		return fmt.Errorf("invalid slot %q (load by slot number, or use --label)", target)
	}
	if err := l.changer.Load(slot, 0); err != nil {
		return err
	}
	name, labeled, lerr := l.readVolumeLabel()
	switch {
	case labeled:
		logf.log("loaded %q: slot %d holds %q", l.medium, slot, name)
	case lerr != nil:
		// Any read error — a foreign label or unparseable/corrupt data — means the
		// slot is NOT blank; match the inventory's "foreign" verdict so an operator is
		// never told an occupied cartridge is empty.
		logf.log("loaded %q: slot %d (foreign — non-NBackup or unreadable data; `nb label --relabel --force` to overwrite)", l.medium, slot)
	default:
		logf.log("loaded %q: slot %d (blank)", l.medium, slot)
	}
	return nil
}
