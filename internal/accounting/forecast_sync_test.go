package accounting

import (
	"testing"

	"github.com/Niloen/nbackup/internal/config"
)

// TestCopyMediaForClosure checks the sync-copy routing: a dumptype's archives reach its
// landing plus every sync target reachable through the rules (transitively), and a
// last:N target reports its run cap while an unbounded feed leaves a target uncapped.
func TestCopyMediaForClosure(t *testing.T) {
	cfg := &config.Config{
		Landing: config.MediumList{"disk"},
		Media:   map[string]config.Media{"disk": {}, "tape": {}, "deep": {}},
		Sync: []config.SyncRule{
			{To: "tape"},                        // auto: mirror the landing
			{To: "deep", From: "tape", Last: 5}, // chain: tape -> deep, last 5
		},
	}
	a := New(Deps{Cfg: cfg})

	got := map[string]bool{}
	for _, m := range a.copyMediaFor("") {
		got[m] = true
	}
	for _, want := range []string{"disk", "tape", "deep"} {
		if !got[want] {
			t.Errorf("copyMediaFor should reach %q (landing/sync closure); got %v", want, got)
		}
	}

	if l, c := a.mediumRunCap("deep"); !c || l != 5 {
		t.Errorf("deep is fed by a last:5 rule: cap=%d capped=%v, want 5/true", l, c)
	}
	if _, c := a.mediumRunCap("tape"); c {
		t.Errorf("tape is fed by an unbounded (last:0) rule — should be uncapped")
	}
	if _, c := a.mediumRunCap("disk"); c {
		t.Errorf("disk is a landing route — should be uncapped")
	}
}
