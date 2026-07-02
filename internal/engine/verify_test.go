package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/drill"
)

// twoDLEFixture dumps two DLEs into one run on disk and mirrors the run onto an
// offsite disk medium, so a run has two archives across two copies — the shape the
// multi-copy verify matrix reasons over.
type twoDLEFixture struct {
	eng     *Engine
	runID   string
	dleA    string
	dleB    string
	diskDir string
	offsite string
}

func newTwoDLEFixture(t *testing.T) *twoDLEFixture {
	t.Helper()
	srcA, srcB := t.TempDir(), t.TempDir()
	write(t, filepath.Join(srcA, "a.txt"), "alpha")
	write(t, filepath.Join(srcB, "b.txt"), "bravo")
	diskDir, offsiteDir := t.TempDir(), t.TempDir()

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": diskDir}},
			"offsite": {Type: "disk", Params: map[string]string{"path": offsiteDir}},
		},
		Sources: []config.DLE{
			{Host: "localhost", Path: srcA},
			{Host: "localhost", Path: srcB},
		},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skip("GNU tar not available")
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if _, err := eng.SyncTo("", "offsite", SyncSelection{}, true, false, nil); err != nil {
		t.Fatalf("sync offsite: %v", err)
	}
	return &twoDLEFixture{
		eng:     eng,
		runID:   s.ID,
		dleA:    config.DLE{Host: "localhost", Path: srcA}.Name(),
		dleB:    config.DLE{Host: "localhost", Path: srcB}.Name(),
		diskDir: diskDir,
		offsite: offsiteDir,
	}
}

// TestVerifyIntactCopyRemains exercises verify.go's "FAILED but an intact copy remains"
// branch: with a DLE on two media, corrupting one copy must report a failure that names
// the surviving copy to re-copy from, not a blanket all-copies failure.
func TestVerifyIntactCopyRemains(t *testing.T) {
	f := newDrillFixture(t, "none")
	eng := f.eng

	// Corrupt only the disk copy of the full; the offsite copy stays intact.
	corrupt(t, payloadFile(t, f.diskDir, "run-2026-06-21.001", 0))

	cap := &capturingLogf{}
	rep, err := eng.Verify([]string{"run-2026-06-21.001"}, VerifyOptions{}, cap.log)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 1 {
		t.Fatalf("failures = %d, want 1 (one corrupt copy)", rep.Failures)
	}
	if !cap.contains("an intact copy remains on offsite") {
		t.Errorf("verify should reassure that an intact copy remains; log:\n%s", cap.joined())
	}
	if cap.contains("FAILED on all") {
		t.Errorf("with a surviving copy verify must not report all copies failed; log:\n%s", cap.joined())
	}
}

// TestVerifyMediumScopedRespectsArchiveGranularity locks the archive-granular placement
// rule under a pinned medium: an archive pruned from one copy (its parts removed from that
// placement) is by design absent there, not a missing-position fault. A medium-scoped
// verify judges the copy against what it actually records — so the pruned DLE is not failed
// on that copy (it survives, and verifies, on its other copy), and only the archives the
// copy still holds are checked.
func TestVerifyMediumScopedRespectsArchiveGranularity(t *testing.T) {
	f := newTwoDLEFixture(t)
	eng := f.eng

	// Prune DLE A's archive from the offsite copy only; disk still holds it, so the
	// run's medium-independent content still lists both archives.
	if _, _, err := eng.cat.RemoveArchive(f.runID, "offsite", f.dleA); err != nil {
		t.Fatal(err)
	}

	cap := &capturingLogf{}
	rep, err := eng.Verify([]string{f.runID}, VerifyOptions{Medium: "offsite"}, cap.log)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 0 {
		t.Fatalf("failures = %d, want 0 (a pruned archive is absent by design, not a fault)", rep.Failures)
	}
	if cap.contains("POSITION MISSING") {
		t.Errorf("a per-archive prune must not read as a missing position; log:\n%s", cap.joined())
	}
	// The archive the offsite copy still holds (DLE B) is verified there; DLE A is not
	// reported against the offsite copy at all.
	for i := range rep.Runs[0].Archives {
		if av := &rep.Runs[0].Archives[i]; av.DLE == f.dleA && av.Medium == "offsite" {
			t.Fatalf("pruned DLE A must not be judged on the offsite copy, got %+v", av)
		}
	}
}

// TestVerifySkipsCopyOnUnknownMedium exercises the ErrUnknownMedium branch: a run whose
// only copy lives on a medium this config no longer defines is out of scope, not damaged
// — verify must SKIP it (with a note) and never report a false integrity failure.
func TestVerifySkipsCopyOnUnknownMedium(t *testing.T) {
	f := newDrillFixture(t, "none")
	eng := f.eng
	id := "run-2026-06-21.001"

	// Leave only the offsite copy, then drop offsite from the config so it is unknown.
	if _, err := eng.cat.RemovePlacement(id, "disk"); err != nil {
		t.Fatal(err)
	}
	delete(eng.cfg.Media, "offsite")

	cap := &capturingLogf{}
	rep, err := eng.Verify([]string{id}, VerifyOptions{}, cap.log)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 0 {
		t.Fatalf("failures = %d, want 0 (a copy on an unknown medium is skipped, not failed)", rep.Failures)
	}
	if !cap.contains("SKIPPED — copies only on media not in this config") {
		t.Errorf("verify should announce the whole-run skip; log:\n%s", cap.joined())
	}
}

// TestVerifyCopyOpenErrorIsPipeline exercises the non-unknown copy-open-error branch: a
// copy on a medium that IS defined but cannot be opened is a real pipeline failure
// (badCopy, ClassPipeline), distinct from the out-of-scope unknown-medium skip.
func TestVerifyCopyOpenErrorIsPipeline(t *testing.T) {
	f := newDrillFixture(t, "none")
	eng := f.eng
	id := "run-2026-06-21.001"

	// Leave only the offsite copy, then redefine offsite as an unreachable cloud medium
	// — still known to the config, so opening it is a genuine failure, not a skip.
	if _, err := eng.cat.RemovePlacement(id, "disk"); err != nil {
		t.Fatal(err)
	}
	eng.cfg.Media["offsite"] = config.Media{Type: "cloud", Params: map[string]string{"url": "bogusscheme://nope"}}

	cap := &capturingLogf{}
	rep, err := eng.Verify([]string{id}, VerifyOptions{}, cap.log)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 1 {
		t.Fatalf("failures = %d, want 1 (a configured copy that will not open is a failure)", rep.Failures)
	}
	if cls := firstFailClass(rep); cls != drill.ClassPipeline {
		t.Fatalf("open-failure class = %s, want pipeline", cls)
	}
}
