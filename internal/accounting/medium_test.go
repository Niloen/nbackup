package accounting

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
)

func day(d int) time.Time { return time.Date(2026, 7, d, 12, 0, 0, 0, time.UTC) }

// TestSummarizeReflectsPrune is the point of the recorded ledger: growth is measured
// from the true curve, so a prune-driven decline yields no misleading fill projection
// where the retained-archive picture could never show a decline at all.
func TestSummarizeReflectsPrune(t *testing.T) {
	grown := Summarize([]catalog.UsageSample{
		{At: day(1), Medium: "disk", Used: 200},
		{At: day(11), Medium: "disk", Used: 1_200},
	}, 10_000)
	if grown.Samples != 2 {
		t.Fatalf("Samples = %d, want 2", grown.Samples)
	}
	if grown.PerDay != 100 { // (1200-200) over 10 days
		t.Errorf("PerDay = %d, want 100", grown.PerDay)
	}
	if grown.ProjFull.IsZero() {
		t.Errorf("expected a fill projection for a growing bounded medium")
	}

	// A net decline over the window (a big prune) yields no growth rate — the
	// projection must not run backwards.
	pruned := Summarize([]catalog.UsageSample{
		{At: day(1), Used: 5_000},
		{At: day(11), Used: 1_000},
	}, 10_000)
	if pruned.PerDay != 0 || !pruned.ProjFull.IsZero() {
		t.Errorf("declining series: PerDay=%d ProjFull=%v; want 0 / zero", pruned.PerDay, pruned.ProjFull)
	}
}

func TestSummarizeShortSpanNoRate(t *testing.T) {
	// Two samples hours apart is too short a baseline to read a daily rate from.
	st := Summarize([]catalog.UsageSample{
		{At: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), Used: 100},
		{At: time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC), Used: 200},
	}, 1000)
	if st.PerDay != 0 || !st.ProjFull.IsZero() {
		t.Errorf("sub-day span: PerDay=%d ProjFull=%v; want 0 / zero", st.PerDay, st.ProjFull)
	}
	if st.Last != time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC) {
		t.Errorf("Last = %v; the span is still summarized", st.Last)
	}
}

func TestSummarizeUnboundedNoProjection(t *testing.T) {
	st := Summarize([]catalog.UsageSample{
		{At: day(1), Used: 100},
		{At: day(11), Used: 1_100},
	}, 0)
	if st.PerDay != 100 {
		t.Errorf("PerDay = %d, want 100 (growth is capacity-independent)", st.PerDay)
	}
	if !st.ProjFull.IsZero() {
		t.Errorf("unbounded medium projected full at %v; want none", st.ProjFull)
	}
}
