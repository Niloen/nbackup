package planner

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
)

func dleNamed(h string) config.DLE { return config.DLE{Host: h, Path: "/data"} }

// levelOf returns the planned level for a DLE name.
func levelOf(p *Plan, name string) int {
	for _, it := range p.Items {
		if it.Name == name {
			return it.Level
		}
	}
	return -99
}

func fullsIn(p *Plan) int {
	n := 0
	for _, it := range p.Items {
		if it.Level == 0 {
			n++
		}
	}
	return n
}

func TestLevelDecisions(t *testing.T) {
	today := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	mk := func(d *catalog.DLEState) *Plan {
		hist := &catalog.History{DLEs: map[string]*catalog.DLEState{"h-data": d}}
		return Build([]config.DLE{dleNamed("h")}, hist, nil, Params{CycleDays: 7}, today)
	}

	// No prior full -> mandatory full.
	if lvl := levelOf(mk(&catalog.DLEState{}), "h-data"); lvl != 0 {
		t.Errorf("first backup: got L%d, want L0", lvl)
	}
	// Recent full -> incremental L1.
	recent := &catalog.DLEState{LastFullDate: today.AddDate(0, 0, -1).Format("2006-01-02")}
	if lvl := levelOf(mk(recent), "h-data"); lvl != 1 {
		t.Errorf("recent full: got L%d, want L1", lvl)
	}
	// At/past the cycle deadline -> forced full. The cycle is a hard ceiling, so a
	// full never ages past it.
	due := &catalog.DLEState{LastFullDate: today.AddDate(0, 0, -7).Format("2006-01-02")}
	if lvl := levelOf(mk(due), "h-data"); lvl != 0 {
		t.Errorf("at cycle deadline: got L%d, want L0", lvl)
	}
}

func TestMultilevelClimb(t *testing.T) {
	today := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	d := &catalog.DLEState{
		LastFullDate: "2026-06-20",
		LastFullSlot: "slot-2026-06-20",
		Runs: []catalog.RunRecord{
			{Date: "2026-06-20", Slot: "slot-2026-06-20", Level: 0},
			{Date: "2026-06-21", Slot: "slot-2026-06-21", Level: 1},
		},
	}
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{"h-data": d}}
	p := Build([]config.DLE{dleNamed("h")}, hist, nil, Params{CycleDays: 7}, today)
	if lvl := levelOf(p, "h-data"); lvl != 2 {
		t.Errorf("after L0,L1 the next level should be L2, got L%d", lvl)
	}
}

// TestPromotionFillsLightRun checks that when two equal DLEs share a deadline
// tomorrow (a future peak), the planner pulls exactly one forward to today so the
// pile-up is spread across two days rather than landing on one.
func TestPromotionFillsLightRun(t *testing.T) {
	today := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{}}
	var dles []config.DLE
	est := map[string]Estimate{}
	for _, h := range []string{"a", "b"} {
		d := dleNamed(h)
		// Last full 6 days ago: with a 7-day cycle the deadline is tomorrow.
		hist.DLEs[d.Name()] = &catalog.DLEState{
			LastFullDate: today.AddDate(0, 0, -6).Format("2006-01-02"),
			Runs:         []catalog.RunRecord{{Date: "old", Slot: "slot-x", Level: 0}},
		}
		dles = append(dles, d)
		est[d.Name()] = Estimate{Full: 100, Incr: 10}
	}
	p := Build(dles, hist, est, Params{CycleDays: 7, RoomBytes: -1}, today)
	if n := fullsIn(p); n != 1 {
		t.Errorf("expected exactly one DLE promoted to spread a shared deadline, got %d fulls", n)
	}
}

