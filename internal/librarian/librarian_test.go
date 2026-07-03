package librarian

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/media"
	_ "github.com/Niloen/nbackup/internal/media/tape" // register the file-backed tape changer
	"github.com/Niloen/nbackup/internal/record"
)

// newTapeLib opens a fresh file-backed tape library (slots blank cartridges, 1 MB
// each) with an empty catalog, wrapped in a librarian for the pool named "pool".
func newTapeLib(t *testing.T, slots int, autoLabel bool) (*Librarian, *catalog.Catalog, media.Changer) {
	t.Helper()
	v, err := media.OpenVolume("tape", media.Options{
		"dir": t.TempDir(), "slots": strconv.Itoa(slots), "volume_size": "1048576"})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(v, "pool", cat, nil, autoLabel, 0), cat, v.(media.Changer)
}

// stampSlot loads a slot into drive 0 and writes a label onto its cartridge,
// recording it in the catalog (as a real `nb label` would).
func stampSlot(t *testing.T, l *Librarian, ch media.Changer, cat *catalog.Catalog, slot int, name string, now time.Time) {
	t.Helper()
	if err := ch.Load(slot, 0); err != nil {
		t.Fatal(err)
	}
	lbl := record.Label{Name: name, Pool: "pool", Epoch: 1, WrittenAt: now}
	if err := l.vol.(media.Labeled).WriteLabel(lbl); err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordVolume(lbl); err != nil {
		t.Fatal(err)
	}
}

func drive0(t *testing.T, ch media.Changer) media.DriveStatus {
	t.Helper()
	drs, err := ch.Drives()
	if err != nil || len(drs) == 0 {
		t.Fatalf("Drives: %v", err)
	}
	return drs[0]
}

// TestLabelRefusesDuplicateName locks the duplicate-label guard on every label path:
// once a volume name is in the catalog, labeling ANOTHER tape with it must fail —
// including via the fast path that labels a pre-loaded blank slot (which used to skip
// the duplicate scan entirely, silently minting two tapes both named T-A).
func TestLabelRefusesDuplicateName(t *testing.T) {
	l, _, ch := newTapeLib(t, 4, false)
	now := time.Unix(1, 0).UTC()
	if err := l.Label("T-A", false, false, 0, now, nil); err != nil {
		t.Fatal(err)
	}
	// Pre-load a different, blank slot — the old early-return target.
	if err := ch.Load(2, 0); err != nil {
		t.Fatal(err)
	}
	err := l.Label("T-A", false, false, 0, now, nil)
	if err == nil {
		t.Fatal("labeling a second tape T-A should fail")
	}
	if !strings.Contains(err.Error(), `"T-A" already exists`) {
		t.Fatalf("error should name the existing volume, got: %v", err)
	}
	// The refused label must not have touched the loaded blank.
	if name, labeled, lerr := l.readVolumeLabel(); lerr != nil || labeled {
		t.Fatalf("loaded blank must stay blank after a refused label, got %q labeled=%v err=%v", name, labeled, lerr)
	}
	// --force is the stale-catalog escape hatch.
	if err := l.Label("T-A", false, true, 0, now, nil); err != nil {
		t.Fatalf("--force should override the duplicate guard: %v", err)
	}
}

