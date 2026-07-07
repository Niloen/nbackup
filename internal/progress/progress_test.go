package progress

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clock is a controllable time source for deterministic timestamps.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock {
	return &clock{t: time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)}
}

func plan() []Plan {
	return []Plan{
		{Name: "bravo", Level: 1, EstBytes: 100},
		{Name: "alpha", Level: 0, EstBytes: 300},
	}
}

// TestTrackerLifecycle drives a run through start -> bytes -> finish and checks
// the snapshot reflects each transition, including ordering by DLE name.
func TestTrackerLifecycle(t *testing.T) {
	c := newClock()
	tr := NewTracker("run-2026-06-23.001", PhaseRunning, 2, plan(), c.now, nil)

	snap := tr.Snapshot()
	if snap.Phase != PhaseRunning {
		t.Fatalf("phase = %s, want running", snap.Phase)
	}
	if len(snap.DLEs) != 2 || snap.DLEs[0].Name != "alpha" {
		t.Fatalf("DLEs not seeded/sorted: %+v", snap.DLEs)
	}
	if snap.TotalEst() != 400 {
		t.Fatalf("total est = %d, want 400", snap.TotalEst())
	}

	tr.StartDLE("alpha")
	c.advance(2 * time.Second)
	tr.AddBytes("alpha", 150, 60)

	snap = tr.Snapshot()
	a := snap.DLEs[0]
	if a.State != StateDumping || a.DoneBytes != 150 || a.OutBytes != 60 {
		t.Fatalf("alpha mid-dump = %+v", a)
	}
	if got := a.Pct(); got != 50 {
		t.Fatalf("alpha pct = %.0f, want 50", got)
	}
	active, done, failed, pending := snap.Counts()
	if active != 1 || done != 0 || failed != 0 || pending != 1 {
		t.Fatalf("counts = %d/%d/%d/%d", active, done, failed, pending)
	}

	tr.FinishDLE("alpha", 7, 300, 120, nil)
	snap = tr.Snapshot()
	a = snap.DLEs[0]
	if a.State != StateDone || a.FileCount != 7 || a.DoneBytes != 300 {
		t.Fatalf("alpha finished = %+v", a)
	}
}

// TestTrackerFailure records a failed DLE with its error message.
func TestTrackerFailure(t *testing.T) {
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 1, plan(), c.now, nil)
	tr.StartDLE("bravo")
	tr.FinishDLE("bravo", 0, 0, 0, errors.New("tar exploded"))

	snap := tr.Snapshot()
	b := snap.DLEs[1] // bravo sorts after alpha
	if b.State != StateFailed || !strings.Contains(b.Err, "tar exploded") {
		t.Fatalf("bravo = %+v", b)
	}
	if _, _, failed, _ := snap.Counts(); failed != 1 {
		t.Fatalf("failed count = %d, want 1", failed)
	}
}

// TestTrackerCancel records a DLE interrupted by a canceled run as canceled — distinct
// from a failure (so it carries no error and is not counted as FAILED) and excluded from
// the live buckets, while the run's canceled phase is terminal.
func TestTrackerCancel(t *testing.T) {
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 2, plan(), c.now, nil)
	tr.StartDLE("alpha")
	tr.CancelDLE("alpha")
	tr.SetPhase(PhaseCanceled)

	snap := tr.Snapshot()
	a := snap.DLEs[0]
	if a.State != StateCanceled || a.Err != "" {
		t.Fatalf("alpha = %+v; want canceled with no error", a)
	}
	if snap.Canceled() != 1 {
		t.Fatalf("canceled count = %d, want 1", snap.Canceled())
	}
	active, done, failed, pending := snap.Counts()
	if active != 0 || done != 0 || failed != 0 || pending != 1 { // bravo still pending; alpha excluded
		t.Fatalf("counts = %d/%d/%d/%d; canceled DLE must not land in any live bucket", active, done, failed, pending)
	}
	if snap.Phase != PhaseCanceled || !snap.Phase.Terminal() {
		t.Fatalf("phase = %s (terminal=%v); want canceled+terminal", snap.Phase, snap.Phase.Terminal())
	}
}

