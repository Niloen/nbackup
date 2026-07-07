package web

import (
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
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
	Alerts     []alert             // "attention needed" rollup; empty ⇒ the all-clear line
	LastDump   *report.Run         // most recent dump record, for the headline card
	RunCount   int                 // runs in the catalog
	TotalBytes int64               // cataloged bytes across all runs
	Media      []engine.MediumInfo // per-medium capacity summary
	History    []report.Run        // recent activity, newest-first
}

// alert is one row of the home "attention needed" rollup: a severity Level driving
// the pill/border color ("bad" red | "warn" amber), a short Tag naming the category,
// a one-line Text, and the detail page it links to. It carries no logic — the rollup
// computes it and the template renders it — so the visual stays consistent with the
// status pills the drills page already uses.
type alert struct {
	Level string // "bad" | "warn"
	Tag   string // short category label shown as a pill (e.g. "failed", "stale")
	Text  string
	Href  string
}

// statusView is a live snapshot with everything the status page shows pre-computed —
// the headline stat, the pipeline rollup, the per-DLE cards and grid cells — so the
// template does no arithmetic or branching (it has neither the clock the rates need
// nor the byte math the pipeline needs).
type statusView struct {
	Snap                          *progress.Snapshot
	Active, Done, Failed, Pending int
	Canceled                      int

	// Estimating flags the sizing prelude, during which the snapshot's fields mean
	// something else — a "done" DLE is merely *sized* and DoneBytes holds its measured
	// estimate, not dumped bytes — so the templates render the sizing view (the
	// per-DLE EstimateRows) instead of a dump view full of misleading "done" DLEs
	// (the same split nb status makes; see progress.renderEstimating).
	Estimating    bool
	Sized         int           // DLEs measured so far
	EstimateSoFar int64         // estimate accumulated so far
	EstimateRows  []estimateRow // per-DLE sizing detail, running first

	// Big/Sub are the headline's right-hand stat — the one decision-ready number the
	// page leads with (the ETA when known), and its quiet context line.
	Big, Sub string

	Pipe                pipeView // the run-level pipeline bar
	DumpRate, DrainRate int64    // bytes/sec (uncompressed / compressed), 0 = not yet measurable
	Elapsed, ETA        string   // formatted; ETA "" when unknown/terminal

	// FlushLanes itemizes the flush backlog per landing, populated only for a fan-out
	// (more than one landing) — mirroring nb status's per-landing lines (see
	// progress.render's Flush section). A single-landing run leaves this nil; the
	// pipeline bar already tells the whole story there.
	FlushLanes []flushLaneView

	// Only workers-many DLEs are in flight at once, so they alone get cards (a
	// miniature pipeline bar each); failures get alert rows up top, and the
	// done/pending majority collapses into the Grid — one square per DLE — instead
	// of a wall of rows. Each slice preserves Snap.DLEs order.
	ActiveDLEs []activeDLE    // StateDumping or StateFlushing — cards
	FailedDLEs []progress.DLE // StateFailed or StateCanceled — alert rows with the error
	Grid       []gridCell     // every DLE, one square each
}

// pipeView is the stacked pipeline bar: of the run's source data, how much is landed
// on the volume(s), staged in a holding disk, in flight, or still to go. Every
// segment is on the source (uncompressed) axis so they are commensurate — see
// stageSplit for how each DLE's bytes are placed. The *Pct fields are the segment
// widths (0..100); ToGo is the untracked remainder the bar leaves as empty track.
type pipeView struct {
	Landed, Holding, Dumping, ToGo, Total int64
	LandedPct, HoldingPct, DumpingPct     float64
}

// stageSplit places one DLE's bytes on the pipeline's source axis, by state:
// pending is all still-to-go (its estimate); a dumping DLE's bytes are in flight —
// uncommitted, whatever the route, so a direct dump's bytes hop straight from here
// to landed when its archive commits; a flushing DLE's dump is complete (its total
// is the actual DoneBytes, not the estimate) and splits landed/holding by its drain
// fraction; done is fully landed. Failed and canceled DLEs leave the bar entirely —
// they will never land, and the Failed section carries them — so a finished run's
// bar reads full, not stuck at the failures' share.
func stageSplit(d progress.DLE) (landed, holding, dumping, total int64) {
	switch d.State {
	case progress.StatePending:
		return 0, 0, 0, d.EstBytes
	case progress.StateDumping:
		total = d.EstBytes
		if d.DoneBytes > total {
			total = d.DoneBytes
		}
		return 0, 0, d.DoneBytes, total
	case progress.StateFlushing:
		landed = int64(float64(d.DoneBytes) * d.DrainPct() / 100)
		return landed, d.DoneBytes - landed, 0, d.DoneBytes
	case progress.StateDone:
		return d.DoneBytes, 0, 0, d.DoneBytes
	}
	return 0, 0, 0, 0
}

// newPipeView rolls the DLEs' stage splits into one bar.
func newPipeView(dles []progress.DLE) pipeView {
	var v pipeView
	for _, d := range dles {
		landed, holding, dumping, total := stageSplit(d)
		v.Landed += landed
		v.Holding += holding
		v.Dumping += dumping
		v.Total += total
	}
	if v.ToGo = v.Total - v.Landed - v.Holding - v.Dumping; v.ToGo < 0 {
		v.ToGo = 0
	}
	v.LandedPct = pctOf(v.Landed, v.Total)
	v.HoldingPct = pctOf(v.Holding, v.Total)
	v.DumpingPct = pctOf(v.Dumping, v.Total)
	return v
}

// activeDLE is one in-flight DLE's card: a miniature of the headline pipeline bar
// (same encoding, learned once) plus a prebuilt one-line caption, so the template
// renders it without branching.
type activeDLE struct {
	Name, Slug string
	Level      int
	State      string
	Pipe       pipeView
	Rate       int64  // dump throughput while dumping (bytes/sec), 0 when not measurable
	Direct     bool   // dumping straight to the landing in a run that also stages via holding
	Caption    string // pct/bytes/route in one quiet line
	Err        string
}

