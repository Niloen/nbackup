package engine

import (
	"context"
	"github.com/Niloen/nbackup/internal/archiveio"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// TestPartialPruneKeepsCatalogArchiveGranular is the end-to-end regression for the
// per-archive prune desync: prune's unit is the archive, so a legitimate partial
// reclaim (one DLE's image deleted from a run's disk copy while a protected run-mate
// stays) must leave every consumer of the copy record agreeing with the medium:
//
//	(a) a second prune is a no-op (the pruned archive is not re-listed forever),
//	(b) medium usage counts only the bytes actually on the medium,
//	(c) verify passes — the pruned archive simply has no copy on that medium, and
//	    the intact copy on the sync target is still verified,
//	(d) a rebuild from the media reproduces the same state,
//
// plus: a restore of the pruned archive falls back to the medium that still holds it.
func TestPartialPruneKeepsCatalogArchiveGranular(t *testing.T) {
	srcA := t.TempDir()
	srcB := t.TempDir()
	write(t, filepath.Join(srcA, "big.bin"), strings.Repeat("A", 200_000))
	write(t, filepath.Join(srcB, "small.txt"), "keep me")

	diskPath, archivePath := t.TempDir(), t.TempDir()
	workdir, stateDir := t.TempDir(), t.TempDir()
	cfgFor := func(diskCapacity string) *config.Config {
		cfg := &config.Config{
			Landing: "disk",
			Cycle:   "1d",
			Media: map[string]config.Media{
				"disk":    {Type: "disk", Capacity: diskCapacity, Params: map[string]string{"path": diskPath}},
				"archive": {Type: "disk", Params: map[string]string{"path": archivePath}},
			},
			Sources:  []config.DLE{{Host: "localhost", Path: srcA}, {Host: "localhost", Path: srcB}},
			Workdir:  workdir,
			StateDir: stateDir,
		}
		cfg.Compress.Scheme = "none"
		return cfg
	}

	// Stage with an unbounded landing so both runs land whole: run 1 (fulls), a
	// forced re-full, run 2 — run 1's chains are then superseded (dead past
	// minimum_age). Both runs are synced to the archive medium.
	eng, err := New(cfgFor(""))
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	run1, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	for _, src := range []string{srcA, srcB} {
		if _, err := eng.ForceFull("localhost:" + src); err != nil {
			t.Fatalf("force full %s: %v", src, err)
		}
	}
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("dump 2: %v", err)
	}
	if _, err := eng.SyncTo("", "archive", SyncSelection{}, true, false, nil); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var prunedDLE, keptDLE string
	for _, a := range run1.Archives {
		if a.Uncompressed > 100_000 {
			prunedDLE = a.DLE
		} else {
			keptDLE = a.DLE
		}
	}
	if prunedDLE == "" || keptDLE == "" {
		t.Fatalf("could not tell the big DLE from the small one: %+v", run1.Archives)
	}

	// Reopen with a capacity that forces reclaiming run 1's big archive but is
	// satisfied once it is gone — the partial reclaim (its small run-mate stays).
	eng2, err := New(cfgFor("300KB"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	eligible, _, freed, err := eng2.Prune("disk", now, true, nil)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if eligible != 1 || freed <= 0 {
		t.Fatalf("prune reclaimed %d archive(s) (%d bytes), want exactly 1 (the partial reclaim)", eligible, freed)
	}
	if p, ok := eng2.placementOn(run1.ID, "disk"); !ok || p.Holds(prunedDLE, 0) || !p.Holds(keptDLE, 0) {
		t.Fatalf("disk placement after prune = %+v (ok=%v), want it to keep only %s", p, ok, keptDLE)
	}

	// (a) Prune is idempotent: a second pass has nothing to reclaim.
	eligible, _, freed, err = eng2.Prune("disk", now, true, nil)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if eligible != 0 || freed != 0 {
		t.Errorf("second prune reclaimed %d archive(s), %d bytes — want 0/0 (prune must converge)", eligible, freed)
	}

	// (b) Usage reflects what is actually on the medium: under capacity again.
	if over, used, capacity, err := eng2.MediumOverCapacity("disk"); err != nil || over {
		t.Errorf("disk still over capacity after prune: used=%d capacity=%d err=%v", used, capacity, err)
	}

	// (c) Verify is green: the pruned archive has no copy on disk (by design), its
	// intact copy on the archive medium is verified, and the surviving run-mate's
	// disk copy still checks out.
	if rep, err := eng2.Verify([]string{run1.ID}, VerifyOptions{}, nil); err != nil || rep.Failures != 0 {
		t.Errorf("verify after partial prune: failures=%d err=%v (%+v)", rep.Failures, err, rep.Runs)
	}
	if rep, err := eng2.Verify([]string{run1.ID}, VerifyOptions{Medium: "disk"}, nil); err != nil || rep.Failures != 0 {
		t.Errorf("medium-scoped verify on the pruned copy: failures=%d err=%v (%+v)", rep.Failures, err, rep.Runs)
	}

	// Restore falls back to the medium that still holds the archive.
	rc, err := eng2.fs.Open(archiveio.Ref{Run: run1.ID, DLE: prunedDLE, Level: 0}, "")
	if err != nil {
		t.Fatalf("open pruned archive via surviving copy: %v", err)
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Errorf("read pruned archive from surviving copy: %v", err)
	}
	rc.Close()

	// (d) A rebuild from the media reproduces the same archive-granular state.
	usedBefore := eng2.Catalog().MediumBytes("disk")
	if _, err := eng2.RebuildCatalog(nil); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if used := eng2.Catalog().MediumBytes("disk"); used != usedBefore {
		t.Errorf("rebuild changed disk usage: %d -> %d (must reflect the medium)", usedBefore, used)
	}
	if p, ok := eng2.placementOn(run1.ID, "disk"); !ok || p.Holds(prunedDLE, 0) || !p.Holds(keptDLE, 0) {
		t.Errorf("disk placement after rebuild = %+v (ok=%v), want only %s (the pruned archive is not on disk)", p, ok, keptDLE)
	}
	eligible, _, _, err = eng2.Prune("disk", now, true, nil)
	if err != nil || eligible != 0 {
		t.Errorf("prune after rebuild reclaimed %d archive(s) (err=%v), want 0", eligible, err)
	}
	if rep, err := eng2.Verify([]string{run1.ID}, VerifyOptions{}, nil); err != nil || rep.Failures != 0 {
		t.Errorf("verify after rebuild: failures=%d err=%v", rep.Failures, err)
	}
}
