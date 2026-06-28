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
	tr := NewTracker("slot-2026-06-23", PhaseRunning, 2, plan(), c.now, nil)

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
	tr := NewTracker("slot", PhaseRunning, 1, plan(), c.now, nil)
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

// TestRateAndETA checks throughput and ETA derive from elapsed time and the
// remaining estimate.
func TestRateAndETA(t *testing.T) {
	c := newClock()
	tr := NewTracker("slot", PhaseRunning, 1, plan(), c.now, nil)
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
	tr := NewTracker("slot-x", PhaseRunning, 1, plan(), c.now, NewFileSink(dir, c.now))

	tr.StartDLE("alpha")
	tr.FinishDLE("alpha", 1, 300, 100, nil)
	tr.SetPhase(PhaseDone)

	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if snap.SlotID != "slot-x" || snap.Phase != PhaseDone {
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

	tr := NewTracker("slot", PhaseRunning, 1, plan(), c.now, sink) // forced initial write
	tr.StartDLE("alpha")                                           // forced

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
	tr := NewTracker("slot-2026-06-23", PhaseRunning, 2, plan(), c.now, nil)
	tr.StartDLE("alpha")
	c.advance(5 * time.Second)
	tr.AddBytes("alpha", 150, 60)

	var sb strings.Builder
	Render(&sb, tr.Snapshot(), c.now())
	out := sb.String()
	for _, want := range []string{"slot-2026-06-23", "running", "alpha", "50%", "dumping"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderDraining: a DLE draining off a holding disk renders its "flushing" state annotated
// with the disk it landed on, so a multi-disk run shows where each buffered.
func TestRenderDraining(t *testing.T) {
	c := newClock()
	tr := NewTracker("slot-2026-06-23", PhaseRunning, 2, plan(), c.now, nil)
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
	tr := NewTracker("slot", PhaseRunning, 1, []Plan{{Name: "alpha", Level: 0, EstBytes: 1000}}, c.now, nil)

	tr.StartDLE("alpha")
	c.advance(10 * time.Second)
	tr.FinishDLE("alpha", 1, 1000, 800, nil) // dumped 1000 uncompressed -> 800 staged, in 10s

	// Dumping is done: the dump rate must freeze at 100 B/s regardless of later elapsed.
	dumpEnd := c.now()
	tr.StartFlush("alpha", "scratch")
	c.advance(8 * time.Second)
	tr.AddDrainBytes("alpha", 400) // 400 of 800 copied in 8s -> 50 B/s drain

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

// TestDirectDumpNoDrain confirms a DLE that never goes through a holding disk shows no
// drain phase: no Drain footer, "direct" in its drain cell, and volume = compressed out.
func TestDirectDumpNoDrain(t *testing.T) {
	c := newClock()
	tr := NewTracker("slot", PhaseRunning, 1, []Plan{{Name: "alpha", Level: 0, EstBytes: 1000}}, c.now, nil)
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
	tr := NewTracker("slot", PhaseRunning, 1, plan(), c.now, NewFileSink(dir, c.now))
	tr.SetPhase(PhaseDone)
	if _, err := Load(dir); err != nil {
		t.Fatalf("status file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, StatusFileName+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind (stat err = %v)", err)
	}
}
