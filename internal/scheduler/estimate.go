package scheduler

import (
	"fmt"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
)

// EstimateSource supplies the per-DLE size predictions planner.Build needs (a full,
// and the incremental at the sitting level and the next). Two implementations feed
// the SAME planner: probeSource asks the archiver live (a metadata tar pass, which
// may SSH to the host — the accurate-but-slow default), and historySource projects
// the sizes from recorded history with no I/O (the offline path behind `nb plan
// --offline` and the webui ghost calendar). Because only the byte inputs differ, an
// offline plan and a live plan pick identical LEVELS — they can disagree only on size.
type EstimateSource interface {
	Estimates(dles []planner.DLE, sink progress.Sink) (map[string]planner.Estimate, []planner.FailedUnit)
}

// probeSource is the live archiver probe — the historical default. It owns the whole
// parallel estimate algorithm (Estimates/warmCaches/estimateDLE) and holds only the
// three Deps closures that algorithm needs, so it stands alone: reading this type shows
// the probe in full, without a hop back through the Scheduler.
type probeSource struct {
	workers     func() int
	history     func() *catalog.History
	archiverFor func(dtName, host string) (archiver.Archiver, error)
}

// probe returns the live archiver estimate source, wired to the scheduler's deps.
func (s *Scheduler) probe() EstimateSource {
	return probeSource{workers: s.d.Workers, history: s.d.History, archiverFor: s.d.ArchiverFor}
}

// Estimates predicts, for each DLE, the size of a full and of the incremental at
// its current level and the next (the inputs the planner's bump decision needs),
// by asking the archiver. For gnutar this is a fast metadata-only tar pass; see
// gnutar.Estimate. Sizes are uncompressed — an upper bound on the compressed bytes
// finally stored.
//
// Estimates run in parallel, bounded by parallelism.workers: each DLE's estimate is
// an independent archiver pass, and on a host with many DLEs the serial sum dominates
// a preview. When sink is non-nil the work is tracked so a caller can paint live
// progress. The fan-out is read-only over shared caches — see warmCaches.
// Estimates returns per-DLE size estimates for the plannable units, and the units whose
// estimate FAILED outright — Amanda's "planner: FAILED" class: a dead estimate almost
// always predicts a dead dump, and planning it anyway at a fictional ~0 B corrupts the
// capacity math (make-room would reserve nothing for a dump that then writes plenty).
// Failed units are dropped from planning and reported like dump failures; a MEASURED
// floor (a partially readable source) is not a failure — it degrades to the Incomplete
// warning as before.
func (p probeSource) Estimates(dles []planner.DLE, sink progress.Sink) (map[string]planner.Estimate, []planner.FailedUnit) {
	states := p.warmCaches(dles)

	workers := p.workers()
	var tr *progress.Tracker
	if sink != nil {
		rows := make([]progress.Plan, len(dles))
		for i, d := range dles {
			rows[i] = progress.Plan{Name: d.ID(), Slug: d.Name(), Rest: d.IsRest()}
		}
		tr = progress.NewTracker(progress.EstimateRunID, progress.PhaseEstimating, workers, rows, time.Now, sink)
	}

	// Phase 2: size in parallel (read-only). Each worker writes its own results[i]
	// (a disjoint index, no lock); the map is built serially after the fan-out joins.
	results := make([]planner.Estimate, len(dles))
	fails := make([]error, len(dles))
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, workers)
	)
	for i, d := range dles {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, d planner.DLE, st *catalog.DLEState) {
			defer wg.Done()
			defer func() { <-sem }()
			if tr != nil {
				tr.StartDLE(d.ID()) // progress display keys by host:path, matching the dump phase
			}
			results[i], fails[i] = p.estimateDLE(d, st)
			if tr != nil {
				tr.FinishDLE(d.ID(), 0, results[i].Full, 0, fails[i])
			}
		}(i, d, states[i])
	}
	wg.Wait()
	if tr != nil {
		tr.SetPhase(progress.PhaseDone)
	}

	out := make(map[string]planner.Estimate, len(dles))
	var failed []planner.FailedUnit
	for i, d := range dles {
		if fails[i] != nil {
			failed = append(failed, planner.FailedUnit{DLE: d, ID: d.ID(), Origin: d.Origin, Reason: fails[i].Error()})
			continue
		}
		out[d.Name()] = results[i] // d.Name() is the internal slug: planner estimate key
	}
	return out, failed
}

