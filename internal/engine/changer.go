package engine

// This file is the engine's media-shape boundary: the single place that dispatches
// on whether a medium is a robotic media.Library, a media.Station (real single
// drive), or a media.ShelfStation (disk-emulated station). Everything here turns an
// operator-level intent — make a volume writable, mount the one holding a slot,
// (re)label, load, inventory — into the right positioning calls. The rest of the
// engine stays medium-shape-agnostic and never type-asserts a Volume itself.

import (
	"errors"
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/policy"
	"github.com/Niloen/nbackup/internal/slot"
	"github.com/Niloen/nbackup/internal/slotio"
)

// Operator handles physical actions the engine cannot perform itself — chiefly
// swapping the reel in a single-drive (manual) tape station when the loaded tape
// won't do. The CLI implements it interactively over stdin; an unattended run
// leaves it nil, so a manual swap degrades to an actionable error instead of
// blocking forever waiting for a human.
type Operator interface {
	// Swap asks the operator to load a reel into the drive for the stated need and
	// returns the chosen reel's Bay id (from req.Shelf), or ok=false to abort
	// (leaving the drive unchanged).
	Swap(req SwapRequest) (reel string, ok bool)
}

// SwapRequest describes why a manual tape swap is needed and what is available.
type SwapRequest struct {
	Medium string               // medium name
	Pool   string               // its label pool
	Reason string               // why the loaded reel won't do (the underlying error)
	Need   string               // a specific volume label wanted (read); "" means any writable (write)
	Expect string               // on a write, the label the run expects to (re)use (the oldest reusable volume); "" when a fresh tape is expected
	Loaded media.VolumeStatus   // what is in the drive now (zero value when empty)
	Shelf  []media.VolumeStatus // reels available to load
}

// reloadable wraps a write-eligibility failure that loading a different volume
// could fix (so a manual station can prompt for a swap), as opposed to a hard
// failure (stale catalog, I/O) that swapping would not help.
type reloadable struct{ error }

func reloadableErr(format string, a ...any) error { return reloadable{fmt.Errorf(format, a...)} }

func isReloadable(err error) bool { r := reloadable{}; return errors.As(err, &r) }

// verifyWritable enforces the label protocol before writing to a medium (a dump
// to the landing, or a copy to a target). Address-identified media (disk, s3) are
// trusted by their path/bucket and return that name with epoch 0. For labeled
// (tape) media it refuses a foreign, blank (unless auto_label), wrong-pool, wrong,
// or relabeled-since volume — the overwrite and wrong-tape protection — records the
// accepted label, and returns the volume identity to record in a placement.
func (e *Engine) verifyWritable(vol media.Volume, medium string, appendable bool, now time.Time) (volName string, epoch int, err error) {
	lv, ok := vol.(media.Labeled)
	if !ok {
		return medium, 0, nil // address-identified: identity is the medium itself
	}
	lbl, labeled, err := lv.ReadLabel()
	switch {
	case errors.Is(err, media.ErrNoVolume):
		return "", 0, reloadableErr("medium %q has no tape loaded; load one with `nb load %s <bay>` or label a blank one with `nb label %s <name>`", medium, medium, medium)
	case errors.Is(err, media.ErrForeignVolume):
		return "", 0, reloadableErr("medium %q holds non-NBackup data; refusing to overwrite — relabel it explicitly with `nb label --force %s <name>`", medium, medium)
	case err != nil:
		return "", 0, err
	case !labeled: // blank volume
		if !e.cfg.AutoLabel {
			return "", 0, reloadableErr("medium %q is blank/unlabeled; run `nb label %s <name>` first (or set auto_label: true)", medium, medium)
		}
		lbl = media.Label{Name: fmt.Sprintf("%s-%s", medium, slot.DateString(now)), Pool: medium, Epoch: 1, WrittenAt: now}
		if err := lv.WriteLabel(lbl); err != nil {
			return "", 0, err
		}
	}
	if lbl.Pool != "" && lbl.Pool != medium {
		return "", 0, reloadableErr("mounted volume %q belongs to pool %q, not %q — wrong tape", lbl.Name, lbl.Pool, medium)
	}
	// Relabeled-since check: a tape we know whose epoch advanced means the catalog
	// is stale for it. (A genuinely different tape is not an error — that is what
	// loading another tape in the pool is for.)
	if known, ok := e.cat.Volume(lbl.Name); ok && known.Label.Epoch != lbl.Epoch {
		return "", 0, fmt.Errorf("volume %q was relabeled since the catalog was updated (epoch %d mounted vs %d cached); run `nb rebuild`", lbl.Name, lbl.Epoch, known.Label.Epoch)
	}
	// One-run-per-tape media refuse to append onto a tape that already holds a run.
	if !appendable {
		if held := e.cat.SlotsOnVolume(lbl.Name); len(held) > 0 {
			return "", 0, reloadableErr("medium %q is not appendable and tape %q already holds %d run(s); load a fresh tape", medium, lbl.Name, len(held))
		}
	}
	if err := e.cat.RecordVolume(lbl); err != nil {
		return "", 0, err
	}
	return lbl.Name, lbl.Epoch, nil
}