// newActiveDLE builds one card. mixed says the run stages other DLEs via a holding
// disk — only then does a direct dump earn its "direct" pill (it occupies a landing
// lane alongside the flushes, worth spotting); in an all-direct run the pill would
// be noise on every card.
func newActiveDLE(d progress.DLE, now time.Time, mixed bool) activeDLE {
	a := activeDLE{Name: d.Name, Slug: d.Slug, Level: d.Level, State: string(d.State),
		Pipe: newPipeView([]progress.DLE{d}), Err: d.Err}
	var parts []string
	switch d.State {
	case progress.StateDumping:
		if d.EstBytes > 0 {
			parts = append(parts, fmt.Sprintf("%.0f%%", d.Pct()),
				sizeutil.FormatBytes(d.DoneBytes)+" of ~"+sizeutil.FormatBytes(d.EstBytes))
		} else if d.DoneBytes > 0 {
			parts = append(parts, sizeutil.FormatBytes(d.DoneBytes))
		}
		if secs := now.Sub(d.StartedAt).Seconds(); !d.StartedAt.IsZero() && secs > 0 {
			a.Rate = int64(float64(d.DoneBytes) / secs)
		}
		switch {
		case d.ToHolding || d.Holding != "":
			parts = append(parts, "staging to holding")
		case mixed:
			a.Direct = true
			if d.Volume != "" {
				parts = append(parts, "direct → "+d.Volume)
			} else {
				parts = append(parts, "direct to landing")
			}
		case d.Volume != "":
			parts = append(parts, "volume "+d.Volume)
		}
	case progress.StateFlushing:
		if d.DrainBytes == 0 {
			parts = append(parts, "dumped", sizeutil.FormatBytes(d.OutBytes)+" in holding, awaiting flush")
		} else {
			parts = append(parts, "flushing", sizeutil.FormatBytes(d.DrainBytes)+" of "+
				sizeutil.FormatBytes(d.DrainTotal())+" to landing")
			if d.Holding != "" {
				parts = append(parts, "from "+d.Holding)
			}
		}
	}
	a.Caption = strings.Join(parts, " · ")
	return a
}

// gridCell is one DLE's square in the all-DLEs grid: its state as a color class
// (matching the pipeline's stage hues, plus failed red and the pending track), and
// a hover title naming it — so the done/pending majority reads at a glance and any
// square clicks through to its /dles page.
type gridCell struct {
	Slug, Class, Title string
}

// newGridCell maps one DLE to its square.
func newGridCell(d progress.DLE) gridCell {
	c := gridCell{Slug: d.Slug}
	switch d.State {
	case progress.StateDone:
		c.Class = "landed"
		c.Title = fmt.Sprintf("%s — done · %s", d.Name, sizeutil.FormatBytes(d.DoneBytes))
	case progress.StateFlushing:
		c.Class = "holding"
		c.Title = d.Name + " — flushing"
	case progress.StateDumping:
		c.Class = "dumping"
		c.Title = d.Name + " — dumping"
	case progress.StateFailed:
		c.Class = "failed"
		c.Title = d.Name + " — failed"
	case progress.StateCanceled:
		c.Class = "failed"
		c.Title = d.Name + " — canceled"
	default:
		c.Title = d.Name + " — pending"
	}
	return c
}

// estimateRow is one DLE's line of the /status sizing table — the web counterpart of
// nb status's estimating table (progress.renderEstimating, whose state/size/time
// helpers are unexported, so their small display rules are mirrored here). Size and
// Time are pre-rendered: the template has neither the clock the elapsed needs nor
// the sized-vs-n/a dash rules.
type estimateRow struct {
	Name, Slug string
	State      string // "sizing" | "sized" | "pending" | "failed" | "canceled"
	Size       string // measured size once sized ("n/a" when no estimate was produced), else "—"
	Time       string // sizing: elapsed so far; sized/failed: how long it took; else "—"
	Err        string
}

// newEstimateRow mirrors progress.renderEstimating's cells for one DLE.
func newEstimateRow(d progress.DLE, now time.Time) estimateRow {
	row := estimateRow{Name: d.Name, Slug: d.Slug, State: string(d.State), Size: "—", Time: "—", Err: d.Err}
	switch d.State {
	case progress.StateDumping:
		row.State = "sizing"
		if !d.StartedAt.IsZero() {
			row.Time = sizeutil.FormatElapsed(now.Sub(d.StartedAt))
		}
	case progress.StateDone:
		row.State = "sized"
		if d.DoneBytes > 0 {
			row.Size = sizeutil.FormatBytes(d.DoneBytes)
		} else {
			row.Size = "n/a"
		}
	}
	if d.State != progress.StateDumping && !d.StartedAt.IsZero() && !d.EndedAt.IsZero() {
		row.Time = sizeutil.FormatElapsed(d.EndedAt.Sub(d.StartedAt))
	}
	return row
}

// estimateRows builds the /status sizing table, actively-sizing DLEs first (they are
// what an operator watching a slow estimate is looking for), then the rest in
// snapshot (name) order.
func estimateRows(dles []progress.DLE, now time.Time) []estimateRow {
	rows := make([]estimateRow, 0, len(dles))
	for _, d := range dles {
		if d.State == progress.StateDumping {
			rows = append(rows, newEstimateRow(d, now))
		}
	}
	for _, d := range dles {
		if d.State != progress.StateDumping {
			rows = append(rows, newEstimateRow(d, now))
		}
	}
	return rows
}

// flushLaneView is one landing's flush backlog for the /status fan-out itemization —
// the web counterpart of progress.LandingDrain, with the percent and rate the
// template cannot compute pre-calculated.
type flushLaneView struct {
	Landing     string
	Done, Total int64
	Pct         float64
	Rate        int64 // bytes/sec, 0 = not yet measurable
}

// newStatusView tallies a snapshot for rendering, or returns nil for no live run. now
// is the reference instant for elapsed/rate/ETA of an in-flight run.
func newStatusView(snap *progress.Snapshot, now time.Time) *statusView {
	if snap == nil {
		return nil
	}
	v := &statusView{Snap: snap}
	v.Active, v.Done, v.Failed, v.Pending = snap.Counts()
	v.Canceled = snap.Canceled()
	v.Elapsed = sizeutil.FormatElapsed(snap.Elapsed(now))
	if snap.Phase == progress.PhaseEstimating {
		v.Estimating = true
		v.Sized = v.Done + v.Failed
		v.EstimateSoFar = snap.TotalDone()
		v.EstimateRows = estimateRows(snap.DLEs, now)
		v.Big = fmt.Sprintf("%d of %d DLE(s) measured", v.Sized, len(snap.DLEs))
		v.Sub = "~" + sizeutil.FormatBytes(v.EstimateSoFar) + " so far · elapsed " + v.Elapsed
		return v
	}
	v.Pipe = newPipeView(snap.DLEs)
	if r := snap.Rate(now); r > 0 {
		v.DumpRate = int64(r)
	}
	if r := snap.DrainRate(now); r > 0 {
		v.DrainRate = int64(r)
	}
	if eta, ok := snap.ETA(now); ok {
		v.ETA = sizeutil.FormatElapsed(eta)
	}
	switch {
	case v.ETA != "":
		v.Big = "~" + v.ETA + " left"
		v.Sub = fmt.Sprintf("elapsed %s · %d worker(s)", v.Elapsed, snap.Workers)
	case snap.Phase.Terminal():
		v.Big = v.Elapsed
		v.Sub = "total run time"
	default:
		v.Big = fmt.Sprintf("%.0f%% landed", v.Pipe.LandedPct)
		v.Sub = fmt.Sprintf("elapsed %s · %d worker(s)", v.Elapsed, snap.Workers)
	}
	if drains := snap.LandingDrains(); len(drains) > 1 {
		for _, ld := range drains {
			lane := flushLaneView{Landing: ld.Landing, Done: ld.Done, Total: ld.Total, Pct: pctOf(ld.Done, ld.Total)}
			if r := snap.LandingDrainRate(ld.Done, now); r > 0 {
				lane.Rate = int64(r)
			}
			v.FlushLanes = append(v.FlushLanes, lane)
		}
	}
	// A direct dump earns its "direct" pill only in a mixed run — one that stages
	// other DLEs via a holding disk (see newActiveDLE).
	mixed := false
	for _, d := range snap.DLEs {
		if d.ToHolding || d.Holding != "" {
			mixed = true
			break
		}
	}
	for _, d := range snap.DLEs {
		v.Grid = append(v.Grid, newGridCell(d))
		switch d.State {
		case progress.StateDumping, progress.StateFlushing:
			v.ActiveDLEs = append(v.ActiveDLEs, newActiveDLE(d, now, mixed))
		case progress.StateFailed, progress.StateCanceled:
			v.FailedDLEs = append(v.FailedDLEs, d)
		}
	}
	return v
}

