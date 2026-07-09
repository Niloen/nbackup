package report

import (
	"sort"
	"time"
)

// TrendPoint is one dump of a DLE as the run history recorded it — the unit of
// the size-evolution views (web charts, list sparklines, the `nb dle` summary
// line). It comes from the run-log's DumpStats rather than the catalog: the log
// outlives pruning (a full retired from every medium still counts as history),
// which is exactly the horizon "evolution" is about.
type TrendPoint struct {
	At      time.Time
	Orig    int64 // uncompressed bytes
	Out     int64 // compressed bytes on the volume
	Seconds float64
	Level   int
}

// DLETrend gathers slug's dump points from the run history (any order),
// oldest-first.
func DLETrend(runs []Run, slug string) []TrendPoint {
	return DLETrends(runs)[slug]
}

// DLETrends gathers every DLE's dump points from the run history (any order) in
// one pass, oldest-first per DLE — the /dles list needs all of them at once.
func DLETrends(runs []Run) map[string][]TrendPoint {
	trends := map[string][]TrendPoint{}
	for _, r := range runs {
		if r.Command != CommandDump {
			continue
		}
		at := r.EndedAt
		if at.IsZero() {
			at = r.StartedAt
		}
		for _, d := range r.DumpStats {
			trends[d.DLE] = append(trends[d.DLE], TrendPoint{
				At: at, Orig: d.Orig, Out: d.Out, Seconds: d.Seconds, Level: d.Level,
			})
		}
	}
	for _, pts := range trends {
		sort.Slice(pts, func(i, j int) bool { return pts[i].At.Before(pts[j].At) })
	}
	return trends
}

// EvolutionWindow bounds how far back a growth rate is measured. A DLE's whole
// history bakes long-gone eras into the slope (the same trap the medium ledger's
// growthWindow avoids); half a year is recent enough to say something about
// tomorrow while spanning several full cycles. Anchored at the series' newest
// point, not the wall clock, so a paused schedule still summarizes its last
// active stretch.
const EvolutionWindow = 180 * 24 * time.Hour

// Evolution summarizes a DLE's size history over the recent window: how its
// full-dump (dataset) size moved, and the typical incremental (churn) size.
// Fulls and incrementals are summarized separately on purpose — a growing
// dataset and growing churn are different findings, and mixing the two levels
// into one series is the sawtooth this feature replaces.
type Evolution struct {
	From, To TrendPoint // first and last full inside the window
	Days     float64    // span between them
	PerDay   int64      // average output-bytes growth per day; negative for a shrinking dataset
	Pct      int        // percent change From→To (output bytes); negative for shrinkage
	// IncrMedian is the median incremental output size in the window; 0 when the
	// window holds no incrementals (a fulls-only schedule).
	IncrMedian int64
}

// SummarizeTrend computes the evolution summary from a DLE's dump points
// (oldest-first, as DLETrend returns). ok is false when the window holds fewer
// than two fulls or they span under a day — too little baseline to read a rate
// from, so nothing is shown rather than a misleading number.
func SummarizeTrend(pts []TrendPoint) (Evolution, bool) {
	if len(pts) == 0 {
		return Evolution{}, false
	}
	cutoff := pts[len(pts)-1].At.Add(-EvolutionWindow)
	var fulls, incrs []TrendPoint
	for _, p := range pts {
		if p.At.Before(cutoff) {
			continue
		}
		if p.Level == 0 {
			fulls = append(fulls, p)
		} else {
			incrs = append(incrs, p)
		}
	}
	if len(fulls) < 2 {
		return Evolution{}, false
	}
	ev := Evolution{From: fulls[0], To: fulls[len(fulls)-1]}
	ev.Days = ev.To.At.Sub(ev.From.At).Hours() / 24
	if ev.Days < 1 {
		return Evolution{}, false
	}
	ev.PerDay = int64(float64(ev.To.Out-ev.From.Out) / ev.Days)
	if ev.From.Out > 0 {
		ev.Pct = int(float64(ev.To.Out-ev.From.Out) / float64(ev.From.Out) * 100)
	}
	if len(incrs) > 0 {
		outs := make([]int64, len(incrs))
		for i, p := range incrs {
			outs[i] = p.Out
		}
		ev.IncrMedian = medianOf(outs)
	}
	return ev, true
}

// medianOf returns the median of vs, which must be non-empty.
func medianOf(vs []int64) int64 {
	s := append([]int64(nil), vs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}
