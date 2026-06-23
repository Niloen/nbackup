package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/slot"
)

// TestRunRestoreEndToEnd exercises the full engine over the disk store:
// full backup, incremental with a deletion, then a chain restore that must match
// the live tree.
func TestRunRestoreEndToEnd(t *testing.T) {
	src := t.TempDir()
	catalogDir := t.TempDir()

	write(t, filepath.Join(src, "keep.txt"), "v1")
	write(t, filepath.Join(src, "gone.txt"), "temp")

	cfg := &config.Config{
		Landing: "disk",
		Media:   map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": catalogDir}}},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(), // catalog state lives separately from the storage medium
	}
	cfg.Compress.Codec = "none" // exercise the pipeline without depending on a compressor binary

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	day1 := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(day1, nil); err != nil {
		t.Fatalf("day1 run: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)
	write(t, filepath.Join(src, "keep.txt"), "v2")
	if err := os.Remove(filepath.Join(src, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	day2 := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	s2, err := eng.Run(day2, nil)
	if err != nil {
		t.Fatalf("day2 run: %v", err)
	}
	if got := s2.Archives[0].Level; got != 1 {
		t.Fatalf("day2 should be L1, got L%d", got)
	}

	dest := t.TempDir()
	name := config.DLE{Host: "h", Path: src}.Name()
	if err := eng.Restore(s2.ID, name, dest, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertContent(t, filepath.Join(dest, "keep.txt"), "v2")
	if _, err := os.Stat(filepath.Join(dest, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt should be deleted after restore, stat err = %v", err)
	}
}

// TestParallelDumpers runs several DLEs with dumpers > 1, exercising concurrent
// writes into one slot, and verifies every archive is present and restorable.
func TestParallelDumpers(t *testing.T) {
	catalogDir := t.TempDir()
	cfg := &config.Config{
		Landing: "disk",
		Media:   map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": catalogDir}}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none" // no compressor-binary dependency in tests
	cfg.Parallelism.Dumpers = 3

	names := []string{"alpha", "bravo", "charlie", "delta"}
	for _, n := range names {
		dir := t.TempDir()
		write(t, filepath.Join(dir, n+".txt"), "content-"+n)
		cfg.Sources = append(cfg.Sources, config.DLE{Host: "h", Path: dir})
	}

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s, err := eng.Run(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("parallel run: %v", err)
	}
	if len(s.Archives) != len(cfg.Sources) {
		t.Fatalf("expected %d archives, got %d", len(cfg.Sources), len(s.Archives))
	}

	// Each DLE restores to its original content.
	for i, d := range cfg.Sources {
		dest := t.TempDir()
		if err := eng.Restore(s.ID, d.Name(), dest, nil); err != nil {
			t.Fatalf("restore %s: %v", d.Name(), err)
		}
		assertContent(t, filepath.Join(dest, names[i]+".txt"), "content-"+names[i])
	}
}

// TestCopyToTapeAndRestore dumps to disk, copies the slot to a (virtual) tape
// medium, then restores it from the tape alone — exercising CopySlot and a tape
// Volume end to end.
func TestCopyToTapeAndRestore(t *testing.T) {
	src := t.TempDir()
	diskDir := t.TempDir()
	tapeDir := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "copy me to tape")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": diskDir}},
			"tape": {Type: "tape", Params: map[string]string{"dir": tapeDir}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	day := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	s, err := eng.Run(day, nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	// Copy is a write, so the tape must be labeled first.
	if err := eng.LabelVolume("tape", "tape-0001", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label tape: %v", err)
	}
	if err := eng.CopySlot(s.ID, "", "tape", false, nil); err != nil {
		t.Fatalf("copy to tape: %v", err)
	}

	// Restore from the tape alone: a fresh engine landed on the tape rebuilds its
	// catalog from the volume, then restores.
	tcfg := &config.Config{
		Landing: "tape",
		Media:   map[string]config.Media{"tape": {Type: "tape", Params: map[string]string{"dir": tapeDir}}},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(), // separate catalog cache, forcing a rebuild from tape
	}
	tcfg.Compress.Codec = "none"
	teng, err := New(tcfg)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := teng.RebuildCatalog(nil); err != nil || n != 1 {
		t.Fatalf("rebuild from tape: n=%d err=%v", n, err)
	}
	dest := t.TempDir()
	name := config.DLE{Host: "h", Path: src}.Name()
	if err := teng.Restore(s.ID, name, dest, nil); err != nil {
		t.Fatalf("restore from tape: %v", err)
	}
	assertContent(t, filepath.Join(dest, "f.txt"), "copy me to tape")
}

// TestTapeLabelVerify exercises the label protocol on a tape landing: a dump is
// refused on a blank tape, succeeds after `nb label`, and is refused when the
// catalog expects a different label than the one mounted (a swapped tape).
func TestTapeLabelVerify(t *testing.T) {
	src := t.TempDir()
	tapeDir := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "data")

	cfg := &config.Config{
		Landing: "lto",
		Media:   map[string]config.Media{"lto": {Type: "tape", Params: map[string]string{"dir": tapeDir}}},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	day := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)

	// Blank tape: dump refused.
	if _, err := eng.Run(day, nil); err == nil {
		t.Fatal("expected dump to be refused on a blank/unlabeled tape")
	}

	// Label it, then a dump succeeds.
	if err := eng.LabelVolume("lto", "lto-0001", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label: %v", err)
	}
	if _, err := eng.Run(day, nil); err != nil {
		t.Fatalf("dump after label: %v", err)
	}

	// Out-of-band relabel of the loaded tape (same name, bumped epoch) makes the
	// catalog stale for it; a dump must refuse until `nb rebuild`. (Loading
	// a genuinely different tape from the pool is not an error under a changer.)
	lv := eng.vol.(media.Labeled)
	if err := lv.WriteLabel(media.Label{Name: "lto-0001", Pool: "lto", Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	day2 := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(day2, nil); err == nil {
		t.Fatal("expected dump to be refused when the mounted tape was relabeled since the catalog was updated")
	}
}

// TestCopyRecordsPlacementAndFailover dumps to disk, copies to a second medium,
// confirms the slot now has two placements, then physically removes the primary
// copy and restores — proving restore falls over to the recorded copy.
func TestCopyRecordsPlacementAndFailover(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "two homes")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"archive": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := eng.CopySlot(s.ID, "", "archive", false, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}

	// A second copy to the same medium is refused (idempotent) unless forced.
	if err := eng.CopySlot(s.ID, "", "archive", false, nil); err == nil {
		t.Fatal("expected re-copy to the same medium to be refused without --force")
	}
	if err := eng.CopySlot(s.ID, "", "archive", true, nil); err != nil {
		t.Fatalf("forced re-copy: %v", err)
	}

	if got := len(eng.cat.Placements(s.ID)); got != 2 {
		t.Fatalf("expected 2 placements after copy, got %d", got)
	}
	if eng.cat.MediumBytes("archive") == 0 {
		t.Errorf("archive medium should report stored bytes")
	}

	// Physically remove the primary copy but leave its placement recorded: restore
	// must try it, fail, and fall over to the archive copy.
	if err := eng.vol.RemoveSlot(s.ID); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	name := config.DLE{Host: "h", Path: src}.Name()
	if err := eng.Restore(s.ID, name, dest, nil); err != nil {
		t.Fatalf("restore (failover to copy): %v", err)
	}
	assertContent(t, filepath.Join(dest, "f.txt"), "two homes")
}

// TestRunWritesStatus confirms a dump leaves a terminal run-status file in the
// catalog workdir, reflecting the sealed slot — the input `nb status` reads.
func TestRunWritesStatus(t *testing.T) {
	src := t.TempDir()
	workdir := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "status me")

	cfg := &config.Config{
		Landing: "disk",
		Media:   map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: workdir,
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}

	snap, err := progress.Load(workdir)
	if err != nil {
		t.Fatalf("load status: %v", err)
	}
	if snap.SlotID != s.ID {
		t.Errorf("status slot = %q, want %q", snap.SlotID, s.ID)
	}
	if snap.Phase != progress.PhaseDone {
		t.Errorf("status phase = %s, want done", snap.Phase)
	}
	if _, done, failed, _ := snap.Counts(); done != 1 || failed != 0 {
		t.Errorf("counts done=%d failed=%d, want 1/0", done, failed)
	}
	if snap.DLEs[0].DoneBytes == 0 {
		t.Error("status should record archived bytes")
	}
}

func boolp(b bool) *bool { return &b }

// TestTapeLibraryRestore copies two slots onto two different tapes in a library,
// removes the disk copies, then restores both — proving the changer auto-mounts
// the bay holding each slot's tape on the read side.
func TestTapeLibraryRestore(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "v1")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lib":  {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "2"}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s1, err := eng.Run(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	if err := eng.LabelVolume("lib", "Tape1", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Tape1: %v", err)
	}
	if err := eng.CopySlot(s1.ID, "", "lib", false, nil); err != nil {
		t.Fatalf("copy s1: %v", err)
	}

	write(t, filepath.Join(src, "f.txt"), "v2")
	time.Sleep(1100 * time.Millisecond)
	s2, err := eng.Run(time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}
	if err := eng.LabelVolume("lib", "Tape2", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Tape2: %v", err)
	}
	if err := eng.CopySlot(s2.ID, "", "lib", false, nil); err != nil {
		t.Fatalf("copy s2: %v", err)
	}

	// The two copies must live on different tapes.
	if v := eng.cat.Placements(s1.ID); len(v) < 2 {
		t.Fatalf("s1 should have a tape copy, placements=%v", v)
	}

	// Drop the disk copies so restore must fall over to the tapes (different bays).
	if err := eng.vol.RemoveSlot(s1.ID); err != nil {
		t.Fatal(err)
	}
	if err := eng.vol.RemoveSlot(s2.ID); err != nil {
		t.Fatal(err)
	}

	name := config.DLE{Host: "h", Path: src}.Name()
	d1 := t.TempDir()
	if err := eng.Restore(s1.ID, name, d1, nil); err != nil {
		t.Fatalf("restore s1 (auto-mount Tape1): %v", err)
	}
	assertContent(t, filepath.Join(d1, "f.txt"), "v1")
	d2 := t.TempDir()
	if err := eng.Restore(s2.ID, name, d2, nil); err != nil {
		t.Fatalf("restore s2 (auto-mount Tape2): %v", err)
	}
	assertContent(t, filepath.Join(d2, "f.txt"), "v2")
}

// TestTapeAppendableFalse: a one-run-per-tape medium refuses a second run on a
// tape that already holds one; a fresh tape accepts it.
func TestTapeAppendableFalse(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "data")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lib":  {Type: "tape", Appendable: boolp(false), Params: map[string]string{"dir": t.TempDir(), "bays": "2"}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s1, err := eng.Run(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	write(t, filepath.Join(src, "f.txt"), "data2")
	time.Sleep(1100 * time.Millisecond)
	s2, err := eng.Run(time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}

	if err := eng.LabelVolume("lib", "Tape1", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Tape1: %v", err)
	}
	if err := eng.CopySlot(s1.ID, "", "lib", false, nil); err != nil {
		t.Fatalf("copy s1 to fresh tape: %v", err)
	}
	// Tape1 now holds a run; a non-appendable medium refuses a second run on it.
	if err := eng.CopySlot(s2.ID, "", "lib", false, nil); err == nil {
		t.Fatal("expected copy onto a non-appendable tape that already holds a run to be refused")
	}
	// A fresh tape accepts it.
	if err := eng.LabelVolume("lib", "Tape2", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Tape2: %v", err)
	}
	if err := eng.CopySlot(s2.ID, "", "lib", false, nil); err != nil {
		t.Fatalf("copy s2 to fresh tape: %v", err)
	}
}

// scriptedOperator stands in for a human at a single-drive station: it loads the
// reel the engine asks for (the needed label on a read, any blank reel on a
// write) and counts how many swaps it performed.
type scriptedOperator struct{ swaps int }

func (o *scriptedOperator) Swap(r librarian.SwapRequest) (string, bool) {
	o.swaps++
	if r.Need != "" { // a read wants a specific label
		for _, b := range r.Shelf {
			if b.Label == r.Need {
				return b.ID, true
			}
		}
		return "", false
	}
	for _, b := range r.Shelf { // a write wants any writable (blank) reel
		if b.Blank {
			return b.ID, true
		}
	}
	return "", false
}

// TestManualStationWriteSwap: a copy to a single-drive station with an empty drive
// prompts the operator to load a reel; with auto_label on, the freshly loaded blank
// reel is labeled and the copy proceeds — the write-side swap path.
func TestManualStationWriteSwap(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "manual write")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lto":  {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "1"}},
		},
		Sources:   []config.DLE{{Host: "h", Path: src}},
		Workdir:   t.TempDir(),
		AutoLabel: true,
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	op := &scriptedOperator{}
	eng.SetOperator(op)

	s, err := eng.Run(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	// The drive is empty: the copy must prompt for a reel, then auto-label it.
	if err := eng.CopySlot(s.ID, "", "lto", false, logfDiscard); err != nil {
		t.Fatalf("copy to manual station: %v", err)
	}
	if op.swaps == 0 {
		t.Fatal("expected the operator to be prompted to load a reel")
	}
	found := false
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "lto" {
			found = true
		}
	}
	if !found {
		t.Fatal("slot should have a placement on the manual station")
	}
}

// TestManualStationReadSwap: two slots land on two reels of a single-drive station;
// with the disk copies gone, restoring each prompts the operator to swap the reel
// holding it into the one drive — the read-side swap path.
func TestManualStationReadSwap(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "v1")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lto":  {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "2"}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s1, err := eng.Run(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	// Reel A: load it, label it, copy s1 (the loaded reel is usable — no swap).
	if err := eng.LoadVolume("lto", "reel-01", false, logfDiscard); err != nil {
		t.Fatalf("load reel-01: %v", err)
	}
	if err := eng.LabelVolume("lto", "Reel-A", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Reel-A: %v", err)
	}
	if err := eng.CopySlot(s1.ID, "", "lto", false, logfDiscard); err != nil {
		t.Fatalf("copy s1: %v", err)
	}

	write(t, filepath.Join(src, "f.txt"), "v2")
	time.Sleep(1100 * time.Millisecond)
	s2, err := eng.Run(time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}
	// Reel B: load (swaps A out of the one drive), label, copy s2.
	if err := eng.LoadVolume("lto", "reel-02", false, logfDiscard); err != nil {
		t.Fatalf("load reel-02: %v", err)
	}
	if err := eng.LabelVolume("lto", "Reel-B", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Reel-B: %v", err)
	}
	if err := eng.CopySlot(s2.ID, "", "lto", false, logfDiscard); err != nil {
		t.Fatalf("copy s2: %v", err)
	}

	// Drop the disk copies so restore must read from the reels.
	if err := eng.vol.RemoveSlot(s1.ID); err != nil {
		t.Fatal(err)
	}
	if err := eng.vol.RemoveSlot(s2.ID); err != nil {
		t.Fatal(err)
	}

	op := &scriptedOperator{}
	eng.SetOperator(op)

	name := config.DLE{Host: "h", Path: src}.Name()
	// The drive holds Reel-B; restoring s1 must prompt to swap in Reel-A.
	d1 := t.TempDir()
	if err := eng.Restore(s1.ID, name, d1, logfDiscard); err != nil {
		t.Fatalf("restore s1 (swap in Reel-A): %v", err)
	}
	assertContent(t, filepath.Join(d1, "f.txt"), "v1")
	// And s2 must prompt to swap Reel-B back in.
	d2 := t.TempDir()
	if err := eng.Restore(s2.ID, name, d2, logfDiscard); err != nil {
		t.Fatalf("restore s2 (swap in Reel-B): %v", err)
	}
	assertContent(t, filepath.Join(d2, "f.txt"), "v2")

	if op.swaps == 0 {
		t.Fatal("expected the operator to be prompted to swap reels on read")
	}
}

