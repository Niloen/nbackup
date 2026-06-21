package planner

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/state"
)

func TestDecideLevels(t *testing.T) {
	today := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)

	// No prior full -> must be a full.
	if lvl, _ := decide("dle-a", &state.DLEState{}, today, 7); lvl != 0 {
		t.Errorf("first backup: got L%d, want L0", lvl)
	}

	// Recent full -> incremental.
	recent := &state.DLEState{LastFullDate: today.AddDate(0, 0, -1).Format("2006-01-02")}
	if lvl, _ := decide("dle-a", recent, today, 7); lvl != 1 {
		t.Errorf("recent full: got L%d, want L1", lvl)
	}

	// Very overdue full (>= 2x interval) -> forced full regardless of stagger.
	overdue := &state.DLEState{LastFullDate: today.AddDate(0, 0, -20).Format("2006-01-02")}
	if lvl, _ := decide("dle-a", overdue, today, 7); lvl != 0 {
		t.Errorf("overdue full: got L%d, want L0", lvl)
	}
}

func TestBuildAssignsBaseSlot(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sources = []config.Source{{Host: "h", Path: "/data"}}
	st := &state.State{DLEs: map[string]*state.DLEState{}}
	st.DLEs["h-data"] = &state.DLEState{
		LastFullDate: "2026-06-20",
		LastFullSlot: "slot-2026-06-20",
		Runs: []state.RunRecord{
			{Date: "2026-06-20", Slot: "slot-2026-06-20", Level: 0},
		},
	}
	p := Build(cfg, st, time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC))
	if len(p.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(p.Items))
	}
	it := p.Items[0]
	if it.Level == 1 && it.BaseSlot != "slot-2026-06-20" {
		t.Errorf("incremental base slot = %q, want slot-2026-06-20", it.BaseSlot)
	}
}
