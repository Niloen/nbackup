package scheduler

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/report"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// pt is a terse TrendPoint builder for the tests.
func pt(date string, level int, orig int64) report.TrendPoint {
	return report.TrendPoint{At: day(date), Level: level, Orig: orig}
}

func TestProjectNeverFulled(t *testing.T) {
	src := historySource{at: day("2026-07-10")}
	// LastFullDate empty means never fulled: size is unknown (0), so the planner's
	// mandatory-full rule still schedules it — only the byte column is blank.
	est := src.project(&catalog.DLEState{}, nil)
	if est.Full != 0 || est.Incr != 0 || est.IncrNext != 0 {
		t.Fatalf("never-fulled DLE should have zero (unknown) sizes, got %+v", est)
	}
}

func TestProjectFullGrowsBySlope(t *testing.T) {
	// Two fulls 10 days apart, +100 GB of uncompressed growth → +10 GB/day. The last
	// full was 2026-07-01 at 200 GB; projecting to 2026-07-11 (10 days on) adds another
	// 100 GB → 300 GB.
	const gb = int64(1) << 30
	pts := []report.TrendPoint{
		pt("2026-06-21", 0, 100*gb),
		pt("2026-07-01", 0, 200*gb),
	}
	st := &catalog.DLEState{
		LastFullDate: "2026-07-01",
		Runs:         []catalog.RunRecord{{Date: "2026-07-01", Level: 0}},
	}
	src := historySource{at: day("2026-07-11")}
	est := src.project(st, pts)
	if want := 300 * gb; est.Full != want {
		t.Fatalf("projected full = %d, want %d (last 200GB + 10 days * 10GB/day)", est.Full, want)
	}
}

func TestAtRepinGrowsFullOverHorizon(t *testing.T) {
	// The forecast repins the projection per simulated day (historySource.At): with a
	// positive growth slope, a full sized further out is bigger than one sized nearer —
	// this is what makes the capacity/cost forecast reflect dataset drift.
	const gb = int64(1) << 30
	pts := []report.TrendPoint{
		pt("2026-06-21", 0, 100*gb),
		pt("2026-07-01", 0, 200*gb), // +100 GB over 10 days = 10 GB/day
	}
	st := &catalog.DLEState{
		LastFullDate: "2026-07-01",
		Runs:         []catalog.RunRecord{{Date: "2026-07-01", Level: 0}},
	}
	near := historySource{at: day("2026-07-11")}.project(st, pts)
	far := historySource{at: day("2026-07-31")}.project(st, pts)
	if !(far.Full > near.Full) {
		t.Fatalf("a later horizon should project a bigger full: near=%d far=%d", near.Full, far.Full)
	}
	if want := 300 * gb; near.Full != want { // 200 + 10 days * 10 GB/day
		t.Errorf("near full = %d, want %d", near.Full, want)
	}
}

func TestProjectFullFlatWithoutSlope(t *testing.T) {
	// A single full gives no slope (SummarizeTrend not ok): Full is the last full flat,
	// not zero and not a guessed rate.
	const gb = int64(1) << 30
	pts := []report.TrendPoint{pt("2026-07-01", 0, 200*gb)}
	st := &catalog.DLEState{
		LastFullDate: "2026-07-01",
		Runs:         []catalog.RunRecord{{Date: "2026-07-01", Level: 0}},
	}
	src := historySource{at: day("2026-07-31")}
	if est := src.project(st, pts); est.Full != 200*gb {
		t.Fatalf("single-full projection = %d, want flat %d", est.Full, 200*gb)
	}
}

func TestProjectIncrPerLevel(t *testing.T) {
	// Sitting at level 1 (last run L1). Incr = median of L1 dumps (10,20,30 → 20);
	// IncrNext = median of L2 dumps (there is one, 5). Level buckets stay separate,
	// mirroring the probe's L-vs-L+1 split.
	const mb = int64(1) << 20
	pts := []report.TrendPoint{
		pt("2026-06-21", 0, 1000*mb),
		pt("2026-07-01", 0, 1000*mb),
		pt("2026-07-02", 1, 10*mb),
		pt("2026-07-03", 1, 20*mb),
		pt("2026-07-04", 1, 30*mb),
		pt("2026-07-05", 2, 5*mb),
	}
	st := &catalog.DLEState{
		LastFullDate: "2026-07-01",
		Runs:         []catalog.RunRecord{{Level: 0}, {Level: 1}},
	}
	src := historySource{at: day("2026-07-06")}
	est := src.project(st, pts)
	if est.Incr != 20*mb {
		t.Errorf("Incr (L1 median) = %d, want %d", est.Incr, 20*mb)
	}
	if est.IncrNext != 5*mb {
		t.Errorf("IncrNext (L2 median) = %d, want %d", est.IncrNext, 5*mb)
	}
}

func TestMedianIncrSinceFullEmpty(t *testing.T) {
	// No incrementals since the full → 0, which the planner reads as "not yet estimable"
	// and holds the level (the safe default, and honest right after a full).
	if got := medianIncrSinceFull(nil, 3, day("2026-07-01")); got != 0 {
		t.Fatalf("incr since full of no L3 points = %d, want 0", got)
	}
}

// TestProjectIncrSinceLastFull pins the rule that only incrementals since the last full
// size the recurring estimate. The shape is from a real partitioned photo DLE: a ~15 GB
// full, and TWO ~14.85 GB "L1" dumps a couple days BEFORE it (one growth event that
// re-dumped nearly the whole dataset over two runs) plus the real daily churn of ~140 kB
// AFTER it. Folding the pre-full re-dumps in would project ~15 GB onto every incremental
// night; scoping to dumps since the last full leaves only the ~140 kB, the true churn
// against the current baseline (the re-dumps' bytes are already in that full).
func TestProjectIncrSinceLastFull(t *testing.T) {
	const kb, mb, gb = int64(1) << 10, int64(1) << 20, int64(1) << 30
	rebaseline := 14*gb + 850*mb // ~14.85 GB, before the last full
	pts := []report.TrendPoint{
		pt("2026-07-08", 0, 8*gb),
		pt("2026-07-09", 1, rebaseline),
		pt("2026-07-09", 1, rebaseline),
		pt("2026-07-11", 0, 15*gb),  // the last full re-bases the dataset
		pt("2026-07-13", 1, 142*kb), // the real recurring churn, since that full
	}
	st := &catalog.DLEState{
		LastFullDate: "2026-07-11",
		Runs:         []catalog.RunRecord{{Level: 0}, {Level: 1}},
	}
	est := historySource{at: day("2026-07-14")}.project(st, pts)
	if est.Incr > 10*mb {
		t.Errorf("Incr = %d B (~%d MB); a pre-full re-dump leaked into the recurring estimate", est.Incr, est.Incr/mb)
	}
}
