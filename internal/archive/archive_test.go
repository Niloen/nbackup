package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGnuTarIncrementalRoundTrip verifies a full + incremental chain reproduces
// the live tree, including a modified file, a NEW file, and — crucially — a
// DELETED file, which GNU tar's listed-incremental restore must remove.
func TestGnuTarIncrementalRoundTrip(t *testing.T) {
	if err := CheckTar(""); err != nil {
		t.Skipf("GNU tar not available: %v", err)
	}
	src := t.TempDir()
	snaps := t.TempDir()
	out := t.TempDir()

	mustWrite(t, filepath.Join(src, "a.txt"), "alpha")
	mustWrite(t, filepath.Join(src, "b.txt"), "beta")
	mustWrite(t, filepath.Join(src, "sub", "c.txt"), "gamma")

	l0 := filepath.Join(out, "l0.tar.zst")
	r0, err := Create(CreateOptions{
		SourcePath:  src,
		OutFile:     l0,
		Level:       0,
		OutSnapshot: filepath.Join(snaps, "L0.snar"),
	})
	if err != nil {
		t.Fatalf("L0 create: %v", err)
	}
	if r0.FileCount != 3 {
		t.Fatalf("L0 file count = %d, want 3", r0.FileCount)
	}

	// listed-incremental compares mtime/ctime at 1s granularity; wait so the
	// modification is unambiguously newer than the L0 snapshot.
	time.Sleep(1100 * time.Millisecond)
	mustWrite(t, filepath.Join(src, "a.txt"), "alpha-CHANGED")
	mustRemove(t, filepath.Join(src, "b.txt"))
	mustWrite(t, filepath.Join(src, "d.txt"), "delta")

	l1 := filepath.Join(out, "l1.tar.zst")
	if _, err := Create(CreateOptions{
		SourcePath:   src,
		OutFile:      l1,
		Level:        1,
		BaseSnapshot: filepath.Join(snaps, "L0.snar"),
		OutSnapshot:  filepath.Join(snaps, "L1.snar"),
	}); err != nil {
		t.Fatalf("L1 create: %v", err)
	}

	dest := t.TempDir()
	if err := Extract("", l0, dest); err != nil {
		t.Fatalf("extract L0: %v", err)
	}
	if err := Extract("", l1, dest); err != nil {
		t.Fatalf("extract L1: %v", err)
	}

	assertContent(t, filepath.Join(dest, "a.txt"), "alpha-CHANGED")
	assertContent(t, filepath.Join(dest, "sub", "c.txt"), "gamma")
	assertContent(t, filepath.Join(dest, "d.txt"), "delta")
	if _, err := os.Stat(filepath.Join(dest, "b.txt")); !os.IsNotExist(err) {
		t.Errorf("b.txt should have been deleted on restore, stat err = %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
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