// pctOf is done/total as a capped 0..100 percentage (0 when there is nothing to
// measure) — mirrors the unexported progress.pct, needed here for the per-landing
// flush lanes a template cannot compute itself.
func pctOf(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	if p := float64(done) / float64(total) * 100; p < 100 {
		return p
	}
	return 100
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

// runsData backs /runs: the run rows plus paging info for the "show all" toggle —
// the same recent-first cap /drills already applies to its run history
// (maxDrillRuns), generalized here since /runs has no cap of its own otherwise.
type runsData struct {
	Rows  []runRow
	Total int  // rows in the full history
	All   bool // true when ?all=1 requested the uncapped list
}

// historyData backs /report the same way runsData backs /runs.
type historyData struct {
	Rows  []historyRow
	Total int
	All   bool
}

// historyRow is one /report row: the run record plus its pre-computed per-command
// detail and wall-clock duration — the web mirror of report.detailCell (unexported,
// so its logic is reproduced in newHistoryRow) with the dump case's run id rendered
// as a link instead of plain text. Built here so report.html does no branching.
type historyRow struct {
	report.Run
	DetailPrefix string // dump only: the run id, for the /runs/<id> link; empty otherwise
	DetailRest   string // the rest of the detail cell (archives/bytes), or the whole cell for non-dump commands
	Duration     string // dash when either endpoint is unrecorded
}

// newHistoryRow builds one /report row from a run record.
func newHistoryRow(r report.Run) historyRow {
	row := historyRow{Run: r, Duration: runDuration(r)}
	switch r.Command {
	case report.CommandDump:
		if r.RunID == "" {
			row.DetailRest = "-"
		} else {
			row.DetailPrefix = r.RunID
			row.DetailRest = fmt.Sprintf("%d archive(s), %s", r.Archives, sizeutil.FormatBytes(r.BytesMoved))
		}
	case report.CommandSync:
		row.DetailRest = fmt.Sprintf("%d run(s) copied, %s", r.RunsCopied, sizeutil.FormatBytes(r.BytesMoved))
	case report.CommandPrune:
		row.DetailRest = fmt.Sprintf("%d archive(s) pruned, %s freed", r.ArchivesPruned, sizeutil.FormatBytes(r.BytesMoved))
	case report.CommandVerify:
		if r.Failures > 0 {
			row.DetailRest = fmt.Sprintf("%d run(s) failed verification", r.Failures)
		} else {
			row.DetailRest = "all verified"
		}
	case report.CommandDrill:
		parts := []string{fmt.Sprintf("%d failure(s)", r.Failures)}
		if r.Skipped > 0 {
			parts = append(parts, fmt.Sprintf("%d skipped", r.Skipped))
		}
		if r.Overdue > 0 {
			parts = append(parts, fmt.Sprintf("%d overdue", r.Overdue))
		}
		row.DetailRest = strings.Join(parts, ", ")
	default:
		row.DetailRest = "-"
	}
	return row
}

// runDuration renders a run's wall-clock span, or a dash when either endpoint is
// unrecorded.
func runDuration(r report.Run) string {
	if r.StartedAt.IsZero() || r.EndedAt.IsZero() {
		return "-"
	}
	return sizeutil.FormatElapsed(r.EndedAt.Sub(r.StartedAt))
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
	Dump     *dumpReportView // the run's dump-report section, or nil when the history holds no record for it
}

// dumpReportView is the /runs/<id> mirror of `nb report --dump`: the headline, the
// STATISTICS grid (Total/Full/Incr), and the per-DLE table. It reproduces
// report.RenderDump's arithmetic (report.headline/renderStats/renderDumpTable are
// unexported) so the template does none of it.
type dumpReportView struct {
	Headline string
	Grid     []dumpGridRow
	Rows     []dumpStatRow
}

// dumpGridRow is one row of the STATISTICS grid — mirrors report.renderStats.
type dumpGridRow struct {
	Label, Total, Full, Incr string
}

// dumpStatRow is one DLE's row of the per-DLE dump table — mirrors report.renderDumpTable.
type dumpStatRow struct {
	ID         string // host:path identity (falls back to the slug), via DLEStat.ID()
	Slug       string // internal DLE slug, for the /dles/<slug> link
	Level      int
	Orig, Out  string
	Comp       string // compression percent, or dash
	Files      int
	Time, Rate string // dash when timing was unavailable
}

// dumpAgg accumulates one column of the STATISTICS grid — mirrors report.agg.
type dumpAgg struct {
	n     int
	orig  int64
	out   int64
	files int
	secs  float64
}

func (a *dumpAgg) add(d report.DLEStat) {
	a.n++
	a.orig += d.Orig
	a.out += d.Out
	a.files += d.Files
	a.secs += d.Seconds
}

// newDumpReportView builds the /runs/<id> dump-report section from a dump record,
// or nil when it carries no per-DLE statistics (a run predating the run-log, or one
// compacted out).
func newDumpReportView(r report.Run) *dumpReportView {
	if len(r.DumpStats) == 0 {
		return nil
	}
	var tot, full, incr dumpAgg
	for _, d := range r.DumpStats {
		tot.add(d)
		if d.Level == 0 {
			full.add(d)
		} else {
			incr.add(d)
		}
	}
	v := &dumpReportView{
		Headline: dumpHeadline(r, tot),
		Grid:     dumpStatsGrid(tot, full, incr, r.EndedAt.Sub(r.StartedAt)),
	}
	for _, d := range r.DumpStats {
		v.Rows = append(v.Rows, dumpStatRow{
			ID: d.ID(), Slug: d.DLE, Level: d.Level,
			Orig: sizeutil.FormatBytes(d.Orig), Out: sizeutil.FormatBytes(d.Out),
			Comp: dumpCompPct(d.Orig, d.Out), Files: d.Files,
			Time: dumpTimeCell(d.Seconds), Rate: dumpRateCell(d.Orig, d.Seconds),
		})
	}
	return v
}

// dumpHeadline is the one-line "did it work" summary — mirrors report.headline.
func dumpHeadline(r report.Run, tot dumpAgg) string {
	sizes := fmt.Sprintf("%s -> %s (%s)", sizeutil.FormatBytes(tot.orig), sizeutil.FormatBytes(tot.out), dumpCompPct(tot.orig, tot.out))
	elapsed := sizeutil.FormatElapsed(r.EndedAt.Sub(r.StartedAt))
	if r.Failed() {
		return fmt.Sprintf("%d DLE(s) dumped, run FAILED [%s] · %s · %s elapsed", tot.n, r.ExitClass, sizes, elapsed)
	}
	return fmt.Sprintf("%d DLE(s) dumped OK · %s · %s elapsed", tot.n, sizes, elapsed)
}

// dumpStatsGrid builds the STATISTICS grid rows — mirrors report.renderStats,
// including its dash rules for an empty column.
func dumpStatsGrid(tot, full, incr dumpAgg, wall time.Duration) []dumpGridRow {
	count := func(a dumpAgg) string {
		if a.n == 0 {
			return "-"
		}
		return fmt.Sprintf("%d", a.n)
	}
	size := func(a dumpAgg) string {
		if a.n == 0 {
			return "-"
		}
		return sizeutil.FormatBytes(a.orig)
	}
	out := func(a dumpAgg) string {
		if a.n == 0 {
			return "-"
		}
		return sizeutil.FormatBytes(a.out)
	}
	files := func(a dumpAgg) string {
		if a.n == 0 {
			return "-"
		}
		return fmt.Sprintf("%d", a.files)
	}
	rows := []dumpGridRow{
		{"DLEs dumped", count(tot), count(full), count(incr)},
		{"Original size", size(tot), size(full), size(incr)},
		{"Output size", out(tot), out(full), out(incr)},
		{"Avg compression", dumpCompPct(tot.orig, tot.out), dumpCompPct(full.orig, full.out), dumpCompPct(incr.orig, incr.out)},
		{"Files", files(tot), files(full), files(incr)},
		{"Dump time (sum)", dumpTimeCell(tot.secs), dumpTimeCell(full.secs), dumpTimeCell(incr.secs)},
		{"Avg dump rate", dumpRateCell(tot.orig, tot.secs), dumpRateCell(full.orig, full.secs), dumpRateCell(incr.orig, incr.secs)},
	}
	if wall > 0 {
		rows = append(rows, dumpGridRow{"Run time (wall)", sizeutil.FormatElapsed(wall), "", ""})
	}
	return rows
}

// dumpCompPct renders the compression ratio, or a dash when there is no original
// size to measure against, or none was saved — mirrors report.compPct.
func dumpCompPct(orig, out int64) string {
	if orig <= 0 || out >= orig {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", float64(out)/float64(orig)*100)
}

// dumpTimeCell renders a dump duration, or a dash when timing was unavailable —
// mirrors report.dumpTime.
func dumpTimeCell(secs float64) string {
	if secs <= 0 {
		return "-"
	}
	return sizeutil.FormatElapsed(time.Duration(secs * float64(time.Second)))
}

// dumpRateCell renders uncompressed throughput, or a dash without timing — mirrors
// report.dumpRate.
func dumpRateCell(orig int64, secs float64) string {
	if secs <= 0 || orig <= 0 {
		return "-"
	}
	return sizeutil.FormatBytes(int64(float64(orig)/secs)) + "/s"
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

// dlesData backs the /dles page: the per-DLE rows plus the activity heatmap above
// them (nil when the catalog has no archives, so the section is omitted).
//
// Rows/Heat render flat, exactly as before, whenever the catalog's DLEs span at
// most one host (the common case). Once more than one host is present, Groups/
// HeatGroups carry the same rows sectioned by host instead — hosts are the actual
// failure domain (one flaky link or dead agent takes out every DLE on a box), so
// a list this size is worth reading as groups rather than N independent rows.
// Rows/Heat stay populated either way (some tests and any future flat consumer can
// still use them); the templates pick Groups/HeatGroups over Rows/Heat whenever
// the former are non-empty.
type dlesData struct {
	Rows []dleRow
	Heat *heatmap

	Groups     []dleGroup  // Sources rows sectioned by host; nil when ungrouped
	HeatGroups []heatGroup // heatmap rows sectioned by host, same grouping; nil when ungrouped
}

// dleHostSummary is the compact rollup shown in a /dles host-section header: the
// host name ("" for the trailing hostless "(other)" section), how many configured
// DLEs it covers, the newest backup across them, and how many are currently stale.
type dleHostSummary struct {
	Host   string
	Count  int
	Newest time.Time
	Stale  int
}

// dleGroup is one host's slice of the Sources table on /dles.
type dleGroup struct {
	Host dleHostSummary
	Rows []dleRow
}

// heatGroup mirrors dleGroup for the activity heatmap — same host, same DLE
// membership and order, just the heatRow shape instead of dleRow. Grouping only
// sections the existing rows; the per-cell markup is untouched.
type heatGroup struct {
	Host dleHostSummary
	Rows []heatRow
}

// hostOf extracts a DLE's host from its "host:path" display identity — the prefix
// before the first ':'. A display with no ':' (the bare-slug fallback for a DLE
// with no host segment) has no host: ok is false, and such a DLE is never grouped
// into a host section or coalesced into a host-level stale alert.
func hostOf(display string) (host string, ok bool) {
	i := strings.IndexByte(display, ':')
	if i < 0 {
		return "", false
	}
	return display[:i], true
}

// partitionByHost splits DLE summaries into per-host groups (in first-seen host
// order) and a hostless remainder, for /dles's grouping trigger and sections.
func partitionByHost(sums []catalog.DLESummary) (order []string, byHost map[string][]catalog.DLESummary, hostless []catalog.DLESummary) {
	byHost = map[string][]catalog.DLESummary{}
	seen := map[string]bool{}
	for _, d := range sums {
		if host, ok := hostOf(d.Display); ok {
			if !seen[host] {
				seen[host] = true
				order = append(order, host)
			}
			byHost[host] = append(byHost[host], d)
		} else {
			hostless = append(hostless, d)
		}
	}
	return
}

// groupDLEs builds the /dles page data from the catalog's DLE summaries, the
// currently-stale ones (for each host section's "K stale" count), and the
// pre-built heatmap. It renders flat (Groups/HeatGroups nil) whenever the
// summaries span at most one host — a single host, or all hostless — so that
// common case looks exactly as it did before grouping existed.
func groupDLEs(sums []catalog.DLESummary, stale []catalog.StaleDLE, heat *heatmap) dlesData {
	rows := make([]dleRow, 0, len(sums))
	for _, d := range sums {
		rows = append(rows, dleRow{
			Slug: d.DLE, Display: d.Display, Runs: d.Runs, LastLevel: d.LastLevel,
			LastFull: d.LastFull, Bytes: d.Bytes, Media: strings.Join(d.Media, ", "),
		})
	}
	data := dlesData{Rows: rows, Heat: heat}

	order, byHost, hostless := partitionByHost(sums)
	if len(order) <= 1 {
		return data
	}

	// StaleDLE carries no Display for a DLE that has never been backed up at all
	// (catalog.StaleDLEs), so fall back to the summary's, which every configured
	// DLE has.
	displayOf := map[string]string{}
	for _, d := range sums {
		displayOf[d.DLE] = d.Display
	}
	staleByHost := map[string]int{}
	for _, d := range stale {
		display := d.Display
		if display == "" {
			display = displayOf[d.DLE]
		}
		if host, ok := hostOf(display); ok {
			staleByHost[host]++
		}
	}

	rowBySlug := make(map[string]dleRow, len(rows))
	for _, r := range rows {
		rowBySlug[r.Slug] = r
	}
	heatBySlug := map[string]heatRow{}
	if heat != nil {
		for _, r := range heat.Rows {
			heatBySlug[r.Slug] = r
		}
	}

	build := func(members []catalog.DLESummary, host string) (dleGroup, heatGroup) {
		hs := dleHostSummary{Host: host, Count: len(members), Stale: staleByHost[host]}
		gr, hg := dleGroup{Host: hs}, heatGroup{Host: hs}
		var newest time.Time
		for _, m := range members {
			if m.LastBackupAt.After(newest) {
				newest = m.LastBackupAt
			}
			if r, ok := rowBySlug[m.DLE]; ok {
				gr.Rows = append(gr.Rows, r)
			}
			if hr, ok := heatBySlug[m.DLE]; ok {
				hg.Rows = append(hg.Rows, hr)
			}
		}
		gr.Host.Newest, hg.Host.Newest = newest, newest
		return gr, hg
	}

	for _, host := range order {
		gr, hg := build(byHost[host], host)
		data.Groups = append(data.Groups, gr)
		if heat != nil {
			data.HeatGroups = append(data.HeatGroups, hg)
		}
	}
	if len(hostless) > 0 {
		gr, hg := build(hostless, "")
		data.Groups = append(data.Groups, gr)
		if heat != nil {
			data.HeatGroups = append(data.HeatGroups, hg)
		}
	}
	return data
}

// stampOrDash formats a time like the "stamp" template func, for use from Go code
// (hostLabel) rather than a template.
func stampOrDash(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return sizeutil.FormatStamp(t)
}

// hostLabel renders a /dles host-section header: the host name (or "(other)" for
// the hostless trailer), its DLE count, the newest backup across them, and a
// "K stale" note in warn color when any of its DLEs are currently stale. Shared by
// the heatmap and Sources sections so both group the same host the same way.
func hostLabel(h dleHostSummary) template.HTML {
	name := h.Host
	if name == "" {
		name = "(other)"
	}
	unit := "DLEs"
	if h.Count == 1 {
		unit = "DLE"
	}
	s := fmt.Sprintf("<strong>%s</strong> · %d %s · newest %s",
		template.HTMLEscapeString(name), h.Count, unit, template.HTMLEscapeString(stampOrDash(h.Newest)))
	if h.Stale > 0 {
		s += fmt.Sprintf(` · <span class="warn">%d stale</span>`, h.Stale)
	}
	return template.HTML(s)
}

// dleDetail backs the single-DLE page: the rollup, the size/time trend chart, the
// recovery points, and this DLE's per-run history.
type dleDetail struct {
	NotFound bool
	Slug     string
	Display  string
	Runs     int
	Bytes    int64
	Media    string
	Trend    template.HTML   // the size-over-time chart, or "" with fewer than two dump records
	Recovery []recoveryPoint // restorable points, newest first (capped unless ?all=1)
	RecTotal int             // recovery points in total (for the show-all toggle)
	RecAll   bool            // true when ?all=1 lifted the cap
	History  []dleArchiveRow
}

// recoveryPoint is one point in time a DLE can be restored to — an archive of this
// DLE in the catalog, paired with the transitive base chain a restore replays, its
// chain health, the media that hold it, and whether a drill has proven it. It answers
// the product's core question: "which points can I restore to, from where, and has it
// been proven?" The view builds it; the template only renders it.
type recoveryPoint struct {
	RunID    string
	Date     string
	Level    int
	At       time.Time
	Chain    string // "L2 ← L1 ← full run-…", or "full" for a level-0 point
	Broken   bool   // a chain member is missing or has no surviving copy
	Reason   string // when broken: which link is missing (e.g. "base run-X has no copy")
	OnePlace bool   // the whole chain lives on a single medium (the "restore from one place" answer)
	Media    string // the media holding the whole chain (OnePlace), else a per-member list
	Drilled  bool   // a drill restored exactly this point and passed
	Gloss    string // newest drilled point only: the ledger's tier gloss
}

// chainMember is one archive in a recovery point's restore chain, with the media
// currently holding it (archive-granular, so a reclaimed copy shows as unheld).
type chainMember struct {
	RunID string
	Level int
	Media []string // media holding this archive; empty = no surviving copy
}

// chainDesc renders a restore chain tip-first with each base to its right — "L2 ← L1
// ← full run-…" — naming the full's run (a different run than the tip) so the
// point-in-time base reads at a glance; a lone level-0 point is just "full".
func chainDesc(members []chainMember) string {
	if len(members) == 1 && members[0].Level == 0 {
		return "full"
	}
	parts := make([]string, 0, len(members))
	for _, m := range members {
		if m.Level == 0 {
			parts = append(parts, "full "+m.RunID)
		} else {
			parts = append(parts, levelTag(m.Level))
		}
	}
	return strings.Join(parts, " ← ")
}

// chainMedia answers "restore from one place": the media that hold every chain member
// (a medium counts only if it holds them all). When no single medium holds the whole
// chain it falls back to a compact per-member listing so the split is still legible.
func chainMedia(members []chainMember) (onePlace bool, text string) {
	if len(members) == 0 {
		return false, "—"
	}
	count := map[string]int{}
	for _, m := range members {
		for _, name := range m.Media {
			count[name]++
		}
	}
	var whole []string
	for name, c := range count {
		if c == len(members) {
			whole = append(whole, name)
		}
	}
	if len(whole) > 0 {
		sort.Strings(whole)
		return true, strings.Join(whole, ", ")
	}
	segs := make([]string, 0, len(members))
	for _, m := range members {
		media := "none"
		if len(m.Media) > 0 {
			media = strings.Join(m.Media, ", ")
		}
		segs = append(segs, levelTag(m.Level)+": "+media)
	}
	return false, strings.Join(segs, " · ")
}

// heatmap is the /dles activity matrix: a DLE × day grid (amoverview, webified). Days
// run oldest→newest left-to-right; each row is one configured DLE.
type heatmap struct {
	Days []heatDay
	Rows []heatRow
}

// heatDay is one column: its date and a sparse tick label (a month on the 1st, a day
// number on Mondays, else "").
type heatDay struct {
	Tick string
}

// heatRow is one DLE's row of daily cells, in Days order.
type heatRow struct {
	Slug    string
	Display string
	Cells   []heatCell
}

// heatCell is one DLE-day: the accent class driving the fill, a tooltip, and — when a
// single run produced the day's archive — that run to link to.
type heatCell struct {
	Class string // "full" | "incr" | "partial" | "none"
	Title string
	RunID string // set only when exactly one run produced the day's archive
}

// dleTrendPoint is one dump record's statistics for a single DLE, the per-DLE series
// drawn on /dles/<slug> — the DumpStats row (report.DLEStat) paired with the run's
// time, oldest first.
type dleTrendPoint struct {
	At      time.Time
	Orig    int64
	Out     int64
	Seconds float64
	Level   int
}

// dleTrendSVG renders a DLE's dump history as an inline SVG line chart: two series
// (original and output bytes) so the compression ratio's stability is visible, with
// full dumps (Level==0) ringed so the cycle rhythm shows against the incrementals.
// Mirrors usageChartSVG's conventions (baseline, end labels, date labels, ≤80
// markers cap, per-point hover title); "" for fewer than two points or a zero span,
// where a line would be meaningless.
func dleTrendSVG(points []dleTrendPoint) template.HTML {
	if len(points) < 2 {
		return ""
	}
	first, last := points[0].At, points[len(points)-1].At
	span := last.Sub(first)
	if span <= 0 {
		return ""
	}
	var peak int64
	for _, p := range points {
		if p.Orig > peak {
			peak = p.Orig
		}
		if p.Out > peak {
			peak = p.Out
		}
	}
	if peak <= 0 {
		return ""
	}
	const vw, vh = 760.0, 220.0
	const padL, padR, padT, padB = 8.0, 8.0, 12.0, 26.0
	plotW, plotH := vw-padL-padR, vh-padT-padB
	baseY := padT + plotH
	scale := int64(float64(peak) * 1.08)
	x := func(t time.Time) float64 { return padL + float64(t.Sub(first))/float64(span)*plotW }
	y := func(v int64) float64 { return padT + (1-float64(v)/float64(scale))*plotH }

	path := func(get func(dleTrendPoint) int64) string {
		var b strings.Builder
		for i, p := range points {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&b, "%s%.1f %.1f ", cmd, x(p.At), y(get(p)))
		}
		return strings.TrimSpace(b.String())
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" style="width:100%%;height:auto;display:block" role="img" aria-label="dump size over time">`, vw, vh)
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--line)" stroke-width="1"/>`, padL, baseY, vw-padR, baseY)
	fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="var(--accent)" stroke-width="2"/>`, path(func(p dleTrendPoint) int64 { return p.Orig }))
	fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="var(--ok)" stroke-width="2"/>`, path(func(p dleTrendPoint) int64 { return p.Out }))
	if len(points) <= 80 {
		for _, p := range points {
			title := fmt.Sprintf("%s — %s — orig %s, out %s%s", p.At.Format("2006-01-02 15:04"), levelTag(p.Level),
				sizeutil.FormatBytes(p.Orig), sizeutil.FormatBytes(p.Out), dumpTimeSuffix(p.Seconds))
			ring := ""
			if p.Level == 0 { // a full dump gets a ring so the cycle rhythm reads at a glance
				ring = ` stroke="var(--fg)" stroke-width="1.5"`
			}
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" fill="var(--accent)"%s><title>%s</title></circle>`, x(p.At), y(p.Orig), ring, title)
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" fill="var(--ok)"%s><title>%s</title></circle>`, x(p.At), y(p.Out), ring, title)
		}
	}
	end := points[len(points)-1]
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--fg)" font-size="11" text-anchor="end">%s</text>`, x(end.At), y(end.Orig)-6, sizeutil.FormatBytes(end.Orig))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--fg)" font-size="11" text-anchor="end">%s</text>`, x(end.At), y(end.Out)+14, sizeutil.FormatBytes(end.Out))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11">%s</text>`, padL, vh-8, first.Format("2006-01-02"))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11" text-anchor="end">%s</text>`, vw-padR, vh-8, last.Format("2006-01-02"))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// levelTag renders a dump level for the trend chart's hover title.
func levelTag(l int) string { return fmt.Sprintf("L%d", l) }

