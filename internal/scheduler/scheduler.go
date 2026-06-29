// Package scheduler is NBackup's planning lane: it turns the configured DLEs, the
// catalog's run history, and the per-run capacity room into a planner.Plan — the
// level schedule a run executes and a preview (`nb plan`, `nb dump --dry-run`)
// shows. It is the estimate/plan/validate arithmetic the engine used to do inline,
// split out behind a narrow dependency slice. The methods are stubs in this commit
// (the engine still does the real work); a later lane fills them in.
package scheduler

import (
	"fmt"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
)

// Scheduler holds the slice of the orchestrator the planner needs: the inputs are
// closures the engine binds to its own config/catalog/archiver resolution, so the
// scheduler never reaches into the engine directly.
type Scheduler struct {
	dles              func() []config.DLE
	history           func() *catalog.History
	forcedFulls       func() map[string]bool
	workers           func() int
	archiverFor       func(dtName, host string) (archiver.Archiver, error)
	excludeFor        func(dtName string) []string
	cycleDays         func() int
	bumpPercent       func() float64
	capacity          func() int64
	capacityRoom      func(now time.Time) int64
	compressCheck     func() error
	preflightDumptype func(dt, host string, checkArchiver bool, checked map[string]bool) error
	remoteHost        func(host string) (config.SSHConfig, bool)
	statSource        func(path string) error
	probeReachable    func(host string) error
}

// Deps is the exported mirror of the Scheduler's dependency slice.
type Deps struct {
	DLEs              func() []config.DLE
	History           func() *catalog.History
	ForcedFulls       func() map[string]bool
	Workers           func() int
	ArchiverFor       func(dtName, host string) (archiver.Archiver, error)
	ExcludeFor        func(dtName string) []string
	CycleDays         func() int
	BumpPercent       func() float64
	Capacity          func() int64
	CapacityRoom      func(now time.Time) int64
	CompressCheck     func() error
	PreflightDumptype func(dt, host string, checkArchiver bool, checked map[string]bool) error
	RemoteHost        func(host string) (config.SSHConfig, bool)
	StatSource        func(path string) error
	ProbeReachable    func(host string) error
}

// New constructs a Scheduler from its dependencies.
func New(d Deps) *Scheduler {
	return &Scheduler{
		dles:              d.DLEs,
		history:           d.History,
		forcedFulls:       d.ForcedFulls,
		workers:           d.Workers,
		archiverFor:       d.ArchiverFor,
		excludeFor:        d.ExcludeFor,
		cycleDays:         d.CycleDays,
		bumpPercent:       d.BumpPercent,
		capacity:          d.Capacity,
		capacityRoom:      d.CapacityRoom,
		compressCheck:     d.CompressCheck,
		preflightDumptype: d.PreflightDumptype,
		remoteHost:        d.RemoteHost,
		statSource:        d.StatSource,
		probeReachable:    d.ProbeReachable,
	}
}

// Plan builds the plan for a run date: it estimates every DLE, fulls the ones
// due by the cycle deadline, and promotes future fulls forward to level light
// runs (bounded by the per-run capacity room). sink (nil to disable) receives a
// live snapshot as each DLE's estimate starts and finishes, for the
// (potentially slow) estimate phase: every DLE is sized by an archiver pass, so
// a long preview is otherwise silent.
func (s *Scheduler) Plan(date time.Time, sink progress.Sink) *planner.Plan {
	dles := s.dles()
	plan := planner.Build(dles, s.history(), s.estimates(dles, sink), s.forcedFulls(), s.plannerParams(date), date)
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
		ar, err := s.archiverFor(it.DLE.DumpTypeName(), it.DLE.Host)
		if err != nil || ar.HasBase(it.Name, it.BaseLevel) {
			continue
		}
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"DLE %s: the L%d incremental state is missing or unusable (a prior dump may have been interrupted, or state_dir moved) — forcing a full (L0)",
			it.DLE.ID(), it.BaseLevel))
		it.Level, it.BaseLevel, it.BaseSlot = 0, -1, ""
		it.EstBytes = it.FullBytes
		it.Reason = "forced full: incremental base missing or unusable"
	}
}

// Validate checks each DLE the way a real run would resolve it, so a preview
// (`nb plan` / `nb dump --dry-run`) surfaces problems the size estimates would
// otherwise swallow into a misleading ~0 B. It runs the same pre-flight a real run
// does — the compression scheme and every dumptype's method and encryption scheme —
// returning a fatal error for an unrunnable config (an unknown compression/method/encryption scheme,
// a missing required key reference, or a scheme/gpg binary not on PATH), so a preview
// no longer gives a green light to a run that `nb dump` will reject. Source paths
// that are missing or unreadable right now are non-fatal warnings (they may be an
// unmounted volume the real run will mount).
func (s *Scheduler) Validate() (warnings []string, err error) {
	if err := s.compressCheck(); err != nil {
		return nil, err
	}
	checkedEnc := map[string]bool{}
	hostProbed := map[string]bool{}
	for _, d := range s.dles() {
		if err := s.preflightDumptype(d.DumpTypeName(), d.Host, false, checkedEnc); err != nil {
			return nil, err
		}
		// Only a local source can be stat'd here; a remote DLE's path lives on the
		// client. A remote host is probed over SSH (once per host) so an unreachable
		// client warns here rather than silently estimating ~0 B — the misleading
		// "healthy" plan `nb check` would otherwise be the only thing to catch.
		if _, remote := s.remoteHost(d.Host); !remote {
			if err := s.statSource(d.Path); err != nil {
				warnings = append(warnings, fmt.Sprintf("DLE %s: source path %s is missing or unreadable (%v) — the real run will fail unless it becomes available", d.ID(), d.Path, err))
			}
		} else if !hostProbed[d.Host] {
			hostProbed[d.Host] = true
			if err := s.probeReachable(d.Host); err != nil {
				warnings = append(warnings, fmt.Sprintf("%v — its DLEs cannot be estimated until it is reachable (shown as ~0 B)", err))
			}
		}
	}
	return warnings, nil
}