// TestRateAndETA checks throughput and ETA derive from elapsed time and the
// remaining estimate.
func TestRateAndETA(t *testing.T) {
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 1, plan(), c.now, nil)
	tr.StartDLE("alpha")
	c.advance(10 * time.Second)
	tr.AddBytes("alpha", 100, 40) // 100 of 400 bytes in 10s -> 10 B/s

	snap := tr.Snapshot()
	now := c.now()
	if got := snap.Rate(now); got != 10 {
		t.Fatalf("rate = %.2f, want 10", got)
	}
	eta, ok := snap.ETA(now)
	if !ok || eta != 30*time.Second { // 300 remaining at 10 B/s
		t.Fatalf("eta = %v ok=%v, want 30s", eta, ok)
	}

	tr.SetPhase(PhaseDone)
	if _, ok := tr.Snapshot().ETA(now); ok {
		t.Fatal("terminal run should report no ETA")
	}
}

// TestFileSinkRoundTrip writes via the sink and reads back with Load, and checks
// the terminal snapshot persists (the "last run" behavior).
func TestFileSinkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := newClock()
	tr := NewTracker("run-x", PhaseRunning, 1, plan(), c.now, NewFileSink(dir, c.now))

	tr.StartDLE("alpha")
	tr.FinishDLE("alpha", 1, 300, 100, nil)
	tr.SetPhase(PhaseDone)

	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if snap.RunID != "run-x" || snap.Phase != PhaseDone {
		t.Fatalf("loaded snapshot = %+v", snap)
	}
	if snap.EndedAt.IsZero() {
		t.Fatal("terminal snapshot should record EndedAt")
	}
}

// TestFileSinkThrottle confirms byte updates are throttled but forced updates
// (state changes) always land.
func TestFileSinkThrottle(t *testing.T) {
	dir := t.TempDir()
	c := newClock()
	sink := NewFileSink(dir, c.now)

	tr := NewTracker("run", PhaseRunning, 1, plan(), c.now, sink) // forced initial write
	tr.StartDLE("alpha")                                          // forced

	// Two byte updates within the throttle window: only the first should write.
	tr.AddBytes("alpha", 10, 4)
	first, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	tr.AddBytes("alpha", 20, 8) // within window, suppressed
	again, _ := Load(dir)
	if again.DLEs[0].DoneBytes != first.DLEs[0].DoneBytes {
		t.Fatalf("throttled update leaked through: %d -> %d", first.DLEs[0].DoneBytes, again.DLEs[0].DoneBytes)
	}

	// A forced update (finish) always writes, even within the window.
	tr.FinishDLE("alpha", 1, 300, 100, nil)
	final, _ := Load(dir)
	if final.DLEs[0].State != StateDone {
		t.Fatalf("forced finish did not land: %+v", final.DLEs[0])
	}
}

// TestLoadMissing maps a missing status file to IsNotExist.
func TestLoadMissing(t *testing.T) {
	_, err := Load(t.TempDir())
	if !IsNotExist(err) {
		t.Fatalf("missing file err = %v, want not-exist", err)
	}
}