// TestManualStationLandingLabel: labeling the engine's own (landing) single-drive
// station rebuilds its catalog against the freshly-labeled reel. Regression for the
// catalog rebuild treating a Station like a robotic Library — iterating bays and
// mounting a bay id, which a single-drive station has none of.
func TestManualStationLandingLabel(t *testing.T) {
	cfg := &config.Config{
		Landing: "vtape",
		Media: map[string]config.Media{
			"vtape": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "3"}},
		},
		Sources: []config.DLE{{Host: "h", Path: t.TempDir()}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A reel must be in the drive to label it.
	if err := eng.LoadVolume("vtape", "reel-01", false, logfDiscard); err != nil {
		t.Fatalf("load reel-01: %v", err)
	}
	// Labeling the landing medium triggers a catalog rebuild against the loaded reel;
	// it must not try to bay-iterate a single-drive station.
	if err := eng.LabelVolume("vtape", "Label1", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label landing manual station: %v", err)
	}
	if known, ok := eng.cat.Volume("Label1"); !ok || known.Label.Epoch != 1 {
		t.Fatalf("catalog should record Label1 at epoch 1 after rebuild (ok=%v)", ok)
	}
}

// tapeEngine builds an engine over a single-drive (manual) tape landing medium
// with the given appendability and minimum age, with no slots or volumes yet.
func tapeEngine(t *testing.T, appendable bool, minAge string) *Engine {
	t.Helper()
	cfg := &config.Config{
		Landing: "lto",
		Media: map[string]config.Media{
			"lto": {
				Type:       "tape",
				MinimumAge: minAge,
				Appendable: &appendable,
				Params:     map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "4"},
			},
		},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// recordVol registers a labeled volume in the catalog at a written-at time.
func recordVol(t *testing.T, eng *Engine, name string, writtenAt time.Time) {
	t.Helper()
	if err := eng.cat.RecordVolume(media.Label{Name: name, Pool: "lto", Epoch: 1, WrittenAt: writtenAt}); err != nil {
		t.Fatal(err)
	}
}

// recordFullOn records a sealed full of one DLE on a given volume.
func recordFullOn(t *testing.T, eng *Engine, date, dle, volume string) {
	t.Helper()
	recordSizedFullOn(t, eng, date, dle, volume, 0)
}

// recordSizedFullOn records a sealed full of one DLE on a volume with a given
// payload size, so a reel's fill can be asserted.
func recordSizedFullOn(t *testing.T, eng *Engine, date, dle, volume string, bytes int64) {
	t.Helper()
	id := slot.IDFromParts(date, 1)
	s := slot.NewSlot(id, date, 1, "test", time.Now())
	s.AddArchive(slot.Archive{DLE: dle, Level: 0, Compressed: bytes})
	if err := s.Seal(time.Now()); err != nil {
		t.Fatal(err)
	}
	p := catalog.Placement{
		Medium:   "lto",
		Archives: []catalog.ArchivePos{{DLE: dle, Level: 0, Parts: []catalog.PartPos{{Volume: volume, Epoch: 1, Pos: 1}}}},
		Seal:     catalog.PartPos{Volume: volume, Epoch: 1, Pos: 2},
	}
	if err := eng.cat.Record(s, p); err != nil {
		t.Fatal(err)
	}
}

// TestExpectedTapeReusesOldest: on a one-run-per-tape medium the next run expects
// the oldest volume whose runs are all reusable (past minimum age with a newer
// recovery path) — Amanda's taper picking the oldest reusable tape.
func TestExpectedTapeReusesOldest(t *testing.T) {
	eng := tapeEngine(t, false, "10d")
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)

	recordVol(t, eng, "lto-0001", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	recordVol(t, eng, "lto-0002", time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))
	recordFullOn(t, eng, "2026-06-01", "h", "lto-0001") // old, superseded -> reusable
	recordFullOn(t, eng, "2026-06-20", "h", "lto-0002") // recent full -> protected

	exp, ok := eng.ExpectedTape(now)
	if !ok {
		t.Fatal("a labeled medium should yield an expectation")
	}
	if exp.NewTape || exp.Label != "lto-0001" {
		t.Fatalf("want oldest reusable lto-0001, got %+v", exp)
	}
	if exp.Recycles != 1 {
		t.Fatalf("want 1 run recycled, got %d", exp.Recycles)
	}
}

