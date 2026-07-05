package scheduler

import (
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
)

// estimates predicts, for each DLE, the size of a full and of the incremental at
// its current level and the next (the inputs the planner's bump decision needs),
// by asking the archiver. For gnutar this is a fast metadata-only tar pass; see
// gnutar.Estimate. Sizes are uncompressed — an upper bound on the compressed bytes
// finally stored.
//
// Estimates run in parallel, bounded by parallelism.workers: each DLE's estimate is
// an independent archiver pass, and on a host with many DLEs the serial sum dominates
// a preview. When sink is non-nil the work is tracked so a caller can paint live
// progress. The fan-out is read-only over shared caches — see warmCaches.
func (s *Scheduler) estimates(dles []config.DLE, sink progress.Sink) map[string]planner.Estimate {
	states := s.warmCaches(dles)

	workers := s.d.Workers()
	var tr *progress.Tracker
	if sink != nil {
		rows := make([]progress.Plan, len(dles))
		for i, d := range dles {
			rows[i] = progress.Plan{Name: d.ID()}
		}
		tr = progress.NewTracker("estimate", progress.PhaseEstimating, workers, rows, time.Now, sink)
	}

	// Phase 2: size in parallel (read-only). Each worker writes its own results[i]
	// (a disjoint index, no lock); the map is built serially after the fan-out joins.
	results := make([]planner.Estimate, len(dles))
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, workers)
	)
	for i, d := range dles {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, d config.DLE, st *catalog.DLEState) {
			defer wg.Done()
			defer func() { <-sem }()
			if tr != nil {
				tr.StartDLE(d.ID()) // progress display keys by host:path, matching the dump phase
			}
			results[i] = s.estimateDLE(d, st)
			if tr != nil {
				tr.FinishDLE(d.ID(), 0, results[i].Full, 0, nil)
			}
		}(i, d, states[i])
	}
	wg.Wait()
	if tr != nil {
		tr.SetPhase(progress.PhaseDone)
	}

	out := make(map[string]planner.Estimate, len(dles))
	for i, d := range dles {
		out[d.Name()] = results[i] // d.Name() is the internal slug: planner estimate key
	}
	return out
}

// warmCaches resolves each DLE's archiver and memoizes its history state SERIALLY,
// before estimates fans out. Both archiverFor and History.DLE mutate caches shared
// across the run (the archiver-by-key map; History's per-DLE memo), so they must run
// on one goroutine — the parallel workers then only ever READ them. Errors from
// archiverFor are swallowed here (the cache warm is best-effort); estimateDLE
// resolves the archiver again and surfaces any failure per-DLE.
func (s *Scheduler) warmCaches(dles []config.DLE) []*catalog.DLEState {
	hist := s.d.History()
	states := make([]*catalog.DLEState, len(dles))
	for i, d := range dles {
		_, _ = s.d.ArchiverFor(d.DumpTypeName(), d.Host)
		states[i] = hist.DLE(d.Name())
	}
	return states
}

func (s *Scheduler) estimateDLE(d config.DLE, st *catalog.DLEState) planner.Estimate {
	name := d.Name() // the internal slug, the archiver's incremental-state key
	arch, err := s.d.ArchiverFor(d.DumpTypeName(), d.Host)
	if err != nil || arch.Check() != nil {
		return planner.Estimate{} // no estimator available (e.g. the archiver's tool missing)
	}
	excl := s.d.ExcludeFor(d.DumpTypeName())
	full, ferr := arch.Estimate(archiver.BackupRequest{DLE: name, Source: d.Path, Level: 0, BaseLevel: -1, Exclude: excl})
	// A non-nil error with a non-zero floor means the archiver walked a partially-readable
	// source (an unreadable member): the size is a floor, not exact. A zero floor is
	// a total failure (e.g. a missing path) that ValidatePlan already reports, so we
	// don't double-warn for it here.
	incomplete := ferr != nil && full > 0
	if st.LastFullDate == "" {
		return planner.Estimate{Full: full, Incomplete: incomplete} // never fulled: only a full is possible
	}

	// The DLE sits at level L (planner.SittingLevel). We estimate that level and
	// the next so the planner can judge whether climbing to L+1 saves enough to be
	// worth it (see planner.chooseIncrLevel). L+1 is only estimable once an L dump
	// exists to base it on; until then IncrNext stays 0.
	lvl := planner.SittingLevel(st)
	est := planner.Estimate{Full: full, Incomplete: incomplete}
	if arch.HasBase(name, lvl-1) {
		est.Incr, _ = arch.Estimate(archiver.BackupRequest{
			DLE: name, Source: d.Path, Level: lvl, BaseLevel: lvl - 1, Exclude: excl,
		})
	}
	if lvl < planner.MaxLevel && arch.HasBase(name, lvl) {
		est.IncrNext, _ = arch.Estimate(archiver.BackupRequest{
			DLE: name, Source: d.Path, Level: lvl + 1, BaseLevel: lvl, Exclude: excl,
		})
	}
	return est
}
