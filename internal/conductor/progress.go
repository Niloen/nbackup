package conductor

import (
	"time"

	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
)

// progressTracker builds the run's dump-phase tracker and the log function to use under it. It
// takes over fileSink — the run-status file the estimate phase opened — so `nb status` sees one
// continuous dump cycle, now under the real run ID. A live terminal sink (when attached) paints
// the same snapshots and suppresses the per-DLE log lines (runLogf becomes nil) so they don't
// scribble over the in-place region. Progress reporting never blocks or fails the backup.
func (c *Conductor) progressTracker(runID string, workers int, items []planner.Item, fileSink progress.Sink, lf logf.Logf) (*progress.Tracker, logf.Logf) {
	sink := fileSink
	runLogf := lf
	if c.d.RunSink != nil {
		sink = progress.MultiSink(fileSink, c.d.RunSink)
		runLogf = nil
	}
	return progress.NewTracker(runID, progress.PhaseRunning, workers, c.planProgress(items), time.Now, sink), runLogf
}

// keepEstimating adapts the estimate phase's status-file sink so the file stays
// non-terminal across the gap between sizing and the first dumped byte. The estimate
// tracker signals completion with a terminal PhaseDone — which a live display uses to
// erase its region — but to the file that would read as a finished run, stopping a
// `nb status --watch` before the dump it is waiting for has even started. Rewriting it
// to PhaseEstimating holds the file open until the dump phase claims it.
func keepEstimating(file progress.Sink) progress.Sink {
	return func(s progress.Snapshot, force bool) {
		if s.Phase.Terminal() {
			s.Phase = progress.PhaseEstimating
		}
		file(s, force)
	}
}

// failEstimated stamps the run-status file terminal for a failure in the prelude
// between sizing and the dump — preflight or make-room — when no dump tracker
// exists yet. The estimate phase has already written the file (non-terminal, so a
// watcher keeps waiting); without this stamp `nb status` and the web UI would show
// the dead run as "estimating" forever. The plan's DLEs are seeded pending —
// nothing was dumped — and the refusal itself becomes the snapshot's run-level Err.
func (c *Conductor) failEstimated(fileSink progress.Sink, plan *planner.Plan, err error) {
	tr := progress.NewTracker(progress.EstimateRunID, progress.PhaseEstimating, c.d.Workers, c.planProgress(plan.Items), time.Now, fileSink)
	tr.Fail(err)
}

// planProgress projects planner items onto the progress package's seed type,
// keeping progress unaware of the planner. The landing route rides along so the
// tracker can meter a fan-out's drains per landing.
func (c *Conductor) planProgress(items []planner.Item) []progress.Plan {
	out := make([]progress.Plan, len(items))
	for i, it := range items {
		out[i] = progress.Plan{Name: it.DLE.ID(), Slug: it.DLE.Name(), Level: it.Level, EstBytes: it.EstBytes, Landings: c.d.LandingsFor(it)}
	}
	return out
}