// Simulate forecasts the next `days` daily runs from `start` without writing
// anything: it plans each day and advances a cloned history between them, so the
// level schedule — when each DLE's full next lands, how its incrementals climb — is
// projected forward. Estimates and the capacity ceiling are sampled once at `start`
// and held constant, so this is a schedule forecast, not a capacity timeline.
func (s *Scheduler) Simulate(start time.Time, days int) []*planner.Plan {
	dles := s.dles()
	return planner.Simulate(dles, s.history(), s.estimates(dles, nil), s.forcedFulls(), s.plannerParams(start), start, days)
}

// plannerParams derives the planner's tuning inputs from config and the medium for
// a run date. Shared by Plan and Simulate so a single-day plan and the forward
// forecast use identical balancing rules.
func (s *Scheduler) plannerParams(date time.Time) planner.Params {
	return planner.Params{
		CycleDays:     s.cycleDays(),
		CapacityBytes: s.capacity(),
		RoomBytes:     s.capacityRoom(date),
		BumpPercent:   s.bumpPercent(),
	}
}

// estimates predicts, for each DLE, the size of a full and of the incremental at
// its current level and the next (the inputs the planner's bump decision needs),
// by asking the archiver. For gnutar this is a
// fast metadata-only tar pass; see gnutar.Estimate. Sizes are uncompressed — an
// upper bound on the compressed bytes finally stored.
// Estimates run in parallel, bounded by parallelism.workers:
// each DLE's estimate is an independent archiver pass, and on a host with many DLEs
// the serial sum dominates a preview. When sink is non-nil the work is tracked so a
// caller can paint live progress. Archivers are resolved serially first because
// archiverFor writes a shared cache the workers must only read.
func (s *Scheduler) estimates(dles []config.DLE, sink progress.Sink) map[string]planner.Estimate {
	hist := s.history()
	out := make(map[string]planner.Estimate, len(dles))
	states := make([]*catalog.DLEState, len(dles))
	for i, d := range dles {
		_, _ = s.archiverFor(d.DumpTypeName(), d.Host) // warm the cache; errors resurface per-DLE below
		states[i] = hist.DLE(d.Name())                 // History.DLE memoizes; resolve serially before the workers read it
	}

	workers := s.workers()
	var tr *progress.Tracker
	if sink != nil {
		rows := make([]progress.Plan, len(dles))
		for i, d := range dles {
			rows[i] = progress.Plan{Name: d.ID()}
		}
		tr = progress.NewTracker("estimate", progress.PhaseEstimating, workers, rows, time.Now, sink)
	}

	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, workers)
		mu  sync.Mutex
	)
	for i, d := range dles {
		wg.Add(1)
		sem <- struct{}{}
		go func(d config.DLE, st *catalog.DLEState) {
			defer wg.Done()
			defer func() { <-sem }()
			name := d.Name() // internal slug: archiver request + planner estimate key
			if tr != nil {
				tr.StartDLE(d.ID()) // progress display keys by host:path, matching the dump phase
			}
			est := s.estimateDLE(d, name, st)
			mu.Lock()
			out[name] = est
			mu.Unlock()
			if tr != nil {
				tr.FinishDLE(d.ID(), 0, est.Full, 0, nil)
			}
		}(d, states[i])
	}
	wg.Wait()
	if tr != nil {
		tr.SetPhase(progress.PhaseDone)
	}
	return out
}

func (s *Scheduler) estimateDLE(d config.DLE, name string, st *catalog.DLEState) planner.Estimate {
	arch, err := s.archiverFor(d.DumpTypeName(), d.Host)
	if err != nil || arch.Check() != nil {
		return planner.Estimate{} // no estimator available (e.g. tar missing)
	}
	excl := s.excludeFor(d.DumpTypeName())
	full, ferr := arch.Estimate(archiver.BackupRequest{DLE: name, SourcePath: d.Path, Level: 0, BaseLevel: -1, Exclude: excl})
	// A non-nil error with a non-zero floor means tar walked a partially-readable
	// source (an unreadable member): the size is a floor, not exact. A zero floor is
	// a total failure (e.g. a missing path) that ValidatePlan already reports, so we
	// don't double-warn for it here.
	incomplete := ferr != nil && full > 0
	if st.LastFullDate == "" {
		return planner.Estimate{Full: full, Incomplete: incomplete} // never fulled: only a full is possible
	}

	// The DLE sits at level L — 1 right after a full, otherwise its last level. We
	// estimate that level and the next so the planner can judge whether climbing to
	// L+1 saves enough to be worth it (see planner.chooseIncrLevel). L+1 is only
	// estimable once an L dump exists to base it on; until then IncrNext stays 0.
	lvl := st.LastLevel()
	if lvl < 1 {
		lvl = 1
	}
	if lvl > planner.MaxLevel {
		lvl = planner.MaxLevel
	}
	est := planner.Estimate{Full: full, Incomplete: incomplete}
	if arch.HasBase(name, lvl-1) {
		est.Incr, _ = arch.Estimate(archiver.BackupRequest{
			DLE: name, SourcePath: d.Path, Level: lvl, BaseLevel: lvl - 1, Exclude: excl,
		})
	}
	if lvl < planner.MaxLevel && arch.HasBase(name, lvl) {
		est.IncrNext, _ = arch.Estimate(archiver.BackupRequest{
			DLE: name, SourcePath: d.Path, Level: lvl + 1, BaseLevel: lvl, Exclude: excl,
		})
	}
	return est
}