// TestRender produces a human report mentioning the key facts.
func TestRender(t *testing.T) {
	c := newClock()
	tr := NewTracker("run-2026-06-23.001", PhaseRunning, 2, plan(), c.now, nil)
	tr.StartDLE("alpha")
	c.advance(5 * time.Second)
	tr.AddBytes("alpha", 150, 60)

	var sb strings.Builder
	Render(&sb, tr.Snapshot(), c.now())
	out := sb.String()
	for _, want := range []string{"run-2026-06-23.001", "running", "alpha", "50%", "dumping"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderEstimating: the sizing prelude renders a per-DLE table — state, measured
// size, and how long each measurement is taking/took — not just the done counter, so
// a slow estimate names its culprit like the dump table does.
func TestRenderEstimating(t *testing.T) {
	c := newClock()
	tr := NewTracker("estimate", PhaseEstimating, 2, []Plan{
		{Name: "alpha"}, {Name: "bravo"}, {Name: "charlie"},
	}, c.now, nil)
	tr.StartDLE("alpha")
	c.advance(2 * time.Second)
	tr.FinishDLE("alpha", 0, 4096, 0, nil) // sized: 4.10 kB in 2s
	tr.StartDLE("bravo")
	c.advance(5 * time.Second) // bravo still sizing after 5s; charlie pending

	var sb strings.Builder
	Render(&sb, tr.Snapshot(), c.now())
	out := sb.String()
	for _, want := range []string{
		"1 of 3 DLEs measured, 1 running",
		"alpha", "sized", "4.10 kB", "2s",
		"bravo", "sizing", "5s",
		"charlie", "pending",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("estimating render missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderDraining: a DLE draining off a holding disk renders its "flushing" state annotated
// with the disk it landed on, so a multi-disk run shows where each buffered.
func TestRenderDraining(t *testing.T) {
	c := newClock()
	tr := NewTracker("run-2026-06-23.001", PhaseRunning, 2, plan(), c.now, nil)
	tr.StartDLE("alpha")
	tr.FinishDLE("alpha", 3, 300, 120, nil)
	tr.StartFlush("alpha", "scratch2")

	snap := tr.Snapshot()
	var got *DLE
	for i := range snap.DLEs {
		if snap.DLEs[i].Name == "alpha" {
			got = &snap.DLEs[i]
		}
	}
	if got == nil || got.State != StateFlushing || got.Holding != "scratch2" {
		t.Fatalf("StartFlush must set state=flushing, holding=scratch2; got %+v", got)
	}

	var sb strings.Builder
	Render(&sb, snap, c.now())
	if out := sb.String(); !strings.Contains(out, "flushing←scratch2") {
		t.Errorf("render must annotate the draining DLE with its holding disk; got:\n%s", out)
	}
}

// TestDrainProgressAndRates drives a holding-disk DLE through dump -> drain -> done and
// checks the two pipelines are metered independently: the drain bar tracks copied bytes,
// the dump rate freezes when dumping ends (not decaying through the drain tail), and the
// drain rate is measured over its own window.
func TestDrainProgressAndRates(t *testing.T) {
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 1, []Plan{{Name: "alpha", Level: 0, EstBytes: 1000}}, c.now, nil)

	tr.StartDLE("alpha")
	c.advance(10 * time.Second)
	tr.FinishDLE("alpha", 1, 1000, 800, nil) // dumped 1000 uncompressed -> 800 staged, in 10s

	// Dumping is done: the dump rate must freeze at 100 B/s regardless of later elapsed.
	dumpEnd := c.now()
	tr.StartFlush("alpha", "scratch")
	c.advance(8 * time.Second)
	tr.AddDrainBytes("alpha", "landing", 400) // 400 of 800 copied in 8s -> 50 B/s drain

	snap := tr.Snapshot()
	now := c.now()
	a := snap.DLEs[0]
	if a.State != StateFlushing || !a.Drains() {
		t.Fatalf("alpha should be draining: %+v", a)
	}
	if got := a.DrainPct(); got != 50 {
		t.Fatalf("drain pct = %.0f, want 50", got)
	}
	if got := a.OnVolume(); got != 400 {
		t.Fatalf("on-volume = %d, want 400", got)
	}
	if got := snap.Rate(now); got != 100 {
		t.Fatalf("dump rate = %.2f, want 100 (frozen at dump end, %v)", got, dumpEnd)
	}
	if got := snap.DrainRate(now); got != 50 {
		t.Fatalf("drain rate = %.2f, want 50", got)
	}
	if got := snap.DrainPct(); got != 50 {
		t.Fatalf("run drain pct = %.0f, want 50", got)
	}

	// The flush bar and a separate Flush footer line must both appear.
	var sb strings.Builder
	Render(&sb, snap, now)
	out := sb.String()
	for _, want := range []string{"DUMP", "FLUSH", "Flush:", "Volume:"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}

	tr.FinishFlush("alpha")
	if got := tr.Snapshot().DLEs[0].DrainBytes; got != 800 {
		t.Fatalf("FinishFlush should settle drain to staged size 800, got %d", got)
	}
}

// TestStagedHoldingNotDirect: a DLE whose dump has committed to a holding disk but is still
// queued behind another DLE's drain (StageHolding called, StartFlush not yet) must read as
// draining — a 0% flush bar, not the misleading "direct" of a dump that bypassed holding.
func TestStagedHoldingNotDirect(t *testing.T) {
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 1, []Plan{{Name: "alpha", Level: 0, EstBytes: 1000}}, c.now, nil)
	tr.StartDLE("alpha")
	tr.StageHolding("alpha", "scratch") // committed to holding...
	tr.FinishDLE("alpha", 1, 1000, 800, nil)
	// ...but the drainer has not reached it yet, so no StartFlush / AddDrainBytes.

	snap := tr.Snapshot()
	a := snap.DLEs[0]
	if !a.Drains() {
		t.Fatalf("a DLE staged on holding must read as draining; got %+v", a)
	}
	if got := a.DrainPct(); got != 0 {
		t.Fatalf("a staged-but-not-yet-drained DLE must show 0%% drain, got %.0f", got)
	}
	var sb strings.Builder
	Render(&sb, snap, c.now())
	if out := sb.String(); strings.Contains(out, "direct") {
		t.Errorf("a DLE buffered on holding must not render as \"direct\":\n%s", out)
	}
}

// TestStagingToHolding: while a holding-bound DLE is still dumping to its disk (MarkToHolding
// called, dump not committed), its FLUSH cell reads "staging" — not the misleading "-"/"direct"
// of a dump that bypassed holding — and its VOLUME is 0, since the bytes are on holding, not the
// authoritative volume yet.
func TestStagingToHolding(t *testing.T) {
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 1, []Plan{{Name: "alpha", Level: 0, EstBytes: 1000}}, c.now, nil)
	tr.StartDLE("alpha")
	tr.MarkToHolding("alpha")      // routed to holding...
	tr.AddBytes("alpha", 500, 400) // ...still dumping there: 400 compressed on holding, not landed

	snap := tr.Snapshot()
	a := snap.DLEs[0]
	if a.Drains() {
		t.Fatalf("a DLE still dumping to holding has not committed, so must not read as draining: %+v", a)
	}
	if got := a.OnVolume(); got != 0 {
		t.Fatalf("staging on-volume = %d, want 0 (bytes are on holding, not the volume)", got)
	}
	var sb strings.Builder
	Render(&sb, snap, c.now())
	out := sb.String()
	if !strings.Contains(out, "staging") {
		t.Errorf("a DLE dumping to holding should show \"staging\" in its drain cell:\n%s", out)
	}
	if strings.Contains(out, "direct") {
		t.Errorf("a holding-bound dump must not render as \"direct\":\n%s", out)
	}
}

// TestDirectDumpNoDrain confirms a DLE that never goes through a holding disk shows no
// drain phase: no Drain footer, "direct" in its drain cell, and volume = compressed out.
func TestDirectDumpNoDrain(t *testing.T) {
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 1, []Plan{{Name: "alpha", Level: 0, EstBytes: 1000}}, c.now, nil)
	tr.StartDLE("alpha")
	tr.FinishDLE("alpha", 1, 1000, 800, nil)

	snap := tr.Snapshot()
	if snap.TotalToDrain() != 0 {
		t.Fatalf("direct dump must not count toward drain: %d", snap.TotalToDrain())
	}
	if got := snap.DLEs[0].OnVolume(); got != 800 {
		t.Fatalf("direct on-volume = %d, want 800 (compressed out)", got)
	}
	var sb strings.Builder
	Render(&sb, snap, c.now())
	out := sb.String()
	if strings.Contains(out, "Flush:") {
		t.Errorf("direct-only run must not render a Flush line:\n%s", out)
	}
	if !strings.Contains(out, "direct") {
		t.Errorf("a done direct dump should show \"direct\" in its drain cell:\n%s", out)
	}
}

// TestAtomicWriteNoTemp confirms the sink leaves no stray temp file behind.
func TestAtomicWriteNoTemp(t *testing.T) {
	dir := t.TempDir()
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 1, plan(), c.now, NewFileSink(dir, c.now))
	tr.SetPhase(PhaseDone)
	if _, err := Load(dir); err != nil {
		t.Fatalf("status file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, StatusFileName+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind (stat err = %v)", err)
	}
}

