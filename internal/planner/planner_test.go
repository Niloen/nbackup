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

func TestLevelDecisions(t *testing.T) {
	today := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	mk := func(d *catalog.DLEState) *Plan {
		hist := &catalog.History{DLEs: map[string]*catalog.DLEState{"h-data": d}}
		return Build([]config.DLE{dleNamed("h")}, hist, nil, Params{FullIntervalDays: 7}, today)
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
	// Overdue past the deadline -> forced full.
	overdue := &catalog.DLEState{LastFullDate: today.AddDate(0, 0, -20).Format("2006-01-02")}
	if lvl := levelOf(mk(overdue), "h-data"); lvl != 0 {
		t.Errorf("overdue full: got L%d, want L0", lvl)
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
	p := Build([]config.DLE{dleNamed("h")}, hist, nil, Params{FullIntervalDays: 7}, today)
	if lvl := levelOf(p, "h-data"); lvl != 2 {
		t.Errorf("after L0,L1 the next level should be L2, got L%d", lvl)
	}
}

// TestDegradeBalancesFulls checks that when several DLEs are due on the same day,
// degrade demotes the excess to incrementals to meet the balance target.
func TestDegradeBalancesFulls(t *testing.T) {
	today := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	// 4 DLEs, each last full 2 days ago (due at interval=2, below deadline 4).
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{}}
	var dles []config.DLE
	est := map[string]Estimate{}
	for _, h := range []string{"a", "b", "c", "d"} {
		d := dleNamed(h)
		name := d.Name()
		hist.DLEs[name] = &catalog.DLEState{
			LastFullDate: today.AddDate(0, 0, -2).Format("2006-01-02"),
			LastFullSlot: "slot-x",
			Runs:         []catalog.RunRecord{{Date: "old", Slot: "slot-x", Level: 0}},
		}
		dles = append(dles, d)
		est[name] = Estimate{Full: 100, Incr: 10}
	}
	// total full 400, interval 2 -> target 200 -> keep 2 fulls, degrade 2.
	p := Build(dles, hist, est, Params{FullIntervalDays: 2, CapacityRoomBytes: -1}, today)
	fulls := 0
	for _, it := range p.Items {
		if it.Level == 0 {
			fulls++
		}
	}
	if fulls != 2 {
		t.Errorf("expected 2 fulls after degrade to target 200, got %d", fulls)
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
	p := Build(dles, hist, est, Params{FullIntervalDays: 7, CapacityBytes: 250}, today)
	if len(p.Warnings) == 0 {
		t.Errorf("expected a structural warning when a recovery set (300) exceeds capacity (250)")
	}
	if len(p.Items) != 3 {
		t.Errorf("backups must still be scheduled despite the warning, got %d items", len(p.Items))
	}

	// Capacity 400 >= total full 300 -> no warning.
	p = Build(dles, hist, est, Params{FullIntervalDays: 7, CapacityBytes: 400}, today)
	if len(p.Warnings) != 0 {
		t.Errorf("did not expect a warning when the recovery set fits, got %v", p.Warnings)
	}

	// Unbounded capacity (0) -> no warning.
	p = Build(dles, hist, est, Params{FullIntervalDays: 7, CapacityBytes: 0}, today)
	if len(p.Warnings) != 0 {
		t.Errorf("unbounded capacity should not warn, got %v", p.Warnings)
	}
}

// TestCapacityRoomForcesDegrade checks the hard ceiling overrides the balance
// target: a tiny capacity room degrades more aggressively.
func TestCapacityRoomForcesDegrade(t *testing.T) {
	today := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{}}
	var dles []config.DLE
	est := map[string]Estimate{}
	for _, h := range []string{"a", "b", "c", "d"} {
		d := dleNamed(h)
		name := d.Name()
		hist.DLEs[name] = &catalog.DLEState{
			LastFullDate: today.AddDate(0, 0, -2).Format("2006-01-02"),
			Runs:         []catalog.RunRecord{{Date: "old", Slot: "slot-x", Level: 0}},
		}
		dles = append(dles, d)
		est[name] = Estimate{Full: 100, Incr: 10}
	}
	// room 130: keep at most 1 full (100) + 3 incr (30) = 130.
	p := Build(dles, hist, est, Params{FullIntervalDays: 2, CapacityRoomBytes: 130}, today)
	fulls := 0
	for _, it := range p.Items {
		if it.Level == 0 {
			fulls++
		}
	}
	if fulls != 1 {
		t.Errorf("expected 1 full under capacity room 130, got %d", fulls)
	}
}
