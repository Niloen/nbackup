package planner

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/dle"
)

func TestDecideLevels(t *testing.T) {
	today := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)

	// No prior full -> must be a full.
	if lvl, _ := decide("dle-a", &catalog.DLEState{}, today, 7); lvl != 0 {
		t.Errorf("first backup: got L%d, want L0", lvl)
	}

	// Recent full -> incremental.
	recent := &catalog.DLEState{LastFullDate: today.AddDate(0, 0, -1).Format("2006-01-02")}
	if lvl, _ := decide("dle-a", recent, today, 7); lvl != 1 {
		t.Errorf("recent full: got L%d, want L1", lvl)
	}

	// Very overdue full (>= 2x interval) -> forced full regardless of stagger.
	overdue := &catalog.DLEState{LastFullDate: today.AddDate(0, 0, -20).Format("2006-01-02")}
	if lvl, _ := decide("dle-a", overdue, today, 7); lvl != 0 {
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
	lvl, _ := decide("dle-a", d, today, 7)
	if lvl != 2 {
		t.Errorf("after L0,L1 the next level should be L2, got L%d", lvl)
	}
}

func TestBuildAssignsBaseSlot(t *testing.T) {
	hist := &catalog.History{DLEs: map[string]*catalog.DLEState{}}
	hist.DLEs["h-data"] = &catalog.DLEState{
		LastFullDate: "2026-06-20",
		LastFullSlot: "slot-2026-06-20",
		Runs: []catalog.RunRecord{
			{Date: "2026-06-20", Slot: "slot-2026-06-20", Level: 0},
		},
	}
	dles := []dle.DLE{{Host: "h", Path: "/data"}}
	p := Build(dles, hist, 7, time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC))
	if len(p.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(p.Items))
	}
	it := p.Items[0]
	if it.Level != 1 {
		t.Fatalf("expected L1 incremental, got L%d", it.Level)
	}
	if it.BaseSlot != "slot-2026-06-20" || it.BaseLevel != 0 {
		t.Errorf("base = (%q, L%d), want (slot-2026-06-20, L0)", it.BaseSlot, it.BaseLevel)
	}
}