// TestFanoutDrainProgress: a two-landing route doubles the DLE's to-drain total, its
// bar reaches 100% only when both landings hold the archive, OnVolume counts only the
// primary's share (no double-counting), and the footer itemizes each landing with its
// own rate.
func TestFanoutDrainProgress(t *testing.T) {
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 1, []Plan{{Name: "alpha", Level: 0, EstBytes: 1000, Landings: []string{"s3", "gdrive"}}}, c.now, nil)

	tr.StartDLE("alpha")
	c.advance(10 * time.Second)
	tr.FinishDLE("alpha", 1, 1000, 800, nil)
	tr.StartFlush("alpha", "scratch")
	c.advance(8 * time.Second)
	tr.AddDrainBytes("alpha", "s3", 800)     // primary done
	tr.AddDrainBytes("alpha", "gdrive", 400) // secondary halfway

	snap := tr.Snapshot()
	a := snap.DLEs[0]
	if got := a.DrainPct(); got != 75 { // 1200 of 1600
		t.Fatalf("drain pct = %.0f, want 75", got)
	}
	if got := a.OnVolume(); got != 800 {
		t.Fatalf("on-volume = %d, want the primary's 800, not the fan-out sum", got)
	}
	drains := snap.LandingDrains()
	if len(drains) != 2 || drains[0].Landing != "s3" || drains[1].Landing != "gdrive" {
		t.Fatalf("LandingDrains = %+v; want s3 then gdrive", drains)
	}
	if drains[0].Done != 800 || drains[0].Total != 800 || drains[1].Done != 400 || drains[1].Total != 800 {
		t.Fatalf("LandingDrains = %+v; want s3 800/800, gdrive 400/800", drains)
	}

	var sb strings.Builder
	Render(&sb, snap, c.now())
	out := sb.String()
	for _, want := range []string{"s3", "gdrive", "Flush:"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}

	tr.FinishFlush("alpha")
	if got := tr.Snapshot().DLEs[0].DrainBytes; got != 1600 {
		t.Fatalf("FinishFlush should settle to staged x landings = 1600, got %d", got)
	}
}