// warmCaches resolves each DLE's archiver and memoizes its history state SERIALLY,
// before estimates fans out. Both archiverFor and History.DLE mutate caches shared
// across the run (the archiver-by-key map; History's per-DLE memo), so they must run
// on one goroutine — the parallel workers then only ever READ them. Errors from
// archiverFor are swallowed here (the cache warm is best-effort); estimateDLE
// resolves the archiver again and surfaces any failure per-DLE.
func (p probeSource) warmCaches(dles []planner.DLE) []*catalog.DLEState {
	hist := p.history()
	states := make([]*catalog.DLEState, len(dles))
	for i, d := range dles {
		_, _ = p.archiverFor(d.DumpTypeName(), d.Host)
		states[i] = hist.DLE(d.Name())
	}
	return states
}

// estimateDLE sizes one unit. A non-nil error is the unit-class failure (dead archiver,
// estimate that measured nothing): the unit is planner-FAILED, not planned at ~0 B.
func (p probeSource) estimateDLE(d planner.DLE, st *catalog.DLEState) (planner.Estimate, error) {
	name := d.Name() // the internal slug, the archiver's incremental-state key
	arch, err := p.archiverFor(d.DumpTypeName(), d.Host)
	if err != nil {
		return planner.Estimate{}, err
	}
	if cerr := arch.Check(); cerr != nil {
		return planner.Estimate{}, fmt.Errorf("archiver unavailable: %w", cerr)
	}
	// R2: the resolved Scope is complete (configured excludes + any partition carves are
	// already baked in by Expand) — consume it VERBATIM. Rebuilding it here would
	// silently drop the rest's carves and double-count its children.
	full, ferr := arch.Estimate(archiver.BackupRequest{DLE: name, Scope: d.Scope, Level: 0, BaseLevel: -1})
	// A non-nil error with a non-zero floor means the archiver walked a partially-readable
	// source (an unreadable member): the size is a MEASURED floor — degrade to the
	// Incomplete warning. A zero floor measured nothing: the estimate is dead and the
	// dump would almost certainly die the same way — the unit fails here, loudly,
	// instead of being planned at a fictional ~0 B.
	if ferr != nil && full == 0 {
		return planner.Estimate{}, fmt.Errorf("estimate failed: %w", ferr)
	}
	incomplete := ferr != nil && full > 0
	if st.LastFullDate == "" {
		return planner.Estimate{Full: full, Incomplete: incomplete}, nil // never fulled: only a full is possible
	}

	// The DLE sits at level L (planner.SittingLevel). We estimate that level and
	// the next so the planner can judge whether climbing to L+1 saves enough to be
	// worth it (see planner.chooseIncrLevel). L+1 is only estimable once an L dump
	// exists to base it on; until then IncrNext stays 0.
	lvl := planner.SittingLevel(st)
	est := planner.Estimate{Full: full, Incomplete: incomplete}
	if arch.HasBase(name, lvl-1, d.Scope) {
		est.Incr, _ = arch.Estimate(archiver.BackupRequest{
			DLE: name, Scope: d.Scope, Level: lvl, BaseLevel: lvl - 1,
		})
	}
	if lvl < planner.MaxLevel && arch.HasBase(name, lvl, d.Scope) {
		est.IncrNext, _ = arch.Estimate(archiver.BackupRequest{
			DLE: name, Scope: d.Scope, Level: lvl + 1, BaseLevel: lvl,
		})
	}
	return est, nil
}