// verifyWritableInteractive is verifyWritable plus the manual-station swap loop:
// on a single-drive (manual) medium whose loaded tape is unusable in a way a
// different reel would fix, it prompts the operator to swap and retries. Robotic
// libraries and unattended runs fall straight through to the underlying error.
func (e *Engine) verifyWritableInteractive(vol media.Volume, medium string, appendable bool, now time.Time, logf Logf) (string, int, error) {
	for {
		name, epoch, err := e.verifyWritable(vol, medium, appendable, now)
		if err == nil {
			return name, epoch, nil
		}
		mc, ok := vol.(media.ShelfStation)
		if !ok || !isReloadable(err) {
			return "", 0, err
		}
		if e.op == nil {
			return "", 0, fmt.Errorf("%v (load a writable tape into the drive and retry)", err)
		}
		expect := e.expectedTapeFor(medium, now).Label
		reel, ok := e.promptSwap(mc, medium, "", expect, err)
		if !ok {
			return "", 0, fmt.Errorf("%v (no tape loaded)", err)
		}
		logf.log("loading reel %s into the %q drive", reel, medium)
		if err := mc.Insert(reel); err != nil {
			return "", 0, err
		}
	}
}

// promptSwap asks the operator (via e.op) to pick a reel to load on a manual
// medium. need is the specific volume label wanted (reads) or "" (writes); expect
// is the volume a write would prefer (the oldest reusable tape) or "" (reads / a
// fresh tape).
func (e *Engine) promptSwap(mc media.ShelfStation, medium, need, expect string, cause error) (string, bool) {
	shelf, err := mc.Shelf()
	if err != nil {
		return "", false
	}
	var loaded media.VolumeStatus
	if st, ok := mc.LoadedVolume(); ok {
		loaded = st
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	return e.op.Swap(SwapRequest{Medium: medium, Pool: medium, Reason: reason, Need: need, Expect: expect, Loaded: loaded, Shelf: shelf})
}

// assertVolume confirms the volume mounted on a medium matches a placement's
// recorded identity (label name + epoch), before reading from it.
func (e *Engine) assertVolume(vol media.Volume, p catalog.Placement) error {
	lv, ok := vol.(media.Labeled)
	if !ok {
		return nil // address-identified: identity is the medium itself
	}
	lbl, labeled, err := lv.ReadLabel()
	if err != nil {
		return err
	}
	if !labeled || lbl.Name != p.Volume || lbl.Epoch != p.Epoch {
		return fmt.Errorf("medium %q has volume %q (epoch %d) mounted, but slot copy is on %q (epoch %d) — mount it or run `nb rebuild`",
			p.Medium, lbl.Name, lbl.Epoch, p.Volume, p.Epoch)
	}
	return nil
}

// mountForRead loads the volume holding a placement's slot so the reader can seek
// it. A robotic Library mounts the bay automatically; a ShelfStation prompts the
// operator to swap the reel in; a plain Station (real drive) can only check what is
// already loaded and ask the operator to load it by hand; non-changer media are a
// no-op (a single addressable volume).
func (e *Engine) mountForRead(vol media.Volume, p catalog.Placement) error {
	if ss, ok := vol.(media.ShelfStation); ok {
		return e.mountManualForRead(ss, p)
	}
	if _, ok := vol.(media.Station); ok {
		if mountedMatches(vol, p.Volume) {
			return nil // the right tape is already in the drive
		}
		return fmt.Errorf("medium %q needs tape %q in the drive (a copy of the slot is on it); load it and retry", p.Medium, p.Volume)
	}
	ch, ok := vol.(media.Library)
	if !ok {
		return nil // address-identified: a single volume, nothing to mount
	}
	if mountedMatches(ch, p.Volume) {
		return nil // the right bay is already in the drive
	}
	bays, err := ch.Bays()
	if err != nil {
		return err
	}
	for _, b := range bays {
		if b.Label == p.Volume {
			return ch.Mount(b.ID)
		}
	}
	return fmt.Errorf("tape %q (holding a copy of the slot on %q) is not in the library; load it with `nb load %s <bay>`", p.Volume, p.Medium, p.Medium)
}

// mountManualForRead loads the reel a placement needs on a single-drive station: if
// it is not already in the drive, it prompts the operator to swap it in, looping
// until the right tape is loaded or the operator aborts. Unattended, it returns an
// actionable error rather than blocking.
func (e *Engine) mountManualForRead(mc media.ShelfStation, p catalog.Placement) error {
	for {
		if mountedMatches(mc, p.Volume) {
			return nil // the needed reel is already in the drive
		}
		if e.op == nil {
			return fmt.Errorf("medium %q needs tape %q in the drive (a copy of the slot is on it); load it and retry", p.Medium, p.Volume)
		}
		reel, ok := e.promptSwap(mc, p.Medium, p.Volume, "", fmt.Errorf("need tape %q", p.Volume))
		if !ok {
			return fmt.Errorf("tape %q was not loaded into the %q drive", p.Volume, p.Medium)
		}
		if err := mc.Insert(reel); err != nil {
			return err
		}
	}
}

// readVolumeLabel reads the loaded volume's label name, if any. It is a no-op for
// address-identified media that carry no label. Library and Station both embed
// Volume, so a changer handle satisfies it directly.
func readVolumeLabel(vol media.Volume) (name string, labeled bool, err error) {
	lv, ok := vol.(media.Labeled)
	if !ok {
		return "", false, nil
	}
	lbl, ok, err := lv.ReadLabel()
	return lbl.Name, ok, err
}

// mountedMatches reports whether the volume currently in vol's drive carries the
// given label. A read error, an empty drive, or address-identified media all count
// as no match — the caller then mounts the right bay or prompts for a swap.
func mountedMatches(vol media.Volume, label string) bool {
	lbl, labeled, err := readVolumeLabel(vol)
	return err == nil && labeled && lbl == label
}

// readerFor opens a Reader positioned to read a placement's volume, after
// mounting the right tape (on a changer) and verifying its identity. For the
// engine's own medium it reuses the open handle and reader.
func (e *Engine) readerFor(p catalog.Placement) (*slotio.Reader, error) {
	if p.Medium == e.mediumName {
		if err := e.mountForRead(e.vol, p); err != nil {
			return nil, err
		}
		if err := e.assertVolume(e.vol, p); err != nil {
			return nil, err
		}
		return e.reader, nil
	}
	vol, _, _, err := e.mediumVolume(p.Medium)
	if err != nil {
		return nil, err
	}
	if err := e.mountForRead(vol, p); err != nil {
		return nil, err
	}
	if err := e.assertVolume(vol, p); err != nil {
		return nil, err
	}
	return slotio.NewReader(vol, e.fopts), nil
}

// LabelVolume writes (or rewrites) the identity label of a medium's volume — the
// deliberate operator act that makes a tape writable. It refuses to overwrite
// foreign data or a still-active volume unless forced; relabeling our own volume
// requires --relabel and bumps the epoch.
func (e *Engine) LabelVolume(mediumName, name string, relabel, force bool, now time.Time, logf Logf) error {
	vol, def, own, err := e.mediumVolume(mediumName)
	if err != nil {
		return err
	}
	minAge, _ := def.MinAge()
	lv, ok := vol.(media.Labeled)
	if !ok {
		return fmt.Errorf("medium %q is address-identified and does not use labels", mediumName)
	}

	// On a single-drive station there is no bay to choose: labeling acts on whatever
	// reel the operator has loaded into the drive. On a robotic library, pick the
	// physical bay this label belongs to and mount it — an existing tape for
	// --relabel, a blank one for a new label.
	if _, ok := vol.(media.Station); ok {
		// nothing to mount; the loaded reel (if any) is the target
	} else if ch, ok := vol.(media.Library); ok {
		bay, err := chooseBay(ch, name, relabel)
		if err != nil {
			return err
		}
		if err := ch.Mount(bay); err != nil {
			return err
		}
	}

	cur, labeled, err := lv.ReadLabel()
	epoch := 1
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
		slots, serr := catalog.ScanSlots(vol)
		if serr != nil {
			return serr
		}
		if protected := policy.Protected(slots, minAge, now); len(protected) > 0 && !force {
			return fmt.Errorf("volume %q still holds %d protected slot(s); refusing to relabel (use --force)", cur.Name, len(protected))
		}
		epoch = cur.Epoch + 1
	}

	lbl := media.Label{Name: name, Pool: mediumName, Epoch: epoch, WrittenAt: now}
	if err := lv.WriteLabel(lbl); err != nil {
		return err
	}
	if got, ok, err := lv.ReadLabel(); err != nil || !ok || got.Name != name {
		return fmt.Errorf("label write could not be confirmed (read back %q, ok=%v, err=%v)", got.Name, ok, err)
	}
	logf.log("labeled %q as %q (epoch %d)", mediumName, name, epoch)

	// Relabeling the engine's own medium wipes the contents the catalog caches:
	// rebuild from the now-empty volume so the catalog reflects reality.
	if own {
		if _, err := e.cat.Rebuild(map[string]media.Volume{e.mediumName: e.vol}); err != nil {
			return fmt.Errorf("rebuild catalog after labeling: %w", err)
		}
	}
	return nil
}

