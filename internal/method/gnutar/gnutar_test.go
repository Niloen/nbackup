package gnutar

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/dle"
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
	d := dle.DLE{Host: "h", Path: src}

	write(t, filepath.Join(src, "a.txt"), "alpha")
	write(t, filepath.Join(src, "b.txt"), "beta")
	write(t, filepath.Join(src, "sub", "c.txt"), "gamma")

	l0 := filepath.Join(out, "l0.tar")
	backup(t, m, method.BackupRequest{DLE: d, Level: 0, OutSnap: filepath.Join(snaps, "L0.snar")}, l0)

	time.Sleep(1100 * time.Millisecond) // 1s mtime granularity
	write(t, filepath.Join(src, "a.txt"), "alpha-CHANGED")
	if err := os.Remove(filepath.Join(src, "b.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "d.txt"), "delta")

	l1 := filepath.Join(out, "l1.tar")
	backup(t, m, method.BackupRequest{
		DLE: d, Level: 1,
		BaseSnap: filepath.Join(snaps, "L0.snar"),
		OutSnap:  filepath.Join(snaps, "L1.snar"),
	}, l1)

	dest := t.TempDir()
	restore(t, m, d, l0, dest)
	restore(t, m, d, l1, dest)

	assertContent(t, filepath.Join(dest, "a.txt"), "alpha-CHANGED")
	assertContent(t, filepath.Join(dest, "sub", "c.txt"), "gamma")
	assertContent(t, filepath.Join(dest, "d.txt"), "delta")
	if _, err := os.Stat(filepath.Join(dest, "b.txt")); !os.IsNotExist(err) {
		t.Errorf("b.txt should have been deleted on restore, stat err = %v", err)
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

func restore(t *testing.T, m method.Method, d dle.DLE, inFile, dest string) {
	t.Helper()
	f, err := os.Open(inFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := m.Restore(d, f, dest); err != nil {
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