// TestExpectedTapeNeedsFresh: when every volume still holds a protected run, the
// run expects a fresh tape rather than recycling a protected one.
func TestExpectedTapeNeedsFresh(t *testing.T) {
	eng := tapeEngine(t, false, "10d")
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)

	recordVol(t, eng, "lto-0001", time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))
	recordFullOn(t, eng, "2026-06-20", "h", "lto-0001") // within minimum age -> protected

	exp, ok := eng.ExpectedTape(now)
	if !ok {
		t.Fatal("a labeled medium should yield an expectation")
	}
	if !exp.NewTape || exp.Label != "" {
		t.Fatalf("want a fresh tape, got %+v", exp)
	}
}

// TestExpectedTapeAppendsToLatest: an appendable medium extends the most recently
// written volume rather than recycling an old one.
func TestExpectedTapeAppendsToLatest(t *testing.T) {
	eng := tapeEngine(t, true, "")
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)

	recordVol(t, eng, "lto-0001", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	recordVol(t, eng, "lto-0002", time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))

	exp, ok := eng.ExpectedTape(now)
	if !ok {
		t.Fatal("a labeled medium should yield an expectation")
	}
	if exp.NewTape || exp.Label != "lto-0002" {
		t.Fatalf("want to append to latest lto-0002, got %+v", exp)
	}
}