// chooseBay selects which physical bay a label operation targets: for --relabel,
// the bay already holding that label; for a new label, a blank bay (preferring
// one already in the drive). It refuses to reuse an existing label without
// --relabel, or to label when no blank bay is free.
func chooseBay(ch media.Library, name string, relabel bool) (string, error) {
	bays, err := ch.Bays()
	if err != nil {
		return "", err
	}
	if relabel {
		for _, b := range bays {
			if b.Label == name {
				return b.ID, nil
			}
		}
		return "", fmt.Errorf("no tape labeled %q in the library to relabel", name)
	}
	for _, b := range bays {
		if b.Label == name {
			return "", fmt.Errorf("a tape labeled %q already exists; use --relabel to reuse it", name)
		}
	}
	if cur, ok := ch.Loaded(); ok {
		for _, b := range bays {
			if b.ID == cur && b.Blank {
				return cur, nil // label the blank already in the drive
			}
		}
	}
	for _, b := range bays {
		if b.Blank {
			return b.ID, nil
		}
	}
	return "", fmt.Errorf("no blank bay available; all %d are in use — relabel an aged-out tape with `nb label --relabel`", len(bays))
}

// ChangerView is a medium's physical inventory for `nb medium <name>`: a robotic
// library's bays, or a single-drive station's drive plus any shelf reels it can
// load. Exactly one of Library/Station is set.
type ChangerView struct {
	Library bool                 // robotic: Bays is the full inventory
	Loaded  string               // loaded bay id (library)
	Bays    []media.VolumeStatus // every bay and what it holds
	Station bool                 // single drive: Drive is what's loaded
	Drive   media.VolumeStatus   // the reel/tape in the drive (when DriveOK)
	DriveOK bool                 // false when the drive is empty
	Shelf   []media.VolumeStatus // reels available to load (ShelfStation only)
}