// dumpTimeSuffix is the ", Ns dump" hover-title suffix, or "" when timing was
// unavailable.
func dumpTimeSuffix(secs float64) string {
	if secs <= 0 {
		return ""
	}
	return ", " + dumpTimeCell(secs) + " dump"
}

// mediaRow is one row of the /media list: a medium's summary (engine.MediumInfo)
// enriched with the utilization fraction and growth-forecast column, which the
// template cannot compute — the read-only merge of Media() with each medium's
// MediumStats. For a labeled pool (Volumes > 0 — tape libraries/stations; never
// keyed on medium type, per the media layer's neutrality) the Utilization/Projected
// full columns are structurally misleading (a healthy rotation keeps most volumes
// permanently near-full by design), so Pool routes the template to PoolRoom instead.
type mediaRow struct {
	engine.MediumInfo
	UtilPct  float64 // 0 when unbounded
	Over     bool    // used past a bounded capacity (sync/copy can land runs over)
	ProjFull string  // "~Nd", or "—" when unbounded or not projected
	Pool     bool    // this medium's pool holds one or more labeled volumes
	PoolRoom string  // "K of N with room" — set only when Pool
}

// newMediaRows merges the medium summaries with each one's recorded growth
// (MediumStats) into the /media list's view rows. stats is a medium-name lookup
// (Source.MediumStats) rather than the whole Source, so this stays a pure view
// function callable from a test fixture as well as the handler.
func newMediaRows(media []engine.MediumInfo, stats func(string) (engine.MediumStats, bool), now time.Time) []mediaRow {
	rows := make([]mediaRow, 0, len(media))
	for _, m := range media {
		row := mediaRow{MediumInfo: m, ProjFull: "—"}
		if m.Capacity > 0 {
			row.UtilPct = float64(m.Used) / float64(m.Capacity) * 100
			row.Over = m.Used > m.Capacity
		}
		st, ok := stats(m.Name)
		if m.Volumes > 0 {
			row.Pool = true
			withRoom, total := poolRoomCount(st.PerVolume, st.PoolVolumes)
			row.PoolRoom = fmt.Sprintf("%d of %d with room", withRoom, total)
		} else if ok && !st.Growth.ProjFull.IsZero() {
			row.ProjFull = fmt.Sprintf("~%dd", projDays(st.Growth.ProjFull, now))
		}
		rows = append(rows, row)
	}
	return rows
}

