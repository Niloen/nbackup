package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
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
	if err := eng.CopySlot(s.ID, "tape", false, nil); err != nil {
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
	// catalog stale for it; a dump must refuse until `nb catalog rebuild`. (Loading
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
	if err := eng.CopySlot(s.ID, "archive", false, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}

	// A second copy to the same medium is refused (idempotent) unless forced.
	if err := eng.CopySlot(s.ID, "archive", false, nil); err == nil {
		t.Fatal("expected re-copy to the same medium to be refused without --force")
	}
	if err := eng.CopySlot(s.ID, "archive", true, nil); err != nil {
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
	if err := eng.CopySlot(s1.ID, "lib", false, nil); err != nil {
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
	if err := eng.CopySlot(s2.ID, "lib", false, nil); err != nil {
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
	if err := eng.CopySlot(s1.ID, "lib", false, nil); err != nil {
		t.Fatalf("copy s1 to fresh tape: %v", err)
	}
	// Tape1 now holds a run; a non-appendable medium refuses a second run on it.
	if err := eng.CopySlot(s2.ID, "lib", false, nil); err == nil {
		t.Fatal("expected copy onto a non-appendable tape that already holds a run to be refused")
	}
	// A fresh tape accepts it.
	if err := eng.LabelVolume("lib", "Tape2", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Tape2: %v", err)
	}
	if err := eng.CopySlot(s2.ID, "lib", false, nil); err != nil {
		t.Fatalf("copy s2 to fresh tape: %v", err)
	}
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
