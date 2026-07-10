package scheduler

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
)

// Plan builds the plan for a run date: it resolves the configured sources into concrete
// DLEs (expanding wildcards/partitions over each archiver — the one live enumeration,
// which FAILS the plan on error rather than guessing), estimates every DLE, fulls the
// ones due by the cycle deadline, and promotes future fulls forward to level light
// runs (bounded by the per-run capacity room). sink (nil to disable) receives a
// live snapshot as each DLE's estimate starts and finishes, for the
// (potentially slow) estimate phase: every DLE is sized by an archiver pass, so
// a long preview is otherwise silent.
//
// The error return is CONFIG-CLASS ONLY (the failure ladder's top rung): an identity
// collision or an unresolvable archiver definition — deterministic misconfiguration an
// operator must fix, so it blocks rather than warns. Every OPERATIONAL failure (a source
// that cannot be enumerated, a dead estimate, later an unreachable host) becomes a
// plan.Failed unit: reported like a dump failure while the rest of the night proceeds.
// The pure algorithm underneath (planner.Build) has no error return at all.
func (s *Scheduler) Plan(date time.Time, sink progress.Sink) (*planner.Plan, error) {
	return s.planFrom(s.liveDLEs(), s.probe(), date, sink)
}

// PlanOffline builds the plan with NO archiver I/O at all: the DLE set comes from the
// catalog's recorded resolved set (not a live enumeration — no SSH/find), sizes are
// projected from recorded history (see historySource), and the force-full post-pass
// (which probes the client's incremental state over SSH) is skipped. This is what makes
// the fast `nb plan --offline` preview and the webui ghost calendar safe to run without
// touching a host. Levels are identical to a live Plan (they are catalog-derived); only
// the byte estimates differ, and are flagged projected by the caller.
func (s *Scheduler) PlanOffline(date time.Time, sink progress.Sink) (*planner.Plan, error) {
	return s.planFrom(s.catalogDLEs(), s.history(date), date, sink)
}

// planFrom is the shared plan body over the two online seams: the DLESource enumerates
// (and, if it can, refines bases), the EstimateSource sizes, and the pure planner turns
// both into the plan. Plan and PlanOffline differ ONLY in the pair of seams they hand
// in — a live pair or a catalog pair — never in the body below.
func (s *Scheduler) planFrom(dsrc DLESource, esrc EstimateSource, date time.Time, sink progress.Sink) (*planner.Plan, error) {
	dles, srcFails, err := dsrc.Resolve()
	if err != nil {
		return nil, err // config-class: a collision or unresolvable definition fails the plan
	}
	est, estFailed := esrc.Estimates(dles, sink)
	// The failure ladder's unit class: failed units leave the plannable set and are
	// carried on plan.Failed — rendered by `nb plan`, marked FAILED in the run tracker,
	// counted into the run's non-zero exit — while every healthy unit proceeds.
	plannable := dles[:0]
	dead := map[string]bool{}
	for _, f := range estFailed {
		dead[f.DLE.Name()] = true
	}
	for _, d := range dles {
		if !dead[d.Name()] {
			plannable = append(plannable, d)
		}
	}
	plan := planner.Build(plannable, s.d.History(), est, s.d.ForcedFulls(), s.plannerParams(date), date)
	for _, f := range srcFails {
		plan.Failed = append(plan.Failed, planner.FailedUnit{ID: f.Source.ID(), Origin: f.Source.ID(), Reason: "source could not be resolved: " + f.Err.Error()})
	}
	plan.Failed = append(plan.Failed, estFailed...)
	dsrc.RefineBases(plan)
	return plan, nil
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
		if err != nil || ar.HasBase(it.Name, it.BaseLevel, it.DLE.Scope) {
			continue
		}
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"DLE %s: forcing a full (L0) — its L%d incremental base is not usable for this dump. Most often this is DELIBERATE: a subtree was newly carved out (a partition child graduated, or an anchored ./ exclude was added), which re-baselines once so the old chain's stale copy ages out. Otherwise the state is genuinely missing (an interrupted prior dump, or a moved state_dir)",
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
func (s *Scheduler) Simulate(start time.Time, days int) ([]*planner.Plan, error) {
	return s.simulateFrom(s.liveDLEs(), s.probe(), start, days)
}

// SimulateOffline forecasts like Simulate but projects sizes from history instead of
// probing — the natural default for a multi-day preview, where the probe buys nothing
// (Simulate samples estimates once and holds them constant) yet costs a full estimate
// sweep. It also feeds the webui ghost calendar its forward schedule.
func (s *Scheduler) SimulateOffline(start time.Time, days int) ([]*planner.Plan, error) {
	return s.simulateFrom(s.catalogDLEs(), s.history(start), start, days)
}

// simulateFrom is the shared forecast body. Like planFrom it takes the two seams, but a
// forecast never refines bases (it projects levels forward, it does not judge on-disk
// state), so it uses only DLESource.Resolve.
func (s *Scheduler) simulateFrom(dsrc DLESource, esrc EstimateSource, start time.Time, days int) ([]*planner.Plan, error) {
	dles, _, err := dsrc.Resolve()
	if err != nil {
		return nil, err
	}
	// A forecast is advisory: failed sources/estimates simply don't appear in it
	// (the real plan reports them as FAILED units).
	est, estFailed := esrc.Estimates(dles, nil)
	dead := map[string]bool{}
	for _, f := range estFailed {
		dead[f.DLE.Name()] = true
	}
	plannable := dles[:0]
	for _, d := range dles {
		if !dead[d.Name()] {
			plannable = append(plannable, d)
		}
	}
	return planner.Simulate(plannable, s.d.History(), est, s.d.ForcedFulls(), s.plannerParams(start), start, days), nil
}
