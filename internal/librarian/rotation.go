// rotation.go — the label rotation: Advance rolls onto the next writable volume, recycling the oldest Floor-cleared tape — ARCHITECTURE.md's "whole-volume recycle on write (label rotation, Amanda's tapecycle)".
package librarian

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/retention"
)

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
	var name string
	var epoch int
	var empty, found bool
	scanErr := l.eachLoadableSlot(l.drive,
		func(s media.SlotStatus) bool { return tried["slot:"+strconv.Itoa(s.Slot)] },
		func(s media.SlotStatus, loadErr error, _ string, labeled bool, lerr error) bool {
			key := "slot:" + strconv.Itoa(s.Slot)
			if loadErr != nil {
				tried[key] = true
				lastErr = loadErr // a cartridge this drive can't load (wrong generation, dud): try the next slot
				return false
			}
			if !labeled && lerr == nil {
				blanks = append(blanks, s.Slot) // a fresh reel: wanted only if no labeled volume will do
				return false
			}
			tried[key] = true
			name, epoch, empty, found = accept(s.Slot)
			return found
		})
	if scanErr != nil {
		return "", 0, false, scanErr
	}
	if found {
		return name, epoch, empty, nil
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
		n := 0
		if slots, err := l.changer.Slots(); err == nil {
			n = len(slots)
		}
		lastErr = fmt.Errorf("all %d slots are already loaded or tried", n)
	}
	return "", 0, false, l.noReusableErr(tried, now, lastErr)
}

// advanceViaShelf prompts the operator to load another reel into a single-drive
// station's drive after the loaded one filled, looping until a writable reel is in
// the drive or the operator aborts. Unattended (no operator) it returns an
// actionable error rather than blocking. It is swapForWrite with the spanning
// path's own wording for the unattended and aborted outcomes.
func (l *Librarian) advanceViaShelf(appendable bool, tried map[string]bool, expect string, now time.Time, logf Logf) (string, int, bool, error) {
	return l.swapForWrite(appendable, tried, expect,
		fmt.Errorf("volume full; another volume is needed"),
		func(error) error {
			return fmt.Errorf("medium %q drive is full; label a blank volume and load it, then re-run", l.medium)
		},
		func(error) error {
			return fmt.Errorf("medium %q drive is full and no further volume was loaded", l.medium)
		},
		now, logf)
}

// swapForWrite is the manual-station swap loop shared by PrepareWrite (starting a
// write) and advanceViaShelf (spanning onto another reel): suggest the oldest reusable
// tape so the operator is told which reel to load (Amanda's "load tape X" — a blank is
// equally accepted), prompt for a swap, then accept-or-recycle whatever was loaded,
// looping until a writable reel is in the drive or the operator gives up. cause seeds
// the prompt's "why" and is updated to the latest rejection so each re-prompt says what
// was wrong with the last reel; unattended/aborted wrap the current cause into the
// caller's actionable message for the two no-operator outcomes. tried accumulates the
// reels attempted (spanning passes its run-wide set; a fresh write starts empty) — a
// reel offered twice is a hard error, since nothing about it can have changed.
func (l *Librarian) swapForWrite(appendable bool, tried map[string]bool, expect string, cause error,
	unattended, aborted func(cause error) error, now time.Time, logf Logf) (string, int, bool, error) {
	for {
		if expect == "" {
			if rec, ok := l.oldestReusable(tried, now); ok {
				expect = rec.Label.Name
			}
		}
		reel, out, err := l.requestSwap("", expect, cause, logf)
		switch {
		case err != nil:
			return "", 0, false, err
		case out == swapUnattended:
			return "", 0, false, unattended(cause)
		case out == swapAborted:
			return "", 0, false, aborted(cause)
		}
		// A named choice (a slotted station's shelf) is tracked by id; a PHYSICAL
		// swap on a real drive returns no id — the label read below identifies the
		// reel and acceptOrRecycle tracks it by name, so "" is never bookkept (it
		// would false-trip this check on the second swap of a spanning run).
		if reel != "" {
			if tried[reel] {
				return "", 0, false, fmt.Errorf("medium %q: volume %q was already used and is full", l.medium, reel)
			}
			tried[reel] = true
		}
		name, epoch, empty, verr := l.acceptOrRecycle(appendable, tried, now, logf)
		if verr == nil {
			tried[name] = true // also track by label name (oldestReusable keys on names)
			l.reserve(name)    // claim it against concurrent recycling
			return name, epoch, empty, nil
		}
		if errors.Is(verr, errBlankNeedsLabel) {
			// auto_label is off and the operator loaded an unlabeled reel. NBackup may
			// not label a blank without auto_label, and re-prompting only offers more
			// blanks it must reject too — eventually looping back to a reel already
			// used and full. Fail fast with the actionable reason instead of looping;
			// pre-label the reels or set auto_label: true.
			return "", 0, false, verr
		}
		if !isReloadable(verr) {
			return "", 0, false, verr
		}
		// reloadable (wrong pool, still-protected …): surface why on the next prompt
		// and recompute the suggestion, then ask for another reel.
		cause, expect = verr, ""
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
	for _, v := range pool {
		if _, _, kept := l.protectedRun(v.Label.Name, l.minAge, now); kept {
			continue // some archive on this tape is still within retention — not reusable
		}
		return v, true
	}
	return catalog.VolumeRecord{}, false
}

// protectedRun is the rotation's safety gate, shared by every path that decides
// whether a volume may be wiped (recycle, relabel, the fail-loud refusal): it computes
// the retention Floor over this medium's own archives (so a copy elsewhere never makes
// a volume reusable) and reports the first run on the labeled volume the Floor still
// keeps — with why, for a refusal message. kept=false means the whole tape is
// Floor-cleared and the rotation may reuse it.
func (l *Librarian) protectedRun(label string, minAge time.Duration, now time.Time) (runID, reason string, kept bool) {
	floor := retention.Compute(l.cat.ArchivesOn(l.medium), minAge, now)
	return floor.First(l.cat.RunIDsOnLabel(label))
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
	if _, _, kept := l.protectedRun(lbl.Name, l.minAge, now); kept {
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
	if err := l.eachLoadableSlot(l.drive, nil,
		func(s media.SlotStatus, loadErr error, n string, labeled bool, _ error) bool {
			if loadErr != nil {
				return false // a cartridge this drive can't load holds no label we can read
			}
			if labeled {
				l.learnBarcode(n, s.Barcode) // remember every label the scan read, not just the hit
				if n == name {
					slot, drive, ok = s.Slot, l.drive, true
					return true
				}
			}
			return false
		}); err != nil {
		return 0, 0, false, err
	}
	return slot, drive, ok, nil
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
	protected := false
	reason := ""
	for _, v := range l.cat.Volumes() {
		if v.Label.Pool != l.medium || tried[v.Label.Name] {
			continue
		}
		if _, r, kept := l.protectedRun(v.Label.Name, l.minAge, now); kept {
			protected = true
			reason = r
			break
		}
	}
	if !protected {
		return fmt.Errorf("medium %q has no further writable bay (load or relabel more volumes): %w", l.medium, lastErr)
	}
	// Name the actual protection: with minimum_age:0 the blocker is a DLE's live recovery
	// chain, not age — "within retention" alone reads as minimum_age and misleads.
	msg := fmt.Sprintf("medium %q: no writable volume — every volume in the pool still holds a protected run (%s)", l.medium, reason)
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
