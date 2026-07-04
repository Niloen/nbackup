package web

import (
	"time"

	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/report"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// page is the envelope every template renders inside: the shared layout reads Title,
// Active (the nav item to highlight), and Refresh (auto-refresh seconds, 0 = off),
// and hands Data to the page body.
type page struct {
	Title   string
	Active  string
	Refresh int
	Now     time.Time
	Data    any
}

// homeData backs the overview page.
type homeData struct {
	Live       *statusView         // a run in progress, or nil at rest
	LastDump   *report.Run         // most recent dump record, for the headline card
	RunCount   int                 // runs in the catalog
	TotalBytes int64               // cataloged bytes across all runs
	Media      []engine.MediumInfo // per-medium capacity summary
	History    []report.Run        // recent activity, newest-first
}

// statusView is a live snapshot with its run-level rollups pre-computed: the DLE
// counts (a template cannot destructure Counts()'s four returns) and the
// dump/flush/volume totals plus now-based rates and ETA (which need the clock a
// template does not have). Per-DLE detail is read straight off Snap.DLEs, whose
// Pct/DrainPct/Drains helpers a template can call directly.
type statusView struct {
	Snap                          *progress.Snapshot
	Active, Done, Failed, Pending int

	DumpDone, DumpEst int64   // uncompressed: dumped so far, and the planner estimate
	DumpRate          int64   // bytes/sec, 0 = not yet measurable
	HasFlush          bool    // a holding-disk run: some DLEs drain to the landing
	Drained, ToDrain  int64   // compressed: copied to the landing so far, and the staged total
	DrainPct          float64 // 0..100
	DrainRate         int64   // bytes/sec, 0 = not yet measurable
	OnVolume          int64   // bytes landed on the authoritative volume
	Elapsed, ETA      string  // formatted; ETA "" when unknown/terminal
}

// newStatusView tallies a snapshot for rendering, or returns nil for no live run. now
// is the reference instant for elapsed/rate/ETA of an in-flight run.
func newStatusView(snap *progress.Snapshot, now time.Time) *statusView {
	if snap == nil {
		return nil
	}
	v := &statusView{Snap: snap}
	v.Active, v.Done, v.Failed, v.Pending = snap.Counts()
	v.DumpDone, v.DumpEst = snap.TotalDone(), snap.TotalEst()
	v.ToDrain, v.Drained = snap.TotalToDrain(), snap.TotalDrained()
	v.HasFlush = v.ToDrain > 0
	v.DrainPct = snap.DrainPct()
	v.OnVolume = snap.TotalOnVolume()
	v.Elapsed = sizeutil.FormatElapsed(snap.Elapsed(now))
	if r := snap.Rate(now); r > 0 {
		v.DumpRate = int64(r)
	}
	if r := snap.DrainRate(now); r > 0 {
		v.DrainRate = int64(r)
	}
	if eta, ok := snap.ETA(now); ok {
		v.ETA = sizeutil.FormatElapsed(eta)
	}
	return v
}

// runRow is one line of the runs list.
type runRow struct {
	ID       string
	Partial  bool
	Archives int
	Bytes    int64
	At       time.Time
	Copies   string
}

// runDetail backs the single-run page.
type runDetail struct {
	NotFound bool
	ID       string
	Date     string
	At       time.Time
	Bytes    int64
	Partial  bool
	Archives []record.Archive
	Copies   []copyRow
}

// copyRow is one placement of a run (a medium it was written to).
type copyRow struct {
	Medium string
	Labels string
}

// dleRow is one line of the DLEs list — the DLE-major catalog rollup (catalog.DLESummary
// flattened for the template, with Media pre-joined).
type dleRow struct {
	Slug      string
	Display   string
	Runs      int
	LastLevel int
	LastFull  string
	Bytes     int64
	Media     string
}

// dleDetail backs the single-DLE page: the rollup plus this DLE's per-run history.
type dleDetail struct {
	NotFound bool
	Slug     string
	Display  string
	Runs     int
	Bytes    int64
	Media    string
	History  []dleArchiveRow
}

// dleArchiveRow is one run's archive for a DLE — a row of the DLE history, linking
// back to the run it belongs to.
type dleArchiveRow struct {
	RunID   string
	Date    string
	Level   int
	Bytes   int64
	At      time.Time
	Files   int
	Partial bool
	Copies  string
}
