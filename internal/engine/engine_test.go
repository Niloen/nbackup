package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// TestRunRestoreEndToEnd exercises the full engine over the local-disk store:
// full backup, incremental with a deletion, then a chain restore that must match
// the live tree.
func TestRunRestoreEndToEnd(t *testing.T) {
	src := t.TempDir()
	catalogDir := t.TempDir()

	write(t, filepath.Join(src, "keep.txt"), "v1")
	write(t, filepath.Join(src, "gone.txt"), "temp")

	cfg := &config.Config{
		Landing: "disk",
		Media:   map[string]config.Media{"disk": {Type: "local-disk", Params: map[string]string{"path": catalogDir}}},
		Sources: []config.DLE{{Host: "h", Path: src}},
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
		Media:   map[string]config.Media{"disk": {Type: "local-disk", Params: map[string]string{"path": catalogDir}}},
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