// ChangerView inventories a changer medium for display. A robotic Library reports
// its bays; a single-drive Station reports the loaded volume and (if it can
// enumerate them) the shelf reels.
func (e *Engine) ChangerView(mediumName string) (ChangerView, error) {
	vol, _, _, err := e.mediumVolume(mediumName)
	if err != nil {
		return ChangerView{}, err
	}
	switch m := vol.(type) {
	case media.Library:
		bays, err := m.Bays()
		if err != nil {
			return ChangerView{}, err
		}
		loaded, _ := m.Loaded()
		return ChangerView{Library: true, Loaded: loaded, Bays: bays}, nil
	case media.Station:
		v := ChangerView{Station: true}
		v.Drive, v.DriveOK = m.LoadedVolume()
		if ss, ok := vol.(media.ShelfStation); ok {
			if shelf, err := ss.Shelf(); err == nil {
				v.Shelf = shelf
			}
		}
		return v, nil
	default:
		return ChangerView{}, fmt.Errorf("medium %q has no changer to inventory (it is addressed directly, not by loading tapes)", mediumName)
	}
}

// LoadVolume mounts a volume on a changer medium, addressed by bay id, or by
// label when byLabel is set (the host-side "load the volume labeled X" helper).
func (e *Engine) LoadVolume(mediumName, target string, byLabel bool, logf Logf) error {
	vol, _, _, err := e.mediumVolume(mediumName)
	if err != nil {
		return err
	}
	// A single-drive station loads a reel from the room into its one drive.
	if ss, ok := vol.(media.ShelfStation); ok {
		return e.insertManual(ss, mediumName, target, byLabel, logf)
	}
	ch, ok := vol.(media.Library)
	if !ok {
		return fmt.Errorf("medium %q has no changer to load (it is addressed directly, not by loading tapes)", mediumName)
	}
	bay := target
	if byLabel {
		bays, err := ch.Bays()
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
	if err := ch.Mount(bay); err != nil {
		return err
	}
	if lbl, labeled, _ := readVolumeLabel(vol); labeled {
		logf.log("loaded %q: bay %s holds %q", mediumName, bay, lbl)
	} else {
		logf.log("loaded %q: bay %s (blank)", mediumName, bay)
	}
	return nil
}

// insertManual loads a reel from a station's room into its single drive, addressed
// by reel id or (with byLabel) by the label it carries.
func (e *Engine) insertManual(mc media.ShelfStation, mediumName, target string, byLabel bool, logf Logf) error {
	shelf, err := mc.Shelf()
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
		return fmt.Errorf("no %s %q in the %q room (it may already be in the drive)", what, target, mediumName)
	}
	if err := mc.Insert(reel); err != nil {
		return err
	}
	if lbl, labeled, _ := readVolumeLabel(mc); labeled {
		logf.log("loaded %q: reel %s holds %q", mediumName, reel, lbl)
	} else {
		logf.log("loaded %q: reel %s (blank)", mediumName, reel)
	}
	return nil
}
