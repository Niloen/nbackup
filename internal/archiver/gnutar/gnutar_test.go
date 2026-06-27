package gnutar

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
)

// newArchiver opens a gnutar archiver whose incremental state lives under stateRoot (the
// caller supplies a temp dir for tests that produce incrementals) and skips when GNU tar
// is absent.
func newArchiver(t *testing.T, stateRoot string) archiver.Archiver {
	t.Helper()
	m, err := archiver.Open("gnutar", nil, programs.Local(), stateRoot)
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
	m := newArchiver(t, t.TempDir())

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
	m := newArchiver(t, t.TempDir())
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

// TestNewExcludeIsNotADeletion pins a load-bearing GNU tar behavior for the
// large-DLE split design (docs/design/split-sources-spec.md): newly EXCLUDING a
// subtree that still exists on disk does NOT record it as a deletion in the
// incremental dumpdir, so a chain restore keeps the stale copy. This is the exact
// opposite of removing the subtree from disk, which a chain restore *does* delete
// (see TestBackupRestoreWithDeletion).
//
// Consequence for the design: carving a subtree out of an existing DLE by adding it
// to a `split:` list is NOT a free, continuous operation — the remainder must be
// RE-BASELINED (forced to a fresh level-0 full) when its exclude set changes, or a
// point-in-time restore of the remainder would resurrect the carved subtree (and
// collide with the new shard's copy). The "only the subtree pays a re-dump" claim is
// false for GNU tar; the reshard costs a remainder full. This test guards that fact
// so a future tar/option change that flips the behavior is caught loudly.
func TestNewExcludeIsNotADeletion(t *testing.T) {
	m := newArchiver(t, t.TempDir())
	src := t.TempDir()
	out := t.TempDir()
	write(t, filepath.Join(src, "datasets", "x.txt"), "x")
	write(t, filepath.Join(src, "keep.txt"), "keep")

	// L0: full, whole tree (the un-split "remainder" before carving).
	l0 := filepath.Join(out, "l0.tar")
	backup(t, m, archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 0, BaseLevel: -1}, l0)

	// L1: incremental that newly excludes datasets/ — but the subtree is STILL on
	// disk (a carve, not a delete). No file is modified or removed.
	l1 := filepath.Join(out, "l1.tar")
	backup(t, m, archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 1, BaseLevel: 0, Exclude: []string{"datasets"}}, l1)

	dest := t.TempDir()
	restore(t, m, l0, dest)
	restore(t, m, l1, dest)

	// The pinned behavior: the excluded-but-still-present subtree SURVIVES the chain
	// restore. If this assertion ever fails, GNU tar started treating exclusion as a
	// deletion — revisit the spec's reshard story, which currently forces a full.
	if _, err := os.Stat(filepath.Join(dest, "datasets", "x.txt")); err != nil {
		t.Errorf("datasets/x.txt should SURVIVE (exclusion is not a deletion in GNU tar); stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "keep.txt")); err != nil {
		t.Errorf("keep.txt should be present: %v", err)
	}
}

// TestEstimate checks the /dev/null client estimate: the full reflects the data
// size, excludes lower it, and an unchanged incremental is far smaller than a full.
func TestEstimate(t *testing.T) {
	src := t.TempDir()
	m := newArchiver(t, t.TempDir())

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

// backup runs the archiver's backup pipeline source to outFile, the way the writer does
// (run the tar stage, drain its stdout, finish), exercising the new BackupSource API.
func backup(t *testing.T, m archiver.Archiver, req archiver.BackupRequest, outFile string) {
	t.Helper()
	f, err := os.Create(outFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bs, err := m.BackupSource(req)
	if err != nil {
		t.Fatalf("backup source L%d: %v", req.Level, err)
	}
	out, wait, err := bs.Exec.RunPipe(nil, bs.Stage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, out); err != nil {
		t.Fatal(err)
	}
	out.Close()
	if err := wait(); err != nil {
		t.Fatalf("backup L%d: %v", req.Level, err)
	}
	if _, err := bs.Finish(); err != nil {
		t.Fatalf("finish L%d: %v", req.Level, err)
	}
	if bs.Cleanup != nil {
		bs.Cleanup()
	}
}

func restore(t *testing.T, m archiver.Archiver, inFile, dest string) {
	t.Helper()
	f, err := os.Open(inFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	out, wait, err := programs.Local().RunPipe(f, m.RestoreStage(dest, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, out); err != nil {
		t.Fatal(err)
	}
	out.Close()
	if err := wait(); err != nil {
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
