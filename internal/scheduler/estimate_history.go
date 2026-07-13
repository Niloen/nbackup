package scheduler

import (
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/report"
)

// historySource projects each DLE's size from recorded history instead of probing
// the archiver, so a plan needs nothing but the catalog and the run-log — no SSH, no
// tar pass, instant. It is the estimate source behind `nb plan --offline`, the
// `--days` forecast (where the probe buys nothing, since Simulate holds estimates
// constant after day 0), and the webui ghost calendar.
//
// The projection sizes are pinned to `at` (the plan date): a full is the last
// recorded full grown forward by the dataset's evolution slope, so a preview a week
// out reflects a week of expected growth. For the multi-day forecast `at` is the
// window start, matching Simulate's "sample once, hold constant" semantics.
type historySource struct {
	hist   *catalog.History
	runLog []report.Run
	at     time.Time
}

// history returns the offline estimate source pinned to the plan date.
func (s *Scheduler) history(at time.Time) EstimateSource {
	return historySource{hist: s.d.History(), runLog: s.d.RunLog(), at: at}
}

// At repins the projection to day t (same catalog/run-log, new horizon), so a forecast
// grows each simulated day's sizes by t's distance from the last recorded full.
func (h historySource) At(t time.Time) EstimateSource { h.at = t; return h }

// Estimates projects every DLE's sizes from history. Unlike the probe it never fails
// a unit: absent history is not a dead source, only an unknown size (Full 0), which a
// never-fulled DLE has anyway — the planner fulls it regardless (see planner.Build's
// days<0 rule), so the SCHEDULE stays correct and only the byte column is blank.
func (h historySource) Estimates(dles []planner.DLE, _ progress.Sink) (map[string]planner.Estimate, []planner.FailedUnit) {
	trends := report.DLETrends(h.runLog)
	out := make(map[string]planner.Estimate, len(dles))
	for _, d := range dles {
		out[d.Name()] = h.project(h.hist.DLE(d.Name()), trends[d.Name()])
	}
	return out, nil
}

// project turns one DLE's history into an Estimate. Full is the last full grown to
// h.at by the fulls' slope; Incr/IncrNext are the median incremental at the sitting
// level and the next — bucketed by level, mirroring the probe's L-vs-L+1 split rather
// than lumping every incremental into one number. Sizes are uncompressed (Orig), the
// same upper-bound semantics the archiver probe returns.
func (h historySource) project(st *catalog.DLEState, pts []report.TrendPoint) planner.Estimate {
	last := lastFull(pts)
	if st.LastFullDate == "" || last == nil {
		return planner.Estimate{} // never fulled: size unknown, planner fulls it anyway
	}
	full := last.Orig
	if ev, ok := report.SummarizeTrend(pts); ok {
		if days := h.at.Sub(last.At).Hours() / 24; days > 0 {
			if full += int64(float64(ev.PerDayOrig) * days); full < 0 {
				full = 0 // a shrinking dataset projected past zero clamps, not inverts
			}
		}
	}
	est := planner.Estimate{Full: full}
	lvl := planner.SittingLevel(st)
	est.Incr = medianIncrSinceFull(pts, lvl, last.At)
	if lvl < planner.MaxLevel {
		est.IncrNext = medianIncrSinceFull(pts, lvl+1, last.At)
	}
	return est
}

// lastFull returns the most recent level-0 point, or nil if the series holds none.
func lastFull(pts []report.TrendPoint) *report.TrendPoint {
	for i := len(pts) - 1; i >= 0; i-- {
		if pts[i].Level == 0 {
			return &pts[i]
		}
	}
	return nil
}

// medianIncrSinceFull estimates a DLE's RECURRING incremental size at level lvl from the
// incrementals recorded SINCE its last full (on or after lastFullAt) — the churn against
// the CURRENT dataset baseline. An incremental only ever means "changes since its base",
// so once a new full re-bases the dataset everything before it describes a superseded
// era: a smaller dataset, or a one-time re-baseline / bulk catch-up whose bytes the new
// full has already absorbed (and whose growth the full's slope carries). Folding those
// in would size a normal night by an old or exceptional era — which is what let a single
// pre-full 15 GB re-dump project onto every incremental day. The last full is the
// natural boundary; no size threshold is needed.
//
// Returns 0 when no incremental has been taken since the full — the planner reads 0 as
// "not yet estimable" and holds the level (chooseIncrLevel). That is the safe default,
// and the honest one right after a full: the churn against the new base is not yet known.
func medianIncrSinceFull(pts []report.TrendPoint, lvl int, lastFullAt time.Time) int64 {
	var sizes []int64
	for _, p := range pts {
		if p.Level == lvl && !p.At.Before(lastFullAt) {
			sizes = append(sizes, p.Orig)
		}
	}
	if len(sizes) == 0 {
		return 0
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] < sizes[j] })
	return sizes[len(sizes)/2]
}
