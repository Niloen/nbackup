package engine

import (
	"time"

	"github.com/Niloen/nbackup/internal/accounting"
	"github.com/Niloen/nbackup/internal/archiveio"
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

// MediumForecast is one medium's projected fill curve.
type MediumForecast = accounting.MediumForecast

// ReadEstimate is the cost of reading a set of archives back off a medium.
type ReadEstimate = accounting.ReadEstimate

// CostSummary prices the current footprint and the next run on the landing medium.
// plan may be nil (footprint only); see accounting.
func (e *Engine) CostSummary(plan *planner.Plan) CostSummary {
	return e.acct.CostSummary(plan)
}

// ForecastCost projects the landing medium's storage cost and capacity headroom
// forward day by day over the caller's simulated plans (see accounting). The caller
// passes the SAME plans it rendered the schedule from — live or offline (`--days`
// vs `--days --offline`) — so the cost/capacity curve and the schedule always agree.
func (e *Engine) ForecastCost(start time.Time, plans []*planner.Plan) []ForecastPoint {
	return e.acct.ForecastCost(start, plans)
}

// ForecastCapacity projects every size-structured landing medium's fill forward over
// the caller's simulated plans — the per-medium generalization of ForecastCost. The
// caller passes the same plans it rendered the schedule from (live or offline).
func (e *Engine) ForecastCapacity(start time.Time, plans []*planner.Plan) []MediumForecast {
	return e.acct.ForecastCapacity(start, plans)
}

// ForecastCapacityOffline projects per-medium fill over `days` from `start` using the
// OFFLINE simulation — catalog + run-log only, no archiver/SSH probe — so the web can
// draw capacity curves without ever touching a host.
func (e *Engine) ForecastCapacityOffline(start time.Time, days int) ([]MediumForecast, error) {
	plans, err := e.sched.SimulateOffline(start, days)
	if err != nil {
		return nil, err
	}
	return e.acct.ForecastCapacity(start, plans), nil
}

// RestoreCost prices a whole-DLE restore (or every DLE) as of a date; see accounting.
func (e *Engine) RestoreCost(dles []string, asOf string) ReadEstimate {
	return e.acct.RestoreCost(dles, asOf)
}

// ReadPlanRow is one archive's line in a selective recovery's extraction plan: how it
// will be read (ranged vs whole and in how many fetches), the encoded bytes that pulls
// versus the whole-archive size, the copy it reads from and that copy's egress price, and
// — on a whole read — why ranging was not possible. It is the EXPLAIN of a file recovery.
type ReadPlanRow struct {
	Ref     archiveio.Ref
	DLE     string // host:path display identity
	Files   int
	Ranged  bool
	Fetches int64
	Read    int64 // encoded bytes pulled off the medium
	Whole   int64 // the whole archive's on-medium size
	Medium  string
	Priced  bool
	Cost    float64
	Reason  string // on a whole read, why ranging was not possible
}

// SelectionPlan plans a file-level recovery: the per-archive read strategy (the EXPLAIN
// rows) plus the aggregate egress estimate the confirmation prompt gates on. The restorer
// plans how each archive is actually read — a framed/atomic archive fetches only its
// selected members' covering frames, everything else the whole payload — and the
// accountant prices that real egress, so both the plan and the confirmation match what the
// extract pulls (not the whole archive by default). One SelectionReads pass feeds both.
func (e *Engine) SelectionPlan(steps []recovery.ExtractStep) ([]ReadPlanRow, ReadEstimate) {
	reads := e.rst.SelectionReads(steps)
	items := make([]accounting.ReadItem, 0, len(reads))
	rows := make([]ReadPlanRow, 0, len(reads))
	for _, rd := range reads {
		it := accounting.ReadItem{Ref: rd.Ref, Bytes: rd.Bytes, Parts: rd.Parts, Ranged: rd.Ranged}
		items = append(items, it)
		price := e.acct.PriceReadRow(it)
		rows = append(rows, ReadPlanRow{
			Ref: rd.Ref, DLE: e.DisplayDLE(rd.Ref.DLE), Files: rd.Files,
			Ranged: rd.Ranged, Fetches: rd.Parts, Read: rd.Bytes, Whole: rd.Whole,
			Medium: price.Medium, Priced: price.Priced, Cost: price.Cost, Reason: rd.Reason,
		})
	}
	return rows, e.acct.PriceRead(items)
}