// TestExpectedTapeReportsReelFill: an appendable run's expectation carries the
// landing reel's capacity (volume_size) and current fill, so a single run is
// bounded by the reel's remaining room (not the whole pool) and `nb plan` can
// show how full the tape is before it spills.
func TestExpectedTapeReportsReelFill(t *testing.T) {
	appendable := true
	cfg := &config.Config{
		Landing: "lto",
		Media: map[string]config.Media{
			"lto": {
				Type:       "tape",
				Appendable: &appendable,
				Params:     map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "2", "volume_size": "1000"},
			},
		},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)

	recordVol(t, eng, "lto-0001", time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))
	recordSizedFullOn(t, eng, "2026-06-20", "h", "lto-0001", 600)

	exp, ok := eng.ExpectedTape(now)
	if !ok {
		t.Fatal("a labeled medium should yield an expectation")
	}
	if exp.Label != "lto-0001" {
		t.Fatalf("want append to lto-0001, got %+v", exp)
	}
	if exp.VolumeBytes != 1000 || exp.UsedBytes != 600 {
		t.Fatalf("want a 1000-byte reel with 600 used, got VolumeBytes=%d UsedBytes=%d", exp.VolumeBytes, exp.UsedBytes)
	}
}

// TestExpectedTapeDiskHasNone: an address-identified medium (disk) carries no
// label, so there is no tape to expect.
func TestExpectedTapeDiskHasNone(t *testing.T) {
	cfg := &config.Config{
		Landing: "disk",
		Media:   map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := eng.ExpectedTape(time.Now()); ok {
		t.Fatal("disk medium should not yield a tape expectation")
	}
}

func logfDiscard(string, ...any) {}

// TestDumpSpansArchiveAcrossTapes dumps a source larger than one tape directly onto
// a tape library, so a single DLE's archive must split into parts across several
// auto-labeled bays. It then verifies the slot and restores it, exercising the read
// path mounting each bay in sequence to reassemble the spanned archive.
func TestDumpSpansArchiveAcrossTapes(t *testing.T) {
	src := t.TempDir()
	// ~150 KiB in one file → one archive larger than a single 160 KiB tape (each tape
	// also spends a 32 KiB header on its label and on each part), so it must span.
	body := strings.Repeat("nbackup-spanning-", 9*1024)
	write(t, filepath.Join(src, "big.txt"), body)

	cfg := &config.Config{
		Landing:   "lib",
		AutoLabel: true,
		Media: map[string]config.Media{
			"lib": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "6", "volume_size": "163840"}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	// Seed a blank bay in the drive so the run has somewhere to start; it auto-labels
	// and rolls onto the rest as each fills.
	if err := eng.LoadVolume("lib", "bay-01", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}

	s, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}

	// The single archive must have split into several parts.
	if len(s.Archives) != 1 {
		t.Fatalf("want 1 archive, got %d", len(s.Archives))
	}
	if s.Archives[0].Parts < 2 {
		t.Fatalf("archive Parts = %d, want >= 2 (the dump must span tapes)", s.Archives[0].Parts)
	}
	// The copy must span more than one volume.
	ps := eng.cat.Placements(s.ID)
	if len(ps) != 1 {
		t.Fatalf("placements = %d, want 1", len(ps))
	}
	if vols := ps[0].Volumes(); len(vols) < 2 {
		t.Fatalf("placement spans %v, want >= 2 volumes", vols)
	}

	// Verify reassembles and re-hashes the spanned archive across its tapes.
	if failures, err := eng.Verify([]string{s.ID}, nil); err != nil || failures != 0 {
		t.Fatalf("verify: failures=%d err=%v", failures, err)
	}

	// Restore must mount each bay in sequence to rebuild the original file.
	dest := t.TempDir()
	name := config.DLE{Host: "h", Path: src}.Name()
	if err := eng.Restore(s.ID, name, dest, nil); err != nil {
		t.Fatalf("restore from spanned tapes: %v", err)
	}
	assertContent(t, filepath.Join(dest, "big.txt"), body)
}

// TestCopySpansArchiveAcrossTapes dumps one big archive to disk, then copies it to a
// small-tape library where the single archive must split across bays (re-splitting
// the already-compressed payload, not recompressing). It drops the disk copy and
// restores from the spanned tapes.
func TestCopySpansArchiveAcrossTapes(t *testing.T) {
	src := t.TempDir()
	body := strings.Repeat("copy-spanning-payload-", 7*1024)
	write(t, filepath.Join(src, "big.txt"), body)

	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true,
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lib":  {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "6", "volume_size": "163840"}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := eng.LoadVolume("lib", "bay-01", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}
	if err := eng.CopySlot(s.ID, "", "lib", false, nil); err != nil {
		t.Fatalf("copy disk->lib: %v", err)
	}

	var tape catalog.Placement
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "lib" {
			tape = p
		}
	}
	if parts, _ := tape.Parts(config.DLE{Host: "h", Path: src}.Name(), 0); len(parts) < 2 {
		t.Fatalf("copied archive parts = %d, want >= 2 (must span)", len(parts))
	}

	// Drop the disk copy so restore must read the spanned tape copy.
	if err := eng.vol.RemoveSlot(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.cat.RemovePlacement(s.ID, "disk"); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	name := config.DLE{Host: "h", Path: src}.Name()
	if err := eng.Restore(s.ID, name, dest, nil); err != nil {
		t.Fatalf("restore from spanned tape copy: %v", err)
	}
	assertContent(t, filepath.Join(dest, "big.txt"), body)
}

// TestPartSizeSplitsWithinTape sets a small part_size on a roomy tape: the archive is
// chopped into several parts that all stay on the one tape (intra-volume splitting —
// the real-drive path where capacity is bounded by part_size, not a bay size). It
// must still verify and restore.
func TestPartSizeSplitsWithinTape(t *testing.T) {
	src := t.TempDir()
	body := strings.Repeat("part-size-", 12*1024) // ~120 KiB
	write(t, filepath.Join(src, "big.txt"), body)

	cfg := &config.Config{
		Landing:   "lib",
		AutoLabel: true,
		Media: map[string]config.Media{
			// One roomy 4 MiB bay, but part_size caps each part at 64 KiB.
			"lib": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "1", "volume_size": "4194304", "part_size": "65536"}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	if err := eng.LoadVolume("lib", "bay-01", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}
	s, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if s.Archives[0].Parts < 2 {
		t.Fatalf("archive Parts = %d, want >= 2 (part_size must split it)", s.Archives[0].Parts)
	}
	if vols := eng.cat.Placements(s.ID)[0].Volumes(); len(vols) != 1 {
		t.Fatalf("parts should stay on one tape, got volumes %v", vols)
	}
	if failures, err := eng.Verify([]string{s.ID}, nil); err != nil || failures != 0 {
		t.Fatalf("verify: failures=%d err=%v", failures, err)
	}
	dest := t.TempDir()
	if err := eng.Restore(s.ID, config.DLE{Host: "h", Path: src}.Name(), dest, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertContent(t, filepath.Join(dest, "big.txt"), body)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}
