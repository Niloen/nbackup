package scheduler

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
)

// Plan builds the plan for a run date: it estimates every DLE, fulls the ones
// due by the cycle deadline, and promotes future fulls forward to level light
// runs (bounded by the per-run capacity room). sink (nil to disable) receives a
// live snapshot as each DLE's estimate starts and finishes, for the
// (potentially slow) estimate phase: every DLE is sized by an archiver pass, so
// a long preview is otherwise silent.
func (s *Scheduler) Plan(date time.Time, sink progress.Sink) *planner.Plan {
	dles := s.d.DLEs()
	plan := planner.Build(dles, s.d.History(), s.estimates(dles, sink), s.d.ForcedFulls(), s.plannerParams(date), date)
	s.forceFullWhereBaseMissing(plan)
	return plan
}

// forceFullWhereBaseMissing downgrades any planned incremental whose base incremental
// state is missing or unusable to a full, in place, with a warning. The planner picks the
// level from the catalog's run history, which can outlive the archiver's per-host state —
// a state_dir that moved, or a base a crashed dump never finished. Rather than fail the
// run (or, worse, dump a full-sized "incremental" onto a dead base), force a fresh full,
// the way Amanda falls back to level 0 when it can't find a usable base. A real run and a
// preview (`nb plan` / `--dry-run`) both go through here, so they agree.
func (s *Scheduler) forceFullWhereBaseMissing(plan *planner.Plan) {
	for i := range plan.Items {
		it := &plan.Items[i]
		if it.Level < 1 {
			continue
		}
		ar, err := s.d.ArchiverFor(it.DLE.DumpTypeName(), it.DLE.Host)
		if err != nil || ar.HasBase(it.Name, it.BaseLevel) {
			continue
		}
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"DLE %s: the L%d incremental state is missing or unusable (a prior dump may have been interrupted, or state_dir moved) — forcing a full (L0)",
			it.DLE.ID(), it.BaseLevel))
		it.Level, it.BaseLevel, it.BaseRun = 0, -1, ""
		it.EstBytes = it.FullBytes
		it.Reason = "forced full: incremental base missing or unusable"
	}
}

// plannerParams derives the planner's tuning inputs from config and the medium for
// a run date. Shared by Plan and Simulate so a single-day plan and the forward
// forecast use identical balancing rules.
func (s *Scheduler) plannerParams(date time.Time) planner.Params {
	return planner.Params{
		CycleDays:     s.d.CycleDays(),
		CapacityBytes: s.d.Capacity(),
		RoomBytes:     s.d.CapacityRoom(date),
		BumpPercent:   s.d.BumpPercent(),
	}
}

// Simulate forecasts the next `days` daily runs from `start` without writing
// anything: it plans each day and advances a cloned history between them, so the
// level schedule — when each DLE's full next lands, how its incrementals climb — is
// projected forward. Estimates and the capacity ceiling are sampled once at `start`
// and held constant, so this is a schedule forecast, not a capacity timeline.
func (s *Scheduler) Simulate(start time.Time, days int) []*planner.Plan {
	dles := s.d.DLEs()
	return planner.Simulate(dles, s.d.History(), s.estimates(dles, nil), s.d.ForcedFulls(), s.plannerParams(start), start, days)
}
