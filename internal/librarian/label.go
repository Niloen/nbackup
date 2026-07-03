// label.go — the operator acts: Label/(re)label, Load, and the changer inventory View — ARCHITECTURE.md's "the deliberate operator act that makes a tape writable".
package librarian

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// Label writes (or rewrites) the identity label of a medium's volume — the
// deliberate operator act that makes a tape writable. It refuses to overwrite
// foreign data, or (without --force) a tape that still holds protected archives;
// relabeling an NBackup volume requires relabel and bumps the epoch. A relabel
// wipes the tape, so it then drops the catalog placements that referenced the old
// volume — for any medium, landing or offsite — and records the new identity, so
// the catalog stops reporting a copy that no longer exists.
func (l *Librarian) Label(name string, relabel, force bool, now time.Time, logf Logf) error {
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
		if err := l.duplicateLabelErr(name, "", relabel, force); err != nil {
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
		if id, reason, ok := l.protectedRun(cur.Name, l.minAge, now); ok && !force {
			return fmt.Errorf("volume %q still holds protected run %s (%s); refusing to relabel (use --force)", cur.Name, id, reason)
		}
		epoch = cur.Epoch + 1
		wiped = cur.Name
	}
	// The relabel half of the duplicate-name guard: renaming this tape (or stamping a
	// blank/foreign one under --relabel) must not collide with a volume the catalog
	// already knows. Restamping the tape that carries the name (cur.Name == name, the
	// in-place recycle) is the legitimate reuse and passes.
	if err := l.duplicateLabelErr(name, wiped, relabel, force); err != nil {
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
func (l *Librarian) duplicateLabelErr(name, current string, relabel, force bool) error {
	known, ok := l.cat.Volume(name)
	if !ok || current == name || force {
		return nil
	}
	if relabel {
		// A relabel acts on whatever tape is loaded, and the operator asked to rename it
		// to a name another cartridge already carries — self-referential advice to "relabel
		// <name>" would just re-run this. To recycle the tape that already holds the name,
		// load THAT tape first (then --relabel bumps its epoch in place).
		return fmt.Errorf("a volume labeled %q already exists (pool %q, epoch %d); relabel recycles the loaded tape, so renaming it to %q would duplicate that — to recycle the existing %q, load it first: `nb load --label %s %s`, then `nb label --relabel %s %s`; or pick a different name (--force overrides if that volume no longer exists)",
			name, known.Label.Pool, known.Label.Epoch, name, name, l.medium, name, l.medium, name)
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
	named, blank := -1, -1
	if err := l.eachLoadableSlot(0, nil,
		func(s media.SlotStatus, loadErr error, n string, labeled bool, lerr error) bool {
			if loadErr != nil {
				return false // a cartridge this drive can't load is not a candidate for labeling
			}
			switch {
			case labeled && n == name && named < 0:
				named = s.Slot
			case !labeled && lerr == nil && blank < 0:
				blank = s.Slot
			}
			return false
		}); err != nil {
		return -1, err
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
