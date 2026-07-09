package report

import (
	"testing"
	"time"
)

func trendRun(at time.Time, stats ...DLEStat) Run {
	return Run{Command: CommandDump, StartedAt: at, EndedAt: at.Add(time.Minute),
		Outcome: OutcomeSuccess, DumpStats: stats}
}

// TestDLETrends checks the gather: dump records (fed newest-first, as the web
// history reads them) come back oldest-first per DLE, non-dump records are
// ignored, and every DLE lands in the map.
func TestDLETrends(t *testing.T) {
	base := time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC)
	runs := []Run{
		trendRun(base.Add(48*time.Hour), DLEStat{DLE: "a", Level: 1, Orig: 30, Out: 3}),
		{Command: CommandPrune, StartedAt: base.Add(36 * time.Hour)},
		trendRun(base.Add(24*time.Hour), DLEStat{DLE: "a", Level: 1, Orig: 20, Out: 2}, DLEStat{DLE: "b", Level: 0, Orig: 9, Out: 9}),
		trendRun(base, DLEStat{DLE: "a", Level: 0, Orig: 10, Out: 1}),
	}
	trends := DLETrends(runs)
	a := trends["a"]
	if len(a) != 3 || !a[0].At.Before(a[1].At) || !a[1].At.Before(a[2].At) {
		t.Fatalf("a = %+v, want 3 points oldest-first", a)
	}
	if a[0].Level != 0 || a[0].Out != 1 || a[2].Out != 3 {
		t.Errorf("a points = %+v", a)
	}
	if len(trends["b"]) != 1 {
		t.Errorf("b = %+v", trends["b"])
	}
	if got := DLETrend(runs, "a"); len(got) != 3 {
		t.Errorf("DLETrend(a) = %+v", got)
	}
}

// TestSummarizeTrend checks the evolution summary: growth rate and percent over
// the windowed fulls, incremental median alongside, fulls older than the window
// (anchored at the newest point) excluded, and shrinkage reported signed.
func TestSummarizeTrend(t *testing.T) {
	day := 24 * time.Hour
	base := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
	gib := int64(1 << 30)

	pts := []TrendPoint{
		{At: base, Level: 0, Out: 999 * gib}, // predates the window: must not skew the rate
		{At: base.Add(210 * day), Level: 0, Out: 100 * gib},
		{At: base.Add(215 * day), Level: 1, Out: 2 * gib},
		{At: base.Add(217 * day), Level: 1, Out: 4 * gib},
		{At: base.Add(219 * day), Level: 1, Out: 30 * gib},
		{At: base.Add(310 * day), Level: 0, Out: 150 * gib},
	}
	ev, ok := SummarizeTrend(pts)
	if !ok {
		t.Fatal("ok = false")
	}
	if ev.From.Out != 100*gib || ev.To.Out != 150*gib {
		t.Errorf("From/To = %v/%v — the pre-window full leaked in", ev.From.Out, ev.To.Out)
	}
	if ev.Pct != 50 {
		t.Errorf("Pct = %d, want 50", ev.Pct)
	}
	if want := int64(float64(50*gib) / 100); ev.PerDay != want {
		t.Errorf("PerDay = %d, want %d", ev.PerDay, want)
	}
	if ev.IncrMedian != 4*gib {
		t.Errorf("IncrMedian = %d, want %d", ev.IncrMedian, 4*gib)
	}

	// Shrinking dataset: signed, still ok — honest info, unlike a fill projection.
	shrink, ok := SummarizeTrend([]TrendPoint{
		{At: base, Level: 0, Out: 100 * gib},
		{At: base.Add(100 * day), Level: 0, Out: 80 * gib},
	})
	if !ok || shrink.Pct != -20 || shrink.PerDay >= 0 {
		t.Errorf("shrink = %+v ok=%v, want Pct -20 and negative PerDay", shrink, ok)
	}

	// Too little baseline: one full, or two inside a day.
	if _, ok := SummarizeTrend([]TrendPoint{{At: base, Level: 0, Out: gib}}); ok {
		t.Error("single full summarized")
	}
	if _, ok := SummarizeTrend([]TrendPoint{
		{At: base, Level: 0, Out: gib}, {At: base.Add(time.Hour), Level: 0, Out: 2 * gib},
	}); ok {
		t.Error("sub-day span summarized")
	}
	if _, ok := SummarizeTrend(nil); ok {
		t.Error("empty series summarized")
	}
}