// poolRoomCount reports how many of a pool's labeled volumes still have room to
// write, against the pool's configured slot count (falling back to the labeled
// count itself when no configured total is known — see MediumStats.PoolVolumes).
func poolRoomCount(pv []engine.VolumeUsage, configured int64) (withRoom int, total int64) {
	total = configured
	if total == 0 {
		total = int64(len(pv))
	}
	for _, v := range pv {
		if v.HasRoom {
			withRoom++
		}
	}
	return withRoom, total
}

// projDays rounds a projected-full instant to whole days from now, floored at 0 (a
// projection landing in the past, from a stale sample, reads as "any day now" rather
// than a negative count).
func projDays(projFull, now time.Time) int {
	d := int(projFull.Sub(now).Hours()/24 + 0.5)
	if d < 0 {
		return 0
	}
	return d
}

// mediumData backs one medium's detail page: the capacity headline, the
// full/incremental split, the growth projection, the used-capacity-over-time chart
// (pre-rendered to inline SVG so the template does no arithmetic), and the per-run
// usage table.
type mediumData struct {
	NotFound bool
	Name     string
	Type     string

	Used     int64
	Capacity int64   // 0 = unbounded
	UtilPct  float64 // Used/Capacity as a percent; 0 when unbounded
	Over     bool    // used past a bounded capacity (sync/copy can land runs over)
	Free     int64   // Capacity-Used when bounded, else 0

	Runs     int
	Archives int
	Volumes  int
	Volume   string // single-volume label (with epoch), or "" for a pool/address-identified medium

	FullBytes int64
	IncrBytes int64
	FullPct   float64 // full share of used, for the split bar

	First time.Time // earliest / latest retained-archive commit (the composition span)
	Last  time.Time

	// From the persisted usage history (package usage): the true used-over-time record.
	Samples   int       // recorded usage samples for this medium
	HistFirst time.Time // first / last recorded sample
	HistLast  time.Time
	PerDay    int64     // average recorded growth, bytes/day; 0 when not derivable
	ProjFull  time.Time // projected capacity-reached date; zero when not projected

	Chart  template.HTML   // the used-over-time area chart, or "" with fewer than two samples
	Points []usagePointRow // the per-run retained series, newest run first

	// Pool-only fields, set when the medium's pool holds one or more labeled
	// volumes (Volumes > 0). A healthy tape rotation keeps most volumes permanently
	// near-full by retention design, so the aggregate Used/Capacity/Growth headline
	// above is a structurally false signal for a pool; VolumeRows is what the page
	// renders instead.
	Pool           bool
	PoolVolumes    int64       // the pool's configured slot count
	UnlabeledSlots int64       // PoolVolumes - len(VolumeRows), when positive
	WithData       int         // labeled volumes holding at least one archive
	WithRoom       int         // labeled volumes with room to write more
	VolumeRows     []volumeRow // one row per labeled volume
}

