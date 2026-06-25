// Package librarian operates a medium's changer for the engine: it turns operator-
// level intents — make a volume writable, mount the one holding a slot, advance to
// the next writable volume, (re)label, load, inventory — into the right positioning
// calls, and runs the label protocol (pool/epoch/appendable/auto-label) on top.
//
// It is the single place that knows a medium's physical shape, so the rest of the
// engine stays shape-agnostic. The shape is one type assertion: a media.Changer is a
// robotic library (it mounts its own bays); anything that is not a Changer is a
// single-drive station or a plain volume. A media.Shelf (the operator-managed room)
// is consulted only to actually do a swap — prompt over the room, then Insert the
// operator's choice — never as a general shape marker. media.Drive (what's loaded)
// is the device read both changer shapes share. So a robotic library advances by
// mounting its next bay; a single drive asks the operator over its Shelf.
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

// ErrVolumeUnavailable marks a read mount that could not place the needed volume in the
// drive: a robotic library that does not hold it, or a single-drive station where no
// operator loaded it. It wraps the actionable message so callers (the drill) can classify
// "missing copy" via errors.Is rather than matching message text.
var ErrVolumeUnavailable = errors.New("needed volume is not available")

// reloadable wraps a write-eligibility failure that loading a different volume
// could fix (so a single drive can prompt for a swap), as opposed to a hard failure
// (stale catalog, I/O) that swapping would not help.
type reloadable struct{ error }

func reloadableErr(format string, a ...any) error { return reloadable{fmt.Errorf(format, a...)} }

func isReloadable(err error) bool { r := reloadable{}; return errors.As(err, &r) }

// Librarian wraps one medium's volume and drives its changer. It is constructed per
// medium the engine touches; it caches the medium's resolved shape (changer / shelf)
// so callers never type-assert a Volume themselves.
type Librarian struct {
	vol       media.Volume
	medium    string
	cat       *catalog.Catalog
	op        Operator
	autoLabel bool

	drive     media.Drive // has a loaded volume (a robotic library or a single drive)
	hasDrive  bool
	changer   media.Changer // robotic library: bays + mount
	isLibrary bool
	shelf     media.Shelf // single-drive station: the operator-managed room
	isStation bool
}

// New constructs a librarian for a medium's open volume. cat is the catalog the
// label protocol consults and records into; op handles manual swaps (nil =
// unattended); autoLabel allows labeling a blank volume during a write.
//
// The medium's shape is read once, here: a media.Changer is a robotic library;
// anything that is not a Changer is a single-drive station (when it is a media.Shelf)
// or a plain address-identified volume (disk, s3). media.Drive is the device read
// (what's loaded) both changer shapes share.
func New(vol media.Volume, medium string, cat *catalog.Catalog, op Operator, autoLabel bool) *Librarian {
	l := &Librarian{vol: vol, medium: medium, cat: cat, op: op, autoLabel: autoLabel}
	l.drive, l.hasDrive = vol.(media.Drive)
	l.changer, l.isLibrary = vol.(media.Changer)
	l.shelf, l.isStation = vol.(media.Shelf)
	return l
}

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
	return l.vol.ReadFile(pos)
}