// TestPromotionBoundedByRoom checks promotion never pushes a run past the per-run
// capacity room: the same shared-deadline pair is left as incrementals when there
// is no room to pull a full forward.
func TestPromotionBoundedByRoom(t *testing.T) {
	today := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{}}
	var dles []config.DLE
	est := map[string]Estimate{}
	for _, h := range []string{"a", "b"} {
		d := dleNamed(h)
		hist.DLEs[d.Name()] = &catalog.DLEState{
			LastFullDate: today.AddDate(0, 0, -6).Format("2006-01-02"),
			Runs:         []catalog.RunRecord{{Date: "old", Slot: "slot-x", Level: 0}},
		}
		dles = append(dles, d)
		est[d.Name()] = Estimate{Full: 100, Incr: 10}
	}
	// Baseline run = 2 incrementals = 20. Promoting one would make it 110; room 50
	// forbids it, so nothing is promoted.
	p := Build(dles, hist, est, Params{CycleDays: 7, RoomBytes: 50}, today)
	if n := fullsIn(p); n != 0 {
		t.Errorf("expected no promotion under tight room, got %d fulls", n)
	}
}

// TestPromotionDoesNotChaseAverage is the core skew guard: a single big DLE far
// from its deadline is NOT pulled forward to fill a light run. Leveling the
// deadline calendar (not a daily byte average) means an irreducible lone full is
// only ever done at the last responsible moment, never re-fulled early.
func TestPromotionDoesNotChaseAverage(t *testing.T) {
	today := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{
		"big-data": {
			// Fulled yesterday: 6 days of cycle left, far from the deadline.
			LastFullDate: today.AddDate(0, 0, -1).Format("2006-01-02"),
			Runs:         []catalog.RunRecord{{Date: "old", Slot: "slot-x", Level: 0}},
		},
	}}
	est := map[string]Estimate{"big-data": {Full: 1_000_000_000, Incr: 1000}}
	// A huge free room: an average-chasing planner would re-full the big DLE today
	// to "use" it. Calendar leveling must not.
	p := Build([]config.DLE{dleNamed("big")}, hist, est, Params{CycleDays: 7, RoomBytes: -1}, today)
	if lvl := levelOf(p, "big-data"); lvl == 0 {
		t.Errorf("a big DLE far from its deadline was promoted (chasing an average); want an incremental")
	}
}

// TestPromotionDoesNotOverFullBigDLE checks the skew guard over a window: with one
// big DLE and many small ones, the big DLE is fulled about once per cycle, not
// repeatedly pulled forward to flatten daily volume.
func TestPromotionDoesNotOverFullBigDLE(t *testing.T) {
	start := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{}}
	var dles []config.DLE
	est := map[string]Estimate{}
	dles = append(dles, dleNamed("big"))
	est["big-data"] = Estimate{Full: 1_000_000, Incr: 1000}
	for _, h := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		dles = append(dles, dleNamed(h))
		est[dleNamed(h).Name()] = Estimate{Full: 10, Incr: 1}
	}

	plans := Simulate(dles, hist, est, Params{CycleDays: 7, RoomBytes: -1}, start, 21)
	bigFulls := 0
	for _, p := range plans {
		if levelOf(p, "big-data") == 0 {
			bigFulls++
		}
	}
	// Day 0 is the mandatory bootstrap full; deadlines then fall ~day 7 and ~day 14.
	// Four fulls over 21 days is the cycle cadence; more means it was over-promoted.
	if bigFulls > 4 {
		t.Errorf("big DLE fulled %d times in 21 days (cycle 7); promotion is over-fulling it", bigFulls)
	}
	if bigFulls < 3 {
		t.Errorf("big DLE only fulled %d times in 21 days; it should full once per cycle", bigFulls)
	}
}

// TestPromotionStaggersLockstepFulls checks that two big DLEs whose last fulls
// coincide are spread onto different days rather than piling onto a shared
// deadline. Promotion pulls one forward at the last responsible moment, draining
// the lock-step one per run and staggering their cycles apart.
func TestPromotionStaggersLockstepFulls(t *testing.T) {
	start := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{}}
	var dles []config.DLE
	est := map[string]Estimate{}
	day0 := start.Format("2006-01-02")
	for _, h := range []string{"downloads", "videos"} {
		d := dleNamed(h)
		hist.DLEs[d.Name()] = &catalog.DLEState{
			LastFullDate: day0,
			LastFullSlot: "slot-x",
			Runs:         []catalog.RunRecord{{Date: day0, Slot: "slot-x", Level: 0}},
		}
		dles = append(dles, d)
		est[d.Name()] = Estimate{Full: 3_300_000_000, Incr: 50_000}
	}

	plans := Simulate(dles, hist, est, Params{CycleDays: 7, RoomBytes: -1}, start, 30)

	fullsPerDay := 0
	bothSameDay := 0
	for _, p := range plans {
		n := fullsIn(p)
		fullsPerDay += n
		if n > 1 {
			bothSameDay++
		}
	}
	if bothSameDay != 0 {
		t.Errorf("lock-step DLEs piled onto %d shared day(s); fulls must stagger to one per run", bothSameDay)
	}
	// Both DLEs still get fulled regularly across the window: the fix must not
	// starve fulls, only spread them.
	if fullsPerDay < 6 {
		t.Errorf("expected the big DLEs to keep fulling across the window, got %d fulls total", fullsPerDay)
	}
}