// volumeRow is one labeled volume's row in a pool medium's inventory table.
type volumeRow struct {
	Label    string
	Epoch    int
	Barcode  string
	Bytes    int64
	Capacity int64   // 0 = unbounded (per-volume capacity not derivable)
	FillPct  float64 // Bytes/Capacity as a percent; 0 when unbounded
	Runs     int
	Archives int
	Last     time.Time // zero if the volume holds nothing yet
	// State is the pill/bar vocabulary for the reel's no-room reason, distinguishing
	// two very different situations accounting.VolumeUsage.HasRoom collapses into one
	// bool: "room" (more can still be written), "used" (a non-appendable reel already
	// holds a run — the rotation working as designed, not an error), or "full" (byte
	// capacity actually reached). See volumeState.
	State string
}

// volumeState classifies a labeled volume's no-room reason for display. HasRoom
// already encodes the medium's appendable policy (see accounting.volumeHasRoom), so
// State needs no policy input of its own: "room" whenever HasRoom is true; "full"
// only when bytes have actually reached a known per-volume capacity (true for both
// appendable and non-appendable media); otherwise "used" — a non-appendable reel
// that already holds a run but isn't byte-full, which is the rotation working as
// intended and must not read as an error state.
func volumeState(v engine.VolumeUsage) string {
	switch {
	case v.HasRoom:
		return "room"
	case v.Capacity > 0 && v.Used >= v.Capacity:
		return "full"
	default:
		return "used"
	}
}

