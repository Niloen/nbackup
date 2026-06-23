package gnutar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/method"
)

func newMethod(t *testing.T) method.Method {
	t.Helper()
	m, err := method.Open("gnutar", method.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Check(); err != nil {
		t.Skipf("GNU tar not available: %v", err)
	}
	return m
}

// TestBackupRestoreWithDeletion verifies a full + incremental chain reproduces
// the live tree, including a modified file, a new file, and a DELETED file that
// GNU tar's listed-incremental restore must remove. The raw tar stream is used
// directly (no compression) to test the method in isolation.
func TestBackupRestoreWithDeletion(t *testing.T) {
	m := newMethod(t)
	src := t.TempDir()
	snaps := t.TempDir()
	out := t.TempDir()

	write(t, filepath.Join(src, "a.txt"), "alpha")
	write(t, filepath.Join(src, "b.txt"), "beta")
	write(t, filepath.Join(src, "sub", "c.txt"), "gamma")

	l0 := filepath.Join(out, "l0.tar")
	backup(t, m, method.BackupRequest{SourcePath: src, Level: 0, OutSnap: filepath.Join(snaps, "L0.snar")}, l0)

	time.Sleep(1100 * time.Millisecond) // 1s mtime granularity
	write(t, filepath.Join(src, "a.txt"), "alpha-CHANGED")
	if err := os.Remove(filepath.Join(src, "b.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "d.txt"), "delta")

	l1 := filepath.Join(out, "l1.tar")
	backup(t, m, method.BackupRequest{
		SourcePath: src, Level: 1,
		BaseSnap: filepath.Join(snaps, "L0.snar"),
		OutSnap:  filepath.Join(snaps, "L1.snar"),
	}, l1)

	dest := t.TempDir()
	restore(t, m, l0, dest)
	restore(t, m, l1, dest)

	assertContent(t, filepath.Join(dest, "a.txt"), "alpha-CHANGED")
	assertContent(t, filepath.Join(dest, "sub", "c.txt"), "gamma")
	assertContent(t, filepath.Join(dest, "d.txt"), "delta")
	if _, err := os.Stat(filepath.Join(dest, "b.txt")); !os.IsNotExist(err) {
		t.Errorf("b.txt should have been deleted on restore, stat err = %v", err)
	}
}

// TestExcludeOption verifies a dumptype option flows through to tar.
func TestExcludeOption(t *testing.T) {
	if _, err := method.Open("gnutar", method.Options{}); err != nil {
		t.Fatal(err)
	}
	m, err := method.Open("gnutar", method.Options{"exclude": "*.log"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Check(); err != nil {
		t.Skipf("GNU tar not available: %v", err)
	}
	src := t.TempDir()
	snaps := t.TempDir()
	out := t.TempDir()
	write(t, filepath.Join(src, "keep.txt"), "keep")
	write(t, filepath.Join(src, "drop.log"), "drop")

	l0 := filepath.Join(out, "l0.tar")
	backup(t, m, method.BackupRequest{SourcePath: src, Level: 0, OutSnap: filepath.Join(snaps, "L0.snar")}, l0)

	dest := t.TempDir()
	restore(t, m, l0, dest)
	if _, err := os.Stat(filepath.Join(dest, "keep.txt")); err != nil {
		t.Errorf("keep.txt should be present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "drop.log")); !os.IsNotExist(err) {
		t.Errorf("drop.log should have been excluded, stat err = %v", err)
	}
}

// TestEstimate checks the /dev/null client estimate: the full reflects the data
// size, excludes lower it, and an unchanged incremental is far smaller than a full.
func TestEstimate(t *testing.T) {
	m := newMethod(t)
	src := t.TempDir()
	snaps := t.TempDir()

	write(t, filepath.Join(src, "big.bin"), strings.Repeat("x", 200000))
	write(t, filepath.Join(src, "small.txt"), "hi")

	full, err := m.Estimate(method.BackupRequest{SourcePath: src, Level: 0})
	if err != nil {
		t.Fatal(err)
	}
	if full < 200000 {
		t.Errorf("full estimate %d should be >= the 200000-byte file", full)
	}

	// Excluding the big file yields a much smaller estimate.
	me, err := method.Open("gnutar", method.Options{"exclude": "*.bin"})
	if err != nil {
		t.Fatal(err)
	}
	excl, err := me.Estimate(method.BackupRequest{SourcePath: src, Level: 0})
	if err != nil {
		t.Fatal(err)
	}
	if excl >= full {
		t.Errorf("excluded estimate %d should be < full estimate %d", excl, full)
	}

	// An unchanged incremental against a real snapshot estimates far below a full.
	time.Sleep(1100 * time.Millisecond) // snapshot time must beat file mtimes (1s granularity)
	l0snap := filepath.Join(snaps, "L0.snar")
	backup(t, m, method.BackupRequest{SourcePath: src, Level: 0, OutSnap: l0snap}, filepath.Join(t.TempDir(), "l0.tar"))
	incr, err := m.Estimate(method.BackupRequest{SourcePath: src, Level: 1, BaseSnap: l0snap})
	if err != nil {
		t.Fatal(err)
	}
	if incr >= full {
		t.Errorf("unchanged incremental estimate %d should be < full %d", incr, full)
	}
}

func backup(t *testing.T, m method.Method, req method.BackupRequest, outFile string) {
	t.Helper()
	f, err := os.Create(outFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := m.Backup(req, f); err != nil {
		t.Fatalf("backup L%d: %v", req.Level, err)
	}
}

func restore(t *testing.T, m method.Method, inFile, dest string) {
	t.Helper()
	f, err := os.Open(inFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := m.Restore(f, dest, nil); err != nil {
		t.Fatalf("restore %s: %v", inFile, err)
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
