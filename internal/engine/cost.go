package engine

import (
	"time"

	"github.com/Niloen/nbackup/internal/accounting"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/recovery"
)

// The dollar arithmetic lives in internal/accounting (the cost overlay on the byte
// ledger); these are the command-surface facades, plus type aliases so callers —
// including internal/cli — keep naming the engine's types.

// CostSummary is the landing medium's cost picture for a single planned run.
type CostSummary = accounting.CostSummary

// ForecastPoint is one day of the projected cost curve.
type ForecastPoint = accounting.ForecastPoint

// ReadEstimate is the cost of reading a set of archives back off a medium.
type ReadEstimate = accounting.ReadEstimate

// CostSummary prices the current footprint and the next run on the landing medium.
// plan may be nil (footprint only); see accounting.
func (e *Engine) CostSummary(plan *planner.Plan) CostSummary {
	return e.acct.CostSummary(plan)
}

// ForecastCost projects the landing medium's monthly storage cost forward day by
// day, feeding the accountant the planner's run simulation; see accounting.
func (e *Engine) ForecastCost(start time.Time, days int) []ForecastPoint {
	return e.acct.ForecastCost(start, e.sched.Simulate(start, days))
}

// RestoreCost prices a whole-DLE restore (or every DLE) as of a date; see accounting.
func (e *Engine) RestoreCost(dles []string, asOf string) ReadEstimate {
	return e.acct.RestoreCost(dles, asOf)
}

// SelectionCost prices a file-level recovery. The restorer plans how each archive will
// actually be read — a framed/atomic archive fetches only its selected members' covering
// frames, everything else the whole payload — and the accountant prices that real egress,
// so the confirmation matches what the extract pulls (not the whole archive by default).
func (e *Engine) SelectionCost(steps []recovery.ExtractStep) ReadEstimate {
	reads := e.rst.SelectionReads(steps)
	items := make([]accounting.ReadItem, 0, len(reads))
	for _, rd := range reads {
		items = append(items, accounting.ReadItem{Ref: rd.Ref, Bytes: rd.Bytes, Parts: rd.Parts, Ranged: rd.Ranged})
	}
	return e.acct.PriceRead(items)
}