// PrepareWrite enforces the label protocol on the loaded volume before writing,
// prompting a swap when the medium is a single drive whose loaded reel won't do.
// Robotic libraries and unattended runs fall straight through to the underlying
// error. It returns the accepted volume's identity to record in a placement.
func (l *Librarian) PrepareWrite(appendable bool, expect string, now time.Time, logf Logf) (string, int, error) {
	name, epoch, err := l.verifyWritable(appendable, now)
	if err == nil {
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
	if l.isLibrary {
		name, epoch, _, aerr := l.Advance(appendable, map[string]bool{}, expect, now, logf)
		if aerr != nil {
			return "", 0, err // surface the original "why the loaded volume won't do"
		}
		return name, epoch, nil
	}
	// A single-drive station prompts the operator to swap in a writable reel.
	if l.isStation {
		for {
			switch _, out, serr := l.requestSwap("", expect, err, logf); {
			case serr != nil:
				return "", 0, serr
			case out == swapUnattended:
				return "", 0, fmt.Errorf("%v (load a writable volume into the drive and retry)", err)
			case out == swapAborted:
				return "", 0, fmt.Errorf("%v (no volume loaded)", err)
			}
			name, epoch, err = l.verifyWritable(appendable, now)
			if err == nil {
				return name, epoch, nil
			}
			if !isReloadable(err) {
				return "", 0, err
			}
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
		return record.Label{}, reloadableErr("medium %q has no volume loaded; load one with `nb load %s <bay>` or label a blank one with `nb label %s <name>`", l.medium, l.medium, l.medium)
	case errors.Is(err, media.ErrForeignVolume):
		return record.Label{}, reloadableErr("medium %q holds non-NBackup data; refusing to overwrite — relabel it explicitly with `nb label --force %s <name>`", l.medium, l.medium)
	case err != nil:
		return record.Label{}, err
	case !labeled: // blank volume
		if !l.autoLabel {
			return record.Label{}, reloadableErr("medium %q is blank/unlabeled; run `nb label %s <name>` first (or set auto_label: true)", l.medium, l.medium)
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
	lv, ok := l.vol.(media.Labeled)
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
		if held := l.cat.SlotsOnLabel(lbl.Name); len(held) > 0 {
			return "", 0, reloadableErr("medium %q is not appendable and volume %q already holds %d run(s); load a fresh volume", l.medium, lbl.Name, len(held))
		}
	}
	if err := l.cat.RecordVolume(lbl); err != nil {
		return "", 0, err
	}
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
// cannot hold the next slot), so a multi-volume copy/sync keeps going. It first
// tries any mountable bay the run has not yet attempted — a robotic library's other
// bays — then, on a single drive with a room, prompts the operator to load another
// reel. A plain volume with no changer returns an actionable error. `tried`
// accumulates the volumes already attempted so the loop terminates; `wasEmpty`
// reports whether the new volume started with no runs (so the caller can tell "slot
// too big for any volume" from "the previous volume was nearly full").
func (l *Librarian) Advance(appendable bool, tried map[string]bool, expect string, now time.Time, logf Logf) (volName string, epoch int, wasEmpty bool, err error) {
	switch {
	case l.isLibrary:
		// A robotic library rolls itself onto its next writable bay.
		return l.advanceViaLibrary(appendable, tried, now, logf)
	case l.isStation:
		// A single-drive station prompts the operator to load another reel.
		return l.advanceViaShelf(appendable, tried, expect, now, logf)
	default:
		return "", 0, false, fmt.Errorf("medium %q is a single volume with no changer; it cannot span volumes", l.medium)
	}
}

// advanceViaLibrary rolls a robotic library onto its next writable bay after the
// loaded one filled: it marks the filled bay tried, then mounts each not-yet-tried
// bay until one verifies writable (skipping wrong-pool / occupied / blank-without-
// autoLabel bays), or reports that no further bay can be written.
func (l *Librarian) advanceViaLibrary(appendable bool, tried map[string]bool, now time.Time, logf Logf) (string, int, bool, error) {
	// The volume that just filled must not be rolled back to; mark it tried.
	if st, ok := l.changer.Loaded(); ok {
		tried[st.ID] = true
	}
	bays, err := l.changer.Bays()
	if err != nil {
		return "", 0, false, err
	}
	var lastErr error
	for _, b := range bays {
		if tried[b.ID] {
			continue
		}
		tried[b.ID] = true
		if err := l.changer.Mount(b.ID); err != nil {
			return "", 0, false, err
		}
		name, epoch, verr := l.verifyWritable(appendable, now)
		if verr != nil {
			lastErr = verr // wrong pool / holds runs / blank with autoLabel off: try the next bay
			continue
		}
		empty := len(l.cat.SlotsOnLabel(name)) == 0
		logf.log("medium %q: rolled to bay %s (volume %q)", l.medium, b.ID, name)
		return name, epoch, empty, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all %d bays are already loaded or tried", len(bays))
	}
	return "", 0, false, fmt.Errorf("medium %q has no further writable bay (load or relabel more volumes): %w", l.medium, lastErr)
}

// advanceViaShelf prompts the operator to load another reel into a single-drive
// station's drive after the loaded one filled, looping until a writable reel is in
// the drive or the operator aborts. Unattended (no operator) it returns an
// actionable error rather than blocking.
func (l *Librarian) advanceViaShelf(appendable bool, tried map[string]bool, expect string, now time.Time, logf Logf) (string, int, bool, error) {
	for {
		reel, out, err := l.requestSwap("", expect, fmt.Errorf("volume full; another volume is needed"), logf)
		switch {
		case err != nil:
			return "", 0, false, err
		case out == swapUnattended:
			return "", 0, false, fmt.Errorf("medium %q drive is full; load a fresh volume and re-run (the copy resumes where it stopped)", l.medium)
		case out == swapAborted:
			return "", 0, false, fmt.Errorf("medium %q drive is full and no further volume was loaded", l.medium)
		}
		if tried[reel] {
			return "", 0, false, fmt.Errorf("medium %q: volume %q was already used and is full", l.medium, reel)
		}
		tried[reel] = true
		name, epoch, verr := l.verifyWritable(appendable, now)
		if verr == nil {
			empty := len(l.cat.SlotsOnLabel(name)) == 0
			return name, epoch, empty, nil
		}
		if !isReloadable(verr) {
			return "", 0, false, verr
		}
		// reloadable (blank without autoLabel, wrong pool, …): prompt for another reel
	}
}

// Remaining reports the writable bytes left on the volume currently in the drive,
// when that is knowable: a finite bay/reel. ok is false for an unbounded volume, a
// medium whose remaining capacity software cannot see (a real drive reports EOT only
// by hitting it), or a non-changer medium — the caller then relies on the reactive
// media.ErrVolumeFull path instead of pre-checking.
func (l *Librarian) Remaining() (int64, bool) {
	if !l.hasDrive {
		return 0, false
	}
	st, ok := l.drive.Loaded()
	if !ok || st.Capacity <= 0 {
		return 0, false
	}
	if st.Used >= st.Capacity {
		return 0, true
	}
	return st.Capacity - st.Used, true
}

// CanSpan reports whether a write to this medium can span volumes mid-archive: it
// needs either a knowable remaining capacity (a finite reel, so the writer can size
// each part to fit and roll proactively) or a configured part_size. Disk and any
// medium without a loaded finite volume cannot span. The engine uses this to serialize
// workers when spanning is possible (a single drive cannot interleave two archives'
// parts).
func (l *Librarian) CanSpan(partSize int64) bool {
	if partSize > 0 {
		return true
	}
	_, known := l.Remaining()
	return known
}

// WriteSink drives a multi-part, possibly multi-volume write for the slotio writer:
// it sizes each part to the loaded volume's remaining capacity (capped by part_size),
// rolls onto a fresh volume when the loaded one fills, and places the seal record. It
// is created after PrepareWrite has mounted and accepted the first volume.
type WriteSink struct {
	l          *Librarian
	appendable bool
	partSize   int64
	now        time.Time
	logf       Logf
	tried      map[string]bool
	volume     string
	epoch      int
}

// WriteSink builds a sink starting on the volume PrepareWrite accepted (its label and
// epoch). partSize (0 = none) caps each part for media whose remaining capacity is
// unknowable or to bound part size deliberately.
func (l *Librarian) WriteSink(volume string, epoch int, appendable bool, partSize int64, now time.Time, logf Logf) *WriteSink {
	return &WriteSink{l: l, appendable: appendable, partSize: partSize, now: now, logf: logf,
		tried: map[string]bool{}, volume: volume, epoch: epoch}
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
		return err
	}
	s.volume, s.epoch = volName, epoch
	return nil
}

// NextPart implements slotio.VolumeSink: it rolls onto a fresh volume if the loaded
// one cannot hold a header plus a byte, then returns the volume and the part's byte cap.
func (s *WriteSink) NextPart() (media.Volume, int64, string, int, error) {
	for max := s.maxPart(); max >= 0 && max < 1; max = s.maxPart() {
		if err := s.advance(); err != nil {
			return nil, 0, "", 0, err
		}
	}
	return s.l.vol, s.maxPart(), s.volume, s.epoch, nil
}

// PlaceSeal implements slotio.VolumeSink: it rolls first if the seal (one whole file
// of the given payload size) will not fit the loaded volume.
func (s *WriteSink) PlaceSeal(size int64) (media.Volume, string, int, error) {
	if room, known := s.l.Remaining(); known && room-record.HeaderBlock < size {
		if err := s.advance(); err != nil {
			return nil, "", 0, err
		}
	}
	return s.l.vol, s.volume, s.epoch, nil
}

// promptSwap asks the operator (via l.op) to pick a reel to load on a single-drive
// station. need is the specific volume label wanted (reads) or "" (writes); expect
// is the volume a write would prefer (the oldest reusable tape) or "".
func (l *Librarian) promptSwap(need, expect string, cause error) (string, bool) {
	shelf, err := l.shelf.Shelf()
	if err != nil {
		return "", false
	}
	var loaded media.VolumeStatus
	if l.hasDrive {
		if st, ok := l.drive.Loaded(); ok {
			loaded = st
		}
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	return l.op.Swap(SwapRequest{Medium: l.medium, Reason: reason, Need: need, Expect: expect, Loaded: loaded, Shelf: shelf})
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
	logf.log("loading reel %s into the %q drive", reel, l.medium)
	if err := l.shelf.Insert(reel); err != nil {
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
	if !l.isLibrary && !l.isStation {
		return nil // address-identified: a single volume, nothing to mount
	}
	if l.mountedMatches(volume) {
		return nil // the right tape is already in the drive
	}
	// A single-drive station prompts the operator to swap the needed reel in.
	if l.isStation {
		return l.mountViaShelf(volume)
	}
	// A robotic library auto-mounts the bay holding the needed label.
	bays, err := l.changer.Bays()
	if err != nil {
		return err
	}
	for _, b := range bays {
		if b.Label == volume {
			return l.changer.Mount(b.ID)
		}
	}
	return fmt.Errorf("%w: volume %q (holding a copy of the slot on %q) is not in the library; load it with `nb load %s <bay>`", ErrVolumeUnavailable, volume, l.medium, l.medium)
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
			return fmt.Errorf("%w: medium %q needs volume %q in the drive (a copy of the slot is on it); load it and retry", ErrVolumeUnavailable, l.medium, volume)
		case out == swapAborted:
			return fmt.Errorf("%w: volume %q was not loaded into the %q drive", ErrVolumeUnavailable, volume, l.medium)
		}
	}
}

// assertVolume confirms the volume mounted on the medium matches the recorded
// identity (label name + epoch) of a part to read, before reading from it.
func (l *Librarian) assertVolume(volume string, epoch int) error {
	lv, ok := l.vol.(media.Labeled)
	if !ok {
		return nil // address-identified: identity is the medium itself
	}
	lbl, labeled, err := lv.ReadLabel()
	if err != nil {
		return err
	}
	if !labeled || lbl.Name != volume || lbl.Epoch != epoch {
		return fmt.Errorf("medium %q has volume %q (epoch %d) mounted, but a slot part is on %q (epoch %d) — mount it or run `nb rebuild`",
			l.medium, lbl.Name, lbl.Epoch, volume, epoch)
	}
	return nil
}

// readVolumeLabel reads the loaded volume's label name, if any. It is a no-op for
// address-identified media that carry no label.
func (l *Librarian) readVolumeLabel() (name string, labeled bool, err error) {
	lv, ok := l.vol.(media.Labeled)
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
// foreign data, or (without --force) a tape that still holds protected slots;
// relabeling an NBackup volume requires relabel and bumps the epoch. A relabel
// wipes the tape, so it then drops the catalog placements that referenced the old
// volume — for any medium, landing or offsite — and records the new identity, so
// the catalog stops reporting a copy that no longer exists.
func (l *Librarian) Label(name string, relabel, force bool, minAge time.Duration, now time.Time, logf Logf) error {
	lv, ok := l.vol.(media.Labeled)
	if !ok {
		return fmt.Errorf("medium %q is address-identified and does not use labels", l.medium)
	}

	// On a robotic library, pick the physical bay this label belongs to and mount it —
	// an existing tape for relabel, a blank one for a new label. On a single-drive
	// station there is no bay to choose: labeling acts on whatever reel the operator
	// has loaded into the drive.
	if l.isLibrary {
		bay, err := l.chooseBay(name, relabel)
		if err != nil {
			return err
		}
		if err := l.changer.Mount(bay); err != nil {
			return err
		}
	}

	cur, labeled, err := lv.ReadLabel()
	epoch := 1
	wiped := "" // the volume a relabel overwrites; its placements become stale
	switch {
	case errors.Is(err, media.ErrForeignVolume):
		if !force {
			return fmt.Errorf("volume holds non-NBackup data; refusing to overwrite (use --force)")
		}
	case err != nil:
		return err
	case labeled:
		if !relabel {
			return fmt.Errorf("volume is already labeled %q (epoch %d); use --relabel to reuse it", cur.Name, cur.Epoch)
		}
		// Reuse the prune/recycle retention test: judge protection over the
		// medium's own slots (so "a newer full exists" is medium-wide), then
		// refuse if the tape being relabeled still holds a protected slot. Reading
		// the catalog rather than scanning the mounted reel correctly attributes a
		// spanned slot to every tape it touches — even the head tape, whose seal
		// record lives only on the last tape of the span.
		floor := retention.Compute(l.cat.SlotsOn(l.medium), minAge, now)
		if id, reason, ok := floor.First(l.cat.SlotsOnLabel(cur.Name)); ok && !force {
			return fmt.Errorf("volume %q still holds protected slot %s (%s); refusing to relabel (use --force)", cur.Name, id, reason)
		}
		epoch = cur.Epoch + 1
		wiped = cur.Name
	}

	lbl := record.Label{Name: name, Pool: l.medium, Epoch: epoch, WrittenAt: now}
	if err := lv.WriteLabel(lbl); err != nil {
		return err
	}
	if got, ok, err := lv.ReadLabel(); err != nil || !ok || got.Name != name {
		return fmt.Errorf("label write could not be confirmed (read back %q, ok=%v, err=%v)", got.Name, ok, err)
	}
	logf.log("labeled %q as %q (epoch %d)", l.medium, name, epoch)

	return l.reconcileRelabel(wiped, lbl)
}

// reconcileRelabel updates the catalog after a (re)label wrote lbl. If wiped names
// the volume the relabel overwrote, it drops the catalog placements that pointed at
// it so it stops reporting copies that no longer exist: a spanned slot crossing the
// wiped tape has its whole medium copy removed (its other parts are now orphaned,
// reclaimable bytes). This is targeted — unlike a full Rebuild it leaves every other
// medium and every intact tape on this one untouched, and it runs for any relabeled
// medium, not just the landing one. It then registers the volume's new identity
// (empty, fresh epoch) so the catalog reflects the relabel immediately rather than
// learning it lazily at the next write.
func (l *Librarian) reconcileRelabel(wiped string, lbl record.Label) error {
	if wiped != "" {
		for _, s := range l.cat.SlotsOnLabel(wiped) {
			if _, err := l.cat.RemovePlacement(s.ID, l.medium); err != nil {
				return fmt.Errorf("drop placements on relabeled volume %q: %w", wiped, err)
			}
		}
	}
	if err := l.cat.RecordVolume(lbl); err != nil {
		return fmt.Errorf("record relabeled volume %q: %w", lbl.Name, err)
	}
	return nil
}

// chooseBay selects which physical bay a label operation targets on a robotic
// library. A bay explicitly loaded (`nb load <bay>`) is the target — label and
// relabel act on it, just as a single-drive station labels whatever reel is in the
// drive — so loading a bay then labeling it does what it says, and `--relabel` on a
// loaded tape recycles it to the new name. With nothing loaded, a relabel finds the
// tape by its current name (an in-place re-stamp) and a new label grabs a blank bay.
// Label() enforces the safety rules on the chosen bay (blank for a new label,
// --relabel + protected-slot guard for a reuse); chooseBay only refuses to create a
// duplicate label on a second bay.
func (l *Librarian) chooseBay(name string, relabel bool) (string, error) {
	bays, err := l.changer.Bays()
	if err != nil {
		return "", err
	}
	loaded := ""
	if cur, ok := l.changer.Loaded(); ok {
		loaded = cur.ID
	}
	// Refuse to stamp a name another bay already carries (a duplicate label). For a
	// relabel, the loaded bay is exempt — re-stamping it (same name, fresh epoch) is
	// allowed — and a name held by a *different* bay is reachable by loading it.
	for _, b := range bays {
		if b.Label == name && !(relabel && b.ID == loaded) {
			if relabel {
				return "", fmt.Errorf("a tape labeled %q already exists on bay %s; load it to relabel it, or pick a different name", name, b.ID)
			}
			return "", fmt.Errorf("a tape labeled %q already exists; use --relabel to reuse it", name)
		}
	}
	if relabel {
		// A loaded bay is the explicit recycle target (`nb load <bay>` picked it), so
		// --relabel can rename/recycle whatever it holds; with nothing loaded, re-stamp
		// the tape currently named `name` in place.
		if loaded != "" {
			return loaded, nil
		}
		for _, b := range bays {
			if b.Label == name {
				return b.ID, nil
			}
		}
		return "", fmt.Errorf("no tape loaded and none labeled %q; run `nb load %s <bay>` to pick the tape to recycle", name, l.medium)
	}
	// A new label takes a blank bay, preferring one already loaded so `nb load <blank>`
	// then `nb label` directs it; a loaded but non-blank bay is left alone (recycle it
	// with --relabel) and the next free blank is used instead.
	if loaded != "" {
		for _, b := range bays {
			if b.ID == loaded && b.Blank {
				return loaded, nil
			}
		}
	}
	for _, b := range bays {
		if b.Blank {
			return b.ID, nil
		}
	}
	return "", fmt.Errorf("no blank bay available; all %d are in use — load a bay to recycle and relabel it with `nb label --relabel`", len(bays))
}

// View is a medium's physical inventory for `nb medium <name>`: a robotic library's
// bays, or a single-drive station's drive plus any room reels it can load. Exactly
// one of Library/Station is set.
type View struct {
	Library bool                 // robotic: Bays is the full inventory
	Loaded  string               // loaded bay id (library)
	Bays    []media.VolumeStatus // every bay and what it holds
	Station bool                 // single drive: Drive is what's loaded
	Drive   media.VolumeStatus   // the reel/tape in the drive (when DriveOK)
	DriveOK bool                 // false when the drive is empty
	Shelf   []media.VolumeStatus // reels available to load (single-drive station)
}

// View inventories the medium's changer for display. A robotic library reports its
// bays; a single-drive station reports the loaded volume and its room reels.
func (l *Librarian) View() (View, error) {
	if l.isLibrary {
		bays, err := l.changer.Bays()
		if err != nil {
			return View{}, err
		}
		loaded := ""
		if st, ok := l.changer.Loaded(); ok {
			loaded = st.ID
		}
		return View{Library: true, Loaded: loaded, Bays: bays}, nil
	}
	if l.isStation {
		v := View{Station: true}
		v.Drive, v.DriveOK = l.drive.Loaded()
		if shelf, err := l.shelf.Shelf(); err == nil {
			v.Shelf = shelf
		}
		return v, nil
	}
	return View{}, fmt.Errorf("medium %q has no changer to inventory (it is addressed directly, not by loading volumes)", l.medium)
}

// Load mounts a volume on a changer medium, addressed by bay/reel id, or by label
// when byLabel is set (the host-side "load the volume labeled X" helper).
func (l *Librarian) Load(target string, byLabel bool, logf Logf) error {
	// A single-drive station loads a reel from the room into its one drive.
	if l.isStation {
		return l.insertFromShelf(target, byLabel, logf)
	}
	if !l.isLibrary {
		return fmt.Errorf("medium %q has no changer to load (it is addressed directly, not by loading volumes)", l.medium)
	}
	bay := target
	if byLabel {
		bays, err := l.changer.Bays()
		if err != nil {
			return err
		}
		found := ""
		for _, b := range bays {
			if b.Label == target {
				found = b.ID
				break
			}
		}
		if found == "" {
			return fmt.Errorf("no tape labeled %q in the library", target)
		}
		bay = found
	}
	if err := l.changer.Mount(bay); err != nil {
		return err
	}
	name, labeled, lerr := l.readVolumeLabel()
	switch {
	case labeled:
		logf.log("loaded %q: bay %s holds %q", l.medium, bay, name)
	case errors.Is(lerr, media.ErrForeignVolume):
		logf.log("loaded %q: bay %s (foreign — non-NBackup data; `nb label --relabel --force` to overwrite)", l.medium, bay)
	default:
		logf.log("loaded %q: bay %s (blank)", l.medium, bay)
	}
	return nil
}

// insertFromShelf loads a reel from a station's room into its single drive, addressed
// by reel id or (with byLabel) by the label it carries.
func (l *Librarian) insertFromShelf(target string, byLabel bool, logf Logf) error {
	shelf, err := l.shelf.Shelf()
	if err != nil {
		return err
	}
	reel := ""
	for _, b := range shelf {
		if (byLabel && b.Label == target) || (!byLabel && b.ID == target) {
			reel = b.ID
			break
		}
	}
	if reel == "" {
		what := "reel"
		if byLabel {
			what = "tape labeled"
		}
		return fmt.Errorf("no %s %q in the %q room (it may already be in the drive)", what, target, l.medium)
	}
	if err := l.shelf.Insert(reel); err != nil {
		return err
	}
	name, labeled, lerr := l.readVolumeLabel()
	switch {
	case labeled:
		logf.log("loaded %q: reel %s holds %q", l.medium, reel, name)
	case errors.Is(lerr, media.ErrForeignVolume):
		logf.log("loaded %q: reel %s (foreign — non-NBackup data; `nb label --relabel --force` to overwrite)", l.medium, reel)
	default:
		logf.log("loaded %q: reel %s (blank)", l.medium, reel)
	}
	return nil
}