// TestRelabelRefusesDuplicateName extends the guard to --relabel: recycling a loaded
// tape UNDER a name some other volume already carries would also mint a duplicate.
// Restamping the tape that itself carries the name (the in-place recycle) stays legal.
func TestRelabelRefusesDuplicateName(t *testing.T) {
	l, _, _ := newTapeLib(t, 3, false)
	now := time.Unix(1, 0).UTC()
	if err := l.Label("T-A", false, false, 0, now, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Label("T-B", false, false, 0, now, nil); err != nil {
		t.Fatal(err) // T-B's slot is left loaded
	}
	err := l.Label("T-A", true, false, 0, now, nil)
	if err == nil || !strings.Contains(err.Error(), `"T-A" already exists`) {
		t.Fatalf("relabeling T-B as T-A should refuse the duplicate, got: %v", err)
	}
	// In-place recycle of the loaded tape under its own name is the legitimate reuse.
	if err := l.Label("T-B", true, false, 0, now, nil); err != nil {
		t.Fatalf("in-place relabel of T-B should succeed: %v", err)
	}
	if lbl, ok, _ := l.vol.(media.Labeled).ReadLabel(); !ok || lbl.Name != "T-B" || lbl.Epoch != 2 {
		t.Fatalf("in-place relabel should bump the epoch, got %+v ok=%v", lbl, ok)
	}
}

// TestFailedLabelRestoresLoadedSlot locks the drive state across a failed `nb label`:
// the slot scan borrows the drive to read labels, and used to leave whatever slot it
// scanned last in the drive — so a following `nb label --relabel` (which acts on the
// loaded tape) wiped a tape the operator never chose. A failed label must leave the
// drive exactly as the operator set it (loaded tape, or empty).
func TestFailedLabelRestoresLoadedSlot(t *testing.T) {
	l, _, ch := newTapeLib(t, 3, false)
	now := time.Unix(1, 0).UTC()
	for _, name := range []string{"OLD-1", "OLD-2", "OLD-3"} {
		if err := l.Label(name, false, false, 0, now, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Load("OLD-1", true, nil); err != nil {
		t.Fatal(err)
	}
	orig := drive0(t, ch).FromSlot

	err := l.Label("NEW-X", false, false, 0, now, nil)
	if err == nil || !strings.Contains(err.Error(), "no blank slot") {
		t.Fatalf("labeling with no blank slot should fail, got: %v", err)
	}
	if got := drive0(t, ch); !got.Loaded || got.FromSlot != orig {
		t.Fatalf("failed label moved the drive: loaded=%v slot=%d, want slot %d", got.Loaded, got.FromSlot, orig)
	}
	if name, labeled, _ := l.readVolumeLabel(); !labeled || name != "OLD-1" {
		t.Fatalf("drive should still hold OLD-1, got %q (labeled=%v)", name, labeled)
	}

	// An originally-empty drive must be empty again after a failed scan.
	if err := ch.Unload(0); err != nil {
		t.Fatal(err)
	}
	err = l.Label("NEW-Y", false, false, 0, now, nil)
	if err == nil {
		t.Fatal("labeling with no blank slot should fail")
	}
	if got := drive0(t, ch); got.Loaded {
		t.Fatalf("failed label left slot %d in an originally-empty drive", got.FromSlot)
	}
}

// TestAdvancePrefersLabeledEmptyOverBlank locks the roll's selection order: a volume
// roll spends the pool's labeled, writable volumes before consuming a blank reel —
// even with auto_label on, where the blank would otherwise verify writable first by
// slot order.
func TestAdvancePrefersLabeledEmptyOverBlank(t *testing.T) {
	l, cat, ch := newTapeLib(t, 3, true) // auto_label ON: the blank IS usable, but must lose
	now := time.Unix(1, 0).UTC()
	stampSlot(t, l, ch, cat, 3, "T-2", now) // labeled empty, in a LATER slot than the blank
	stampSlot(t, l, ch, cat, 1, "T-1", now) // the tape that "filled"; left loaded

	name, _, empty, err := l.Advance(true, map[string]bool{}, "", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if name != "T-2" {
		t.Fatalf("roll should prefer labeled-empty T-2 over the blank in slot 2, got %q", name)
	}
	if !empty {
		t.Fatal("T-2 holds no runs; wasEmpty should be true")
	}
}

// TestFailedAdvanceNeverWritesUnverified locks the poison fix: a failed roll can leave
// an unverified cartridge (the scan's last blank) in the drive, and the sink used to
// hand that reel out to the next archive — stamping archive data onto an unlabeled
// tape (flagging it foreign forever) while the placement claimed the old volume. After
// a failed roll the sink must re-run the label check and refuse, leaving the reel blank.
func TestFailedAdvanceNeverWritesUnverified(t *testing.T) {
	l, cat, ch := newTapeLib(t, 3, false) // auto_label off; slots 2+3 stay blank
	now := time.Unix(1, 0).UTC()
	stampSlot(t, l, ch, cat, 1, "T-1", now) // the run's current tape, loaded

	sink := l.Allocator("T-1", 1, true, 0, now, nil)
	if err := sink.advance(); err == nil {
		t.Fatal("advance with only blank reels (auto_label off) should fail")
	}
	// The failed roll leaves a blank in the drive; the sink must not hand it out.
	if _, _, _, _, err := sink.NextPart(); err == nil {
		t.Fatal("NextPart after a failed roll must re-verify the drive, not write to an unverified reel")
	}
	files, err := l.driveVol().Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("the blank reel was written to (%d file(s)) — poisoned as foreign", len(files))
	}
}

// TestViewReportsSlotLabels locks the inventory's per-slot label report: labeling a
// cartridge teaches the catalog its barcode↔label pairing, so `nb medium` names the
// slot's volume from that learned memory (never a peek — a real library reads a label
// only once the cartridge is in a drive). A never-loaded cartridge stays unknown.
func TestViewReportsSlotLabels(t *testing.T) {
	l, _, ch := newTapeLib(t, 3, false)
	now := time.Unix(1, 0).UTC()
	if err := ch.Load(1, 0); err != nil { // a loaded blank is the label target
		t.Fatal(err)
	}
	if err := l.Label("T-A", false, false, 0, now, nil); err != nil {
		t.Fatal(err)
	}
	if err := ch.Unload(0); err != nil { // return the cartridge so slot 1 is occupied
		t.Fatal(err)
	}
	v, err := l.View()
	if err != nil {
		t.Fatal(err)
	}
	if got := v.SlotLabels[1]; got != "T-A" {
		t.Fatalf("slot 1 should report learned label T-A, got %q", got)
	}
	for _, s := range []int{2, 3} {
		if got, ok := v.SlotLabels[s]; ok {
			t.Fatalf("slot %d was never loaded, so its label is unknowable; got %q", s, got)
		}
	}
}

// TestBlankNeedsLabelMatchesThroughReloadable locks the wrapping that lets the
// single-drive spanning loop fail fast: the blank-reel error must read as BOTH
// reloadable (so other callers still treat it as swap-eligible) AND match
// errBlankNeedsLabel via errors.Is (so advanceViaShelf stops re-prompting). It
// relies on reloadable.Unwrap; without it errors.Is cannot descend.
func TestBlankNeedsLabelMatchesThroughReloadable(t *testing.T) {
	blank := reloadable{fmt.Errorf("medium %q has a blank/unlabeled reel loaded: %w", "desk", errBlankNeedsLabel)}
	if !isReloadable(blank) {
		t.Error("blank-reel error should be reloadable")
	}
	if !errors.Is(blank, errBlankNeedsLabel) {
		t.Error("blank-reel error should match errBlankNeedsLabel via errors.Is")
	}

	// A different reloadable reason (wrong pool, still-protected, …) must NOT match
	// the sentinel — those still loop and re-prompt for another reel.
	other := reloadableErr("mounted volume belongs to the wrong pool")
	if !isReloadable(other) {
		t.Error("other reason should still be reloadable")
	}
	if errors.Is(other, errBlankNeedsLabel) {
		t.Error("a non-blank reloadable reason must not match errBlankNeedsLabel")
	}
}
