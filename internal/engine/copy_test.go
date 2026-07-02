package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// copyFixture dumps one run onto disk with two extra media defined ("archive", "cold")
// so copy source/target permutations can be exercised.
func copyFixture(t *testing.T) (*Engine, string, string) {
	t.Helper()
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "copy me")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"archive": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"cold":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
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
	return eng, s.ID, config.DLE{Host: "localhost", Path: src}.Name()
}

// TestCopyNoCopyOnSource exercises copySource's "no copy on source medium" branch: a
// copy whose source medium does not hold the run must fail before any bytes flow.
func TestCopyNoCopyOnSource(t *testing.T) {
	eng, runID, _ := copyFixture(t)

	err := eng.CopyRun(runID, "archive", "cold", false, logfDiscard)
	if err == nil {
		t.Fatal("copy from a medium with no copy of the run must fail")
	}
	if !strings.Contains(err.Error(), "no copy on source medium") {
		t.Errorf("error = %v, want a no-copy-on-source failure", err)
	}
}

// TestCopySourceOpenCheckFails exercises copySource's openCheck branch: the run has a
// copy recorded on the source medium, but that medium can no longer be opened, so the
// copy fails fast on the open probe rather than mid-read.
func TestCopySourceOpenCheckFails(t *testing.T) {
	eng, runID, _ := copyFixture(t)

	// Land a real copy on "archive", then redefine it as an unreachable cloud medium:
	// the placement is still recorded, so copySource passes the placement check and
	// fails on the open probe.
	if err := eng.CopyRun(runID, "disk", "archive", false, logfDiscard); err != nil {
		t.Fatalf("seed archive copy: %v", err)
	}
	eng.cfg.Media["archive"] = config.Media{Type: "cloud", Params: map[string]string{"url": "bogusscheme://nope"}}

	err := eng.CopyRun(runID, "archive", "cold", false, logfDiscard)
	if err == nil {
		t.Fatal("copy from an unopenable source medium must fail")
	}
}

// TestCopyForceReclaimsThenRecopies exercises prepareRun's --force branch: a forced
// re-copy of a run already on the target reclaims the prior copy first (so the old
// files are not orphaned) and re-authors it, leaving exactly one placement on the target.
func TestCopyForceReclaimsThenRecopies(t *testing.T) {
	eng, runID, name := copyFixture(t)

	if err := eng.CopyRun(runID, "disk", "archive", false, logfDiscard); err != nil {
		t.Fatalf("first copy: %v", err)
	}
	if err := eng.CopyRun(runID, "disk", "archive", true, logfDiscard); err != nil {
		t.Fatalf("forced re-copy: %v", err)
	}
	// Still exactly two placements (disk + archive) — the reclaim removed the old
	// archive copy before re-authoring, so it did not accumulate a duplicate.
	if got := len(eng.cat.Placements(runID)); got != 2 {
		t.Fatalf("placements after forced re-copy = %d, want 2", got)
	}
	// The re-copied archive still restores from the target alone.
	if _, err := eng.cat.RemovePlacement(runID, "disk"); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := eng.Restore(runID, name, dest, false, logfDiscard); err != nil {
		t.Fatalf("restore from re-copied archive: %v", err)
	}
	assertContent(t, filepath.Join(dest, "f.txt"), "copy me")
}

// TestCopyTransferOpenFails exercises transferOne's clerk.Open error branch: when the
// source payload has gone missing (its placement is still recorded), opening the
// archive to copy it fails with the source medium named.
func TestCopyTransferOpenFails(t *testing.T) {
	eng, runID, _ := copyFixture(t)

	// The placement stays in the catalog, but the underlying files are gone — the
	// clerk cannot open the archive to read it.
	removeRunFiles(t, eng, runID)

	err := eng.CopyRun(runID, "disk", "archive", false, logfDiscard)
	if err == nil {
		t.Fatal("copy of a run whose source files are missing must fail")
	}
	if !strings.Contains(err.Error(), "from \"disk\"") && !strings.Contains(err.Error(), "copy") {
		t.Errorf("error = %v, want a copy-from-source failure", err)
	}
}

// TestCopyTransferMidStreamFails exercises transferOne's xfer.Transfer error branch: a
// truncated source payload opens but fails the archiveio part assertions mid-read, so
// the transfer aborts with the target medium named.
func TestCopyTransferMidStreamFails(t *testing.T) {
	eng, runID, _ := copyFixture(t)

	// Truncate the payload so the archive opens but its bytes no longer match the
	// header-framed part lengths, faulting mid-transfer.
	p := payloadFile(t, mediumRunDir(t, eng, "disk"), runID, 0)
	if err := os.Truncate(p, 4); err != nil {
		t.Fatal(err)
	}

	err := eng.CopyRun(runID, "disk", "archive", false, logfDiscard)
	if err == nil {
		t.Fatal("copy of a truncated source archive must fail")
	}
}

// mediumRunDir returns a disk medium's on-disk root, so a test can locate a run's
// payload files (payloadFile expects the directory holding the runs/ tree).
func mediumRunDir(t *testing.T, eng *Engine, medium string) string {
	t.Helper()
	return eng.cfg.Media[medium].Params["path"]
}
