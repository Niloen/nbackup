package catalog

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// TestOpenCorruptCache: a malformed cache file is a hard, named error — not a
// silent empty catalog that would make every committed run look lost and could
// drive a destructive reclaim.
func TestOpenCorruptCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CacheFile), []byte("{ this is not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if err == nil || !strings.Contains(err.Error(), "parse catalog cache") {
		t.Fatalf("want a parse error for a corrupt cache, got: %v", err)
	}
}

func archivePos(dle string, level, partPos, commitPos int) ArchivePos {
	return ArchivePos{
		DLE:    dle,
		Level:  level,
		Parts:  []FilePos{{Pos: partPos}},
		Commit: FilePos{Pos: commitPos},
	}
}

// TestRemovePlacementRoundTrip: dropping one medium's copy leaves the run while
// another copy survives; dropping the last copy removes the entry entirely (the
// run no longer exists anywhere) and persists across a reopen.
func TestRemovePlacementRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := "run-2026-06-20.001"
	arch := record.Archive{Run: run, DLE: "h-data", Level: 0, Compressed: 100}
	if err := cat.AddArchive(arch, "disk", archivePos("h-data", 0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddArchive(arch, "tape", archivePos("h-data", 0, 0, 1)); err != nil {
		t.Fatal(err)
	}

	gone, err := cat.RemovePlacement(run, "disk")
	if err != nil {
		t.Fatal(err)
	}
	if gone {
		t.Fatal("gone should be false while the tape copy survives")
	}
	if _, err := cat.ReadRun(run); err != nil {
		t.Fatalf("run must survive while a copy remains: %v", err)
	}

	gone, err = cat.RemovePlacement(run, "tape")
	if err != nil {
		t.Fatal(err)
	}
	if !gone {
		t.Fatal("gone should be true once the last copy is dropped")
	}
	if _, err := cat.ReadRun(run); err == nil {
		t.Fatal("run must be gone once its last copy is removed")
	}

	// The removal persisted: a reopen sees no runs.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Runs()) != 0 {
		t.Fatalf("reopened catalog should have no runs, got %d", len(reopened.Runs()))
	}
}

// TestRemovePlacementUnknownRun is a no-op that neither errors nor creates state.
func TestRemovePlacementUnknownRun(t *testing.T) {
	cat, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gone, err := cat.RemovePlacement("run-does-not-exist.001", "disk")
	if err != nil || gone {
		t.Fatalf("unknown run should be a no-op (gone=false, nil err), got gone=%v err=%v", gone, err)
	}
}

// TestVolumeRegistryRoundTrip: RecordVolume upserts a labeled volume's identity,
// Volume/Volumes read it back, and RemoveVolume drops it (a relabel retires the
// old name) — all persisted across a reopen.
func TestVolumeRegistryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	lbl := record.Label{Name: "vol-a", Pool: "tape", Epoch: 1}
	if err := cat.RecordVolume(lbl); err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Volume("vol-a"); !ok {
		t.Fatal("RecordVolume should make vol-a resolvable")
	}
	if len(cat.Volumes()) != 1 {
		t.Fatalf("Volumes() = %d, want 1", len(cat.Volumes()))
	}

	// Reopen: the volume registry persisted.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Volume("vol-a"); !ok {
		t.Fatal("volume registry should persist across Open")
	}

	// RemoveVolume of an absent name is a no-op; of a present one it drops it.
	if err := reopened.RemoveVolume("nope"); err != nil {
		t.Fatalf("removing an absent volume should be a no-op, got: %v", err)
	}
	if err := reopened.RemoveVolume("vol-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Volume("vol-a"); ok {
		t.Fatal("RemoveVolume should retire vol-a")
	}
}

// TestQueryProjections exercises the per-medium and per-label read projections
// the policy layer (retention, reclaim, reusability) reasons over.
func TestQueryProjections(t *testing.T) {
	cat, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := "run-2026-06-20.001"
	arch := record.Archive{Run: run, DLE: "h-data", Level: 0, Compressed: 100}
	pos := ArchivePos{DLE: "h-data", Level: 0, Parts: []FilePos{{Label: "vol-a", Pos: 0}}, Commit: FilePos{Label: "vol-a", Pos: 1}}
	if err := cat.AddArchive(arch, "tape", pos); err != nil {
		t.Fatal(err)
	}

	if got := cat.RunsOn("tape"); len(got) != 1 || got[0].ID != run {
		t.Fatalf("RunsOn(tape) = %v, want the one run", got)
	}
	if got := cat.RunsOn("disk"); len(got) != 0 {
		t.Fatalf("RunsOn(disk) = %v, want none", got)
	}
	if got := cat.RunsOnLabel("vol-a"); len(got) != 1 {
		t.Fatalf("RunsOnLabel(vol-a) = %v, want the run", got)
	}
	if got := cat.RunIDsOnLabel("vol-a"); len(got) != 1 || got[0] != run {
		t.Fatalf("RunIDsOnLabel(vol-a) = %v, want [%s]", got, run)
	}
	if got := cat.Archives(); len(got) != 1 || got[0].DLE != "h-data" {
		t.Fatalf("Archives() = %v, want the one archive", got)
	}
	if got := cat.ArchivesOn("tape"); len(got) != 1 {
		t.Fatalf("ArchivesOn(tape) = %v, want the one archive", got)
	}
}

// TestUnreadableFooterIsOrphan: a part with a footer-shaped but corrupt (unparseable)
// commit file must read as an orphan/uncommitted archive — never assembled into the
// catalog, and the scan must not crash. This is the partial-write tolerance the
// rebuild depends on.
func TestUnreadableFooterIsOrphan(t *testing.T) {
	dir := t.TempDir()
	vol := newVolume(t, dir)
	runID := "run-2026-06-22.001"

	// A part, plus a KindCommit file whose payload is not a valid commit footer.
	putPart(t, vol, runID, record.Archive{DLE: "h-data", Level: 0})
	if _, err := writeFileT(vol, record.Header{Run: runID, Kind: record.KindCommit, DLE: "h-data", Level: 0},
		func(w io.Writer) error {
			_, e := w.Write([]byte("this is not a valid commit footer {{{{"))
			return e
		}); err != nil {
		t.Fatal(err)
	}

	// The scan must not crash and must not index the archive (no valid footer).
	cat, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n, err := cat.Rebuild(map[string]media.Volume{"disk": newVolume(t, dir)})
	if err != nil {
		t.Fatalf("a corrupt footer must not crash the scan: %v", err)
	}
	if n != 0 {
		t.Fatalf("an archive with an unreadable footer must not be indexed, got %d runs", n)
	}

	// OrphanFiles sees both the part and the unparseable commit file as orphans
	// (neither is referenced by an assembled archive).
	orphans, err := OrphanFiles(newVolume(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 2 {
		t.Fatalf("want 2 orphans (part + torn footer), got %d: %+v", len(orphans), orphans)
	}
}
