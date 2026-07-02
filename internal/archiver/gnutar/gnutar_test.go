package gnutar

import (
	"context"
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

// TestSnapshotPromotionIsAtomic verifies a dump's snapshot only enters the library once
// the archive is committed: an uncommitted (failed/interrupted) dump leaves the committed
// base byte-for-byte intact, so a retry builds on a valid base rather than a corpse. This
// is the regression guard for "an out-of-space dump zeroed the snapshot, so the next
// incremental re-dumped everything".
func TestSnapshotPromotionIsAtomic(t *testing.T) {
	src := t.TempDir()
	stateRoot := t.TempDir()
	m := newArchiver(t, stateRoot)
	write(t, filepath.Join(src, "a.txt"), "alpha")

	// A committed full leaves a usable base.
	backup(t, m, archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 0, BaseLevel: -1}, filepath.Join(t.TempDir(), "l0.tar"))
	if !m.HasBase("app", 0) {
		t.Fatal("L0 base should exist after a committed full")
	}
	live := filepath.Join(stateRoot, "app", "L0.snar")
	good, err := os.ReadFile(live)
	if err != nil || len(good) == 0 {
		t.Fatalf("read committed L0 snapshot: err=%v len=%d", err, len(good))
	}

	// Start a fresh L0 dump, run tar, but never promote it — the archive never committed
	// (e.g. the medium filled). Only Cleanup runs, as on the failure path.
	bs, err := m.BackupSource(archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 0, BaseLevel: -1})
	if err != nil {
		t.Fatal(err)
	}
	out, wait, err := bs.Exec.RunPipe(context.Background(), nil, bs.Stage)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, out)
	out.Close()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if bs.Cleanup != nil {
		bs.Cleanup()
	}

	after, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("committed base vanished after an uncommitted dump: %v", err)
	}
	if string(after) != string(good) {
		t.Fatal("committed base was mutated by an uncommitted dump (it must be untouched until promote)")
	}
	if !m.HasBase("app", 0) {
		t.Fatal("base should still be usable after an uncommitted dump")
	}
}