// TestSkipLandingTellsTheTruth: a landing the run declared unusable up front (failed to
// open, or refused at make-room) leaves every DLE's route and lands on the snapshot with
// its reason — so no drain total counts it and FinishFlush cannot settle a copy that never
// happened (the status-file lie where a skipped landing read as fully drained).
func TestSkipLandingTellsTheTruth(t *testing.T) {
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 1, []Plan{{Name: "alpha", Level: 0, EstBytes: 1000, Landings: []string{"s3", "tape"}}}, c.now, nil)
	tr.SkipLanding("tape", "open landing: no writable volume")

	snap := tr.Snapshot()
	if len(snap.Skipped) != 1 || snap.Skipped[0].Landing != "tape" || !strings.Contains(snap.Skipped[0].Reason, "no writable volume") {
		t.Fatalf("skipped = %+v", snap.Skipped)
	}
	if got := snap.DLEs[0].Landings; len(got) != 1 || got[0] != "s3" {
		t.Fatalf("route = %v; want the skipped landing removed", got)
	}

	// Dump and drain to the surviving lane only, exactly as the spool would.
	tr.StartDLE("alpha")
	tr.FinishDLE("alpha", 1, 1000, 800, nil)
	tr.StartFlush("alpha", "scratch")
	tr.AddDrainBytes("alpha", "s3", 800)
	tr.FinishFlush("alpha")

	d := tr.Snapshot().DLEs[0]
	if _, lied := d.Drained["tape"]; lied {
		t.Fatalf("drained = %v; a skipped landing must never read as drained", d.Drained)
	}
	if d.Drained["s3"] != 800 || d.DrainBytes != 800 || d.DrainPct() != 100 {
		t.Fatalf("surviving lane: drained=%v drainBytes=%d pct=%.0f; want 800/800 at 100%%", d.Drained, d.DrainBytes, d.DrainPct())
	}

	var sb strings.Builder
	Render(&sb, tr.Snapshot(), c.now())
	out := sb.String()
	for _, want := range []string{"SKIPPED landing tape", "nb sync --to tape"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}
}

// TestFinishFlushDoesNotSettleVoidedLane: a lane whose drain failed mid-run has its meter
// voided to 0 by the spool (the failed copy never committed, so nothing of it is on the
// landing); FinishFlush must leave it at 0 — the missing copy stays visible — instead of
// settling it to the staged size like a landed lane.
func TestFinishFlushDoesNotSettleVoidedLane(t *testing.T) {
	c := newClock()
	tr := NewTracker("run", PhaseRunning, 1, []Plan{{Name: "alpha", Level: 0, EstBytes: 1000, Landings: []string{"s3", "gdrive"}}}, c.now, nil)
	tr.StartDLE("alpha")
	tr.FinishDLE("alpha", 1, 1000, 800, nil)
	tr.StartFlush("alpha", "scratch")
	tr.AddDrainBytes("alpha", "s3", 800)
	tr.AddDrainBytes("alpha", "gdrive", 300) // partial copy...
	tr.AddDrainBytes("alpha", "gdrive", 0)   // ...failed: the spool voids the meter
	tr.FinishFlush("alpha")

	d := tr.Snapshot().DLEs[0]
	if d.Drained["gdrive"] != 0 || d.Drained["s3"] != 800 || d.DrainBytes != 800 {
		t.Fatalf("drained=%v drainBytes=%d; the failed lane must stay at 0, the landed one settle to 800", d.Drained, d.DrainBytes)
	}
	if got := d.DrainPct(); got == 100 {
		t.Fatalf("drain pct = %.0f; a missing copy must keep the drain short of 100%%", got)
	}
}
