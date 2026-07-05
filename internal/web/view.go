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

// drillsData backs the drills page: the coverage rollup, the per-DLE ledger, and
// the recent drill runs.
type drillsData struct {
	Window                  string   // formatted coverage window (e.g. "30d")
	Passing, Stale, Failing int      // ledger records by current health
	Never                   []string // configured DLEs never drilled (display names)
	Overdue                 int      // DLEs not covered within the window
	Ledger                  []drillLedgerRow
	Runs                    []drillRunRow
}

// drillLedgerRow is one DLE's last drill outcome — a row of the recoverability
// ledger, classified against the current time (failing / stale / ok).
type drillLedgerRow struct {
	DLE            string
	Status         string // "ok" | "stale" | "failing" — also the pill class
	Failing, Stale bool
	Tier           string
	What           string // one-line gloss of what the tier tested
	Medium         string
	AsOf           string
	RunID          string // target run the drill restored to (links to /runs/<id>)
	At             time.Time
	Age            string // formatted now-LastDrill (e.g. "3d 4h")
	Bytes          int64  // egress the drill read off the medium
	Drills         int    // total applied drills of this DLE
	Class, Detail  string // failure class + reason when failing
	Remedy         string // operator guidance for the failure class
}

// drillRunRow is one drill invocation from the run history, with its per-DLE
// outcomes.
type drillRunRow struct {
	EndedAt  time.Time
	Failed   bool
	Error    string
	Tier     string
	What     string // one-line gloss of what the tier tested
	Bytes    int64  // total egress the drill read
	Drilled  int    // targets actually exercised
	Failures int
	Skipped  int
	Overdue  int
	Targets  []drillTargetRow
}

// drillTargetRow is one DLE's outcome within a drill run.
type drillTargetRow struct {
	DLE       string
	OK        bool
	Drilled   bool // false = skipped (needed an operator)
	Class     string
	Degrading bool // passed before, failing now
	Bytes     int64
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