// TestHasBaseRejectsEmptySnapshot verifies a present-but-empty snapshot (the corpse a
// killed dump can leave behind) does not count as a usable base, so the engine forces a
// full rather than building a full-sized incremental on it.
func TestHasBaseRejectsEmptySnapshot(t *testing.T) {
	stateRoot := t.TempDir()
	m := newArchiver(t, stateRoot)
	dir := filepath.Join(stateRoot, "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "L0.snar")
	if err := os.WriteFile(snap, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if m.HasBase("app", 0) {
		t.Fatal("an empty snapshot must not count as a usable base")
	}
	if err := os.WriteFile(snap, []byte("GNU tar-1.34-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.HasBase("app", 0) {
		t.Fatal("a non-empty snapshot should count as a usable base")
	}
}

// TestList verifies the deep-verify member lister: `tar -t` over a real archive returns
// its members (the structural half of a verify), and the pipeline completing cleanly proves
// the stream is a valid, listable archive.
func TestList(t *testing.T) {
	src := t.TempDir()
	out := t.TempDir()
	m := newArchiver(t, t.TempDir())
	write(t, filepath.Join(src, "a.txt"), "alpha")
	write(t, filepath.Join(src, "sub", "c.txt"), "gamma")

	l0 := filepath.Join(out, "l0.tar")
	backup(t, m, archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 0, BaseLevel: -1}, l0)

	f, err := os.Open(l0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	members, err := m.List(f)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, mem := range members {
		got[mem] = true
	}
	for _, want := range []string{"./a.txt", "./sub/", "./sub/c.txt"} {
		if !got[want] {
			t.Errorf("List missing member %q; got %v", want, members)
		}
	}
}

// TestListRejectsGarbage verifies List surfaces an error (rather than an empty success)
// when the input is not a valid tar stream — the "did it decode" half of a deep verify.
func TestListRejectsGarbage(t *testing.T) {
	m := newArchiver(t, t.TempDir())
	_, err := m.List(strings.NewReader("this is not a tar archive at all, just noise\n"))
	if err == nil {
		t.Fatal("List must fail on a non-tar stream")
	}
}

// TestEstimateIncompleteFloor verifies that when a source file cannot be read, Estimate
// returns the running total as a FLOOR alongside an error naming the incompleteness — so
// capacity planning warns rather than silently undercounting to ~0.
func TestEstimateIncompleteFloor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root: a chmod-000 directory is still traversable, so tar never reports it unreadable")
	}
	src := t.TempDir()
	m := newArchiver(t, t.TempDir())
	write(t, filepath.Join(src, "readable.bin"), strings.Repeat("x", 50000))
	// A directory tar cannot open during the metadata walk (the /dev/null estimate stats
	// bodies but must still enter directories) — an unreadable *file* would still be stat-able.
	locked := filepath.Join(src, "locked")
	write(t, filepath.Join(locked, "inner.bin"), strings.Repeat("y", 50000))
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	total, err := m.Estimate(archiver.BackupRequest{DLE: "app", SourcePath: src, Level: 0, BaseLevel: -1})
	if err == nil {
		t.Fatal("an unreadable source must make Estimate report incompleteness")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("error = %q, want it to name the incomplete estimate", err)
	}
	if total <= 0 {
		t.Errorf("floor total = %d, want > 0 (the readable file's bytes, not a silent ~0)", total)
	}
}

// TestSeedSnapshotCopyFailure verifies seedSnapshot surfaces the error when an incremental's
// base snapshot is absent (CopyFile of a missing source) rather than silently seeding an
// empty state — which would make the incremental re-dump everything.
func TestSeedSnapshotCopyFailure(t *testing.T) {
	stateRoot := t.TempDir()
	m := newArchiver(t, stateRoot)
	g := m.(*gnutar)
	// Level 1 with base level 0 present nowhere: seedSnapshot must CopyFile the (missing)
	// L0 snapshot and fail.
	out := filepath.Join(t.TempDir(), "work.snar")
	err := g.seedSnapshot(archiver.BackupRequest{DLE: "app", SourcePath: t.TempDir(), Level: 1, BaseLevel: 0}, out)
	if err == nil {
		t.Fatal("seeding an incremental with no base snapshot must fail")
	}
}

// TestPromoteSnapshotRenameFailure verifies promoteSnapshot surfaces the rename error when
// there is no work snapshot to promote (a dump that never wrote one) — the failure half of
// the ".new"-then-rename promotion.
func TestPromoteSnapshotRenameFailure(t *testing.T) {
	stateRoot := t.TempDir()
	m := newArchiver(t, stateRoot)
	g := m.(*gnutar)
	if err := g.promoteSnapshot("app", 0); err == nil {
		t.Fatal("promoting with no work snapshot must fail (nothing to rename)")
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
	out, wait, err := bs.Exec.RunPipe(context.Background(), nil, bs.Stage)
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
	// A real dump promotes the new snapshot into the library only once the archive is
	// committed; this helper models that success path.
	if bs.Promote != nil {
		if err := bs.Promote(); err != nil {
			t.Fatalf("promote L%d: %v", req.Level, err)
		}
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
	out, wait, err := programs.Local().RunPipe(context.Background(), f, m.RestoreStage(dest, nil))
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

// TestClassifyTarStderr locks the partial/fatal split that lets an unreadable-file dump
// commit (a partial archive) while a genuinely fatal tar error still aborts. It is the
// careful core of the "commit partial rather than discard a usable backup" behavior.
func TestClassifyTarStderr(t *testing.T) {
	// Unreadable members + benign info → partial, with the paths captured; no fatal error.
	stderr := "tar: .: Directory is new\n" +
		"tar: ./secret.txt: Cannot open: Permission denied\n" +
		"tar: ./priv/key: Cannot stat: Permission denied\n" +
		"Total bytes written: 10240 (10KiB, 35MiB/s)\n" +
		"tar: Exiting with failure status due to previous errors\n"
	unreadable, fatal := classifyTarStderr(stderr)
	if fatal != nil {
		t.Fatalf("read-only errors should not be fatal: %v", fatal)
	}
	if len(unreadable) != 2 || unreadable[0] != "./secret.txt" || unreadable[1] != "./priv/key" {
		t.Errorf("unreadable = %v, want [./secret.txt ./priv/key]", unreadable)
	}

	// A genuinely fatal line (write failure) → fatal, even mixed with a read error.
	_, fatal = classifyTarStderr("tar: ./a: Cannot open: Permission denied\ntar: /dev/st0: Cannot write: No space left on device\n")
	if fatal == nil {
		t.Error("an unrecognized fatal line should make the dump fail")
	}

	// A clean / warning-only run → no partial, no fatal. The volatile-file warnings
	// (changed / removed / shrunk) all describe a file that mutated mid-walk; the archive
	// stays valid, so none of them may abort the dump.
	unreadable, fatal = classifyTarStderr("tar: ./log: file changed as we read it\n" +
		"tar: ./state/L0.snar.new: File removed before we read it\n" +
		"tar: ./db: File shrunk by 4096 bytes, padding with zeros\n" +
		"Total bytes written: 512\n")
	if fatal != nil || len(unreadable) != 0 {
		t.Errorf("clean/warning run: unreadable=%v fatal=%v, want none", unreadable, fatal)
	}
}
