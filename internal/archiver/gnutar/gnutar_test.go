package gnutar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
)

// newArchiver opens a gnutar archiver with the given options (the caller supplies
// state_dir for tests that produce incrementals) and skips when GNU tar is absent.
func newArchiver(t *testing.T, opts archiver.Options) archiver.Archiver {
	t.Helper()
	m, err := archiver.Open("gnutar", opts)
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
// directly (no compression) to test the archiver in isolation.
func TestBackupRestoreWithDeletion(t *testing.T) {
	src := t.TempDir()
	out := t.TempDir()
	m := newArchiver(t, archiver.Options{"state_dir": t.TempDir()})

	write(t, filepath.Join(src, "a.txt"), "alpha")
	write(t, filepath.Join(src, "b.txt"), "beta")
	write(t, filepath.Join(src, "sub", "c.txt"), "gamma")

	l0 := filepath.Join(out, "l0.tar")
	backup(t, m, archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 0, BaseLevel: -1}, l0)

	time.Sleep(1100 * time.Millisecond) // 1s mtime granularity
	write(t, filepath.Join(src, "a.txt"), "alpha-CHANGED")
	if err := os.Remove(filepath.Join(src, "b.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "d.txt"), "delta")

	l1 := filepath.Join(out, "l1.tar")
	backup(t, m, archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 1, BaseLevel: 0}, l1)

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

// TestExclude verifies the request's exclude patterns flow through to tar.
func TestExclude(t *testing.T) {
	m := newArchiver(t, archiver.Options{"state_dir": t.TempDir()})
	src := t.TempDir()
	out := t.TempDir()
	write(t, filepath.Join(src, "keep.txt"), "keep")
	write(t, filepath.Join(src, "drop.log"), "drop")

	l0 := filepath.Join(out, "l0.tar")
	backup(t, m, archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 0, BaseLevel: -1, Exclude: []string{"*.log"}}, l0)

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
	src := t.TempDir()
	m := newArchiver(t, archiver.Options{"state_dir": t.TempDir()})

	write(t, filepath.Join(src, "big.bin"), strings.Repeat("x", 200000))
	write(t, filepath.Join(src, "small.txt"), "hi")

	full, err := m.Estimate(archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 0, BaseLevel: -1})
	if err != nil {
		t.Fatal(err)
	}
	if full < 200000 {
		t.Errorf("full estimate %d should be >= the 200000-byte file", full)
	}

	// Excluding the big file yields a much smaller estimate.
	excl, err := m.Estimate(archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 0, BaseLevel: -1, Exclude: []string{"*.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if excl >= full {
		t.Errorf("excluded estimate %d should be < full estimate %d", excl, full)
	}

	// An unchanged incremental against a real snapshot estimates far below a full.
	time.Sleep(1100 * time.Millisecond) // snapshot time must beat file mtimes (1s granularity)
	backup(t, m, archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 0, BaseLevel: -1}, filepath.Join(t.TempDir(), "l0.tar"))
	if !m.HasBase("app", 0) {
		t.Fatal("L0 snapshot should exist after a full backup")
	}
	incr, err := m.Estimate(archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 1, BaseLevel: 0})
	if err != nil {
		t.Fatal(err)
	}
	if incr >= full {
		t.Errorf("unchanged incremental estimate %d should be < full %d", incr, full)
	}
}

func backup(t *testing.T, m archiver.Archiver, req archiver.BackupRequest, outFile string) {
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

func restore(t *testing.T, m archiver.Archiver, inFile, dest string) {
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