// usagePointRow is one run's contribution to a medium's usage, for the detail table.
type usagePointRow struct {
	Run   string
	At    time.Time
	Added int64
	Used  int64   // cumulative on the medium after this run
	Pct   float64 // cumulative as a percent of capacity; 0 when unbounded
}

// newMediumData flattens a medium's usage picture (engine.MediumStats: the retained
// composition, the catalog's recorded usage ledger, and the growth statistics over
// it) into the view model — display percentages plus the used-over-time chart drawn
// from the ledger samples, which show the prune/relabel declines the retained
// picture cannot.
func newMediumData(st engine.MediumStats) mediumData {
	d := mediumData{
		Name: st.Name, Type: st.Type,
		Used: st.Used, Capacity: st.Capacity, Over: st.Capacity > 0 && st.Used > st.Capacity,
		Runs: st.Runs, Archives: st.Archives, Volumes: st.Volumes,
		FullBytes: st.FullBytes, IncrBytes: st.IncrBytes,
		First: st.First, Last: st.Last,
	}
	if st.Volume != "" {
		d.Volume = st.Volume
		if st.Epoch > 0 {
			d.Volume = fmt.Sprintf("%s (epoch %d)", st.Volume, st.Epoch)
		}
	}
	if st.Capacity > 0 {
		d.UtilPct = float64(st.Used) / float64(st.Capacity) * 100
		if st.Used < st.Capacity {
			d.Free = st.Capacity - st.Used
		}
	}
	if st.Used > 0 {
		d.FullPct = float64(st.FullBytes) / float64(st.Used) * 100
	}
	// Growth + fill projection come from the recorded ledger, not the retained-archive
	// span, so a prune's decline is reflected rather than hidden.
	d.Samples, d.PerDay, d.ProjFull = st.Growth.Samples, st.Growth.PerDay, st.Growth.ProjFull
	if st.Growth.Samples >= 2 {
		d.HistFirst, d.HistLast = st.Growth.First, st.Growth.Last
	}
	capLabel := "capacity"
	d.Pool = len(st.PerVolume) > 0
	if d.Pool {
		capLabel = "pool capacity"
		d.PoolVolumes = st.PoolVolumes
		if gap := st.PoolVolumes - int64(len(st.PerVolume)); gap > 0 {
			d.UnlabeledSlots = gap
		}
		// The aggregate growth/projection is suppressed for a pool (a recycling
		// rotation's sawtooth defeats a linear fill projection); Room replaces it.
		d.PerDay, d.ProjFull = 0, time.Time{}
		for _, v := range st.PerVolume {
			row := volumeRow{
				Label: v.Label, Epoch: v.Epoch, Barcode: v.Barcode,
				Bytes: v.Bytes, Capacity: v.Capacity, Runs: v.Runs, Archives: v.Archives,
				Last: v.Last, State: volumeState(v),
			}
			if v.Capacity > 0 {
				row.FillPct = float64(v.Used) / float64(v.Capacity) * 100
			}
			if v.Bytes > 0 || v.Archives > 0 {
				d.WithData++
			}
			if v.HasRoom {
				d.WithRoom++
			}
			d.VolumeRows = append(d.VolumeRows, row)
		}
	}
	d.Chart = usageChartSVG(st.Usage, st.Capacity, capLabel)
	for i := len(st.ByRun) - 1; i >= 0; i-- { // newest first for the table
		p := st.ByRun[i]
		row := usagePointRow{Run: p.Run, At: p.At, Added: p.Added, Used: p.Used}
		if st.Capacity > 0 {
			row.Pct = float64(p.Used) / float64(st.Capacity) * 100
		}
		d.Points = append(d.Points, row)
	}
	return d
}