// TestCycleCapacityWarning checks the structural cycle check: when one full of
// every DLE cannot fit capacity, the plan carries a warning (recoverability is
// at risk) but still schedules the backups.
func TestCycleCapacityWarning(t *testing.T) {
	today := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{}}
	var dles []config.DLE
	est := map[string]Estimate{}
	for _, h := range []string{"a", "b", "c"} {
		d := dleNamed(h)
		dles = append(dles, d)
		est[d.Name()] = Estimate{Full: 100, Incr: 10} // total full 300
	}

	// Capacity 250 < total full 300 -> warn.
	p := Build(dles, hist, est, Params{CycleDays: 7, CapacityBytes: 250}, today)
	if len(p.Warnings) == 0 {
		t.Errorf("expected a structural warning when a recovery set (300) exceeds capacity (250)")
	}
	if len(p.Items) != 3 {
		t.Errorf("backups must still be scheduled despite the warning, got %d items", len(p.Items))
	}

	// Capacity 400 >= total full 300 -> no warning.
	p = Build(dles, hist, est, Params{CycleDays: 7, CapacityBytes: 400}, today)
	if len(p.Warnings) != 0 {
		t.Errorf("did not expect a warning when the recovery set fits, got %v", p.Warnings)
	}

	// Unbounded capacity (0) -> no warning.
	p = Build(dles, hist, est, Params{CycleDays: 7, CapacityBytes: 0}, today)
	if len(p.Warnings) != 0 {
		t.Errorf("unbounded capacity should not warn, got %v", p.Warnings)
	}
}

// TestSimulateSchedule checks the forward forecast advances history between days:
// a fresh DLE fulls on day 0, climbs incrementals through the cycle, and fulls
// again at the deadline — and the caller's history is left untouched. A lone DLE
// is never promoted (it cannot reduce its own peak), so the schedule is clean.
func TestSimulateSchedule(t *testing.T) {
	start := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{}}
	dles := []config.DLE{dleNamed("h")}
	est := map[string]Estimate{"h-data": {Full: 100, Incr: 10}}

	plans := Simulate(dles, hist, est, Params{CycleDays: 7, RoomBytes: -1}, start, 15)
	if len(plans) != 15 {
		t.Fatalf("want 15 plans, got %d", len(plans))
	}
	want := []int{0, 1, 2, 3, 4, 5, 6, 0, 1, 2, 3, 4, 5, 6, 0}
	for i, p := range plans {
		if !p.Date.Equal(start.AddDate(0, 0, i)) {
			t.Errorf("day %d: plan date %s, want %s", i, p.Date, start.AddDate(0, 0, i))
		}
		if lvl := levelOf(p, "h-data"); lvl != want[i] {
			t.Errorf("day %d: got L%d, want L%d", i, lvl, want[i])
		}
	}
	// The forecast clones history; the caller's copy must be unmodified.
	if len(hist.DLEs) != 0 {
		t.Errorf("Simulate mutated the input history: %v", hist.DLEs)
	}
}

// TestSimulateClampsDays checks a non-positive day count still yields one plan.
func TestSimulateClampsDays(t *testing.T) {
	start := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{}}
	plans := Simulate([]config.DLE{dleNamed("h")}, hist, nil, Params{CycleDays: 7}, start, 0)
	if len(plans) != 1 {
		t.Fatalf("days=0 should clamp to one plan, got %d", len(plans))
	}
}
