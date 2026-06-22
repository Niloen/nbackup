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