// usageChartSVG renders a medium's recorded used-capacity ledger as a self-contained
// inline SVG area chart: no external assets (the strict artifact-style CSP the webui
// keeps), colors driven by the page's CSS variables so it tracks light/dark, and the
// geometry computed here so the template stays declarative. Because it draws the
// catalog's recorded samples (not the currently-retained archives), a prune shows as
// the curve falling. It returns "" for fewer than two samples or a zero time span,
// where a line would be meaningless. Coordinates are safe (numbers plus datestamp
// timestamps), so template.HTML is sound. capLabel names the dashed ceiling line
// ("capacity", or "pool capacity" for a labeled pool, where the number is the sum
// of per-volume capacities rather than a single store's size).
func usageChartSVG(series []catalog.UsageSample, capacity int64, capLabel string) template.HTML {
	if len(series) < 2 {
		return ""
	}
	first, last := series[0].At, series[len(series)-1].At
	span := last.Sub(first)
	if span <= 0 {
		return ""
	}
	const vw, vh = 760.0, 220.0
	const padL, padR, padT, padB = 8.0, 8.0, 12.0, 26.0
	plotW, plotH := vw-padL-padR, vh-padT-padB
	baseY := padT + plotH

	var peak int64 // the tallest point the curve reaches (a prune can put it above the end)
	for _, s := range series {
		if s.Used > peak {
			peak = s.Used
		}
	}
	// Scale to whichever is taller — capacity as the ceiling, or (when usage has run
	// over capacity) the peak plus headroom. Either way the capacity line stays on
	// scale and visible, most of all in the over-capacity case.
	scale := capacity
	if peak > scale {
		scale = int64(float64(peak) * 1.08)
	}
	if scale <= 0 {
		return ""
	}
	drawCap := capacity > 0 && capacity <= scale
	x := func(t time.Time) float64 { return padL + float64(t.Sub(first))/float64(span)*plotW }
	y := func(v int64) float64 { return padT + (1-float64(v)/float64(scale))*plotH }

	var line, area strings.Builder
	for i, s := range series {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&line, "%s%.1f %.1f ", cmd, x(s.At), y(s.Used))
	}
	fmt.Fprintf(&area, "M%.1f %.1f ", x(first), baseY)
	for _, s := range series {
		fmt.Fprintf(&area, "L%.1f %.1f ", x(s.At), y(s.Used))
	}
	fmt.Fprintf(&area, "L%.1f %.1f Z", x(last), baseY)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" style="width:100%%;height:auto;display:block" role="img" aria-label="used capacity over time">`, vw, vh)
	// Baseline.
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--line)" stroke-width="1"/>`, padL, baseY, vw-padR, baseY)
	// Capacity ceiling, when a bounded medium.
	if drawCap {
		cy := y(capacity)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--warn)" stroke-width="1" stroke-dasharray="4 3"/>`, padL, cy, vw-padR, cy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--warn)" font-size="11" text-anchor="end">%s %s</text>`, vw-padR, cy-3, capLabel, sizeutil.FormatBytes(capacity))
	}
	// Area + line.
	fmt.Fprintf(&b, `<path d="%s" fill="var(--accent)" fill-opacity="0.15"/>`, strings.TrimSpace(area.String()))
	fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="var(--accent)" stroke-width="2"/>`, strings.TrimSpace(line.String()))
	// Point markers (bounded, so a long history stays light), each with a hover title.
	if len(series) <= 80 {
		for _, s := range series {
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2.5" fill="var(--accent)"><title>%s — %s</title></circle>`,
				x(s.At), y(s.Used), s.At.Format("2006-01-02 15:04"), sizeutil.FormatBytes(s.Used))
		}
	}
	// End label.
	end := series[len(series)-1]
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--fg)" font-size="11" text-anchor="end">%s</text>`,
		x(end.At), y(end.Used)-6, sizeutil.FormatBytes(end.Used))
	// X-axis end dates.
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11">%s</text>`, padL, vh-8, first.Format("2006-01-02"))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11" text-anchor="end">%s</text>`, vw-padR, vh-8, last.Format("2006-01-02"))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// drillsData backs the drills page: the coverage rollup, the per-DLE ledger, and
// the recent drill runs.
type drillsData struct {
	Window                  string    // formatted coverage window (e.g. "30d")
	Passing, Stale, Failing int       // ledger records by current health
	Never                   []dleLink // configured DLEs never drilled
	Overdue                 int       // DLEs not covered within the window
	Ledger                  []drillLedgerRow
	Runs                    []drillRunRow
}

// drillLedgerRow is one DLE's last drill outcome — a row of the recoverability
// ledger, classified against the current time (failing / stale / ok).
type drillLedgerRow struct {
	DLE            string
	Slug           string // internal DLE slug, for the /dles/<slug> link
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

// dleLink is a display name paired with its internal slug, for the "Never drilled"
// list to link back to /dles/<slug> rather than showing bare text.
type dleLink struct {
	Slug, Display string
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
	Slug      string // internal DLE slug, for the /dles/<slug> link
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
