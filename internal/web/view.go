package web

import (
	"fmt"
	"html/template"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/dletree"
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
	Sized         int                         // DLEs measured so far
	EstimateSoFar int64                       // estimate accumulated so far
	EstimateRows  []pathGroupRow[estimateRow] // per-DLE sizing detail, path-arranged

	// Big/Sub are the headline's right-hand stat — the one decision-ready number the
	// page leads with (the ETA when known), and its quiet context line.
	Big, Sub string

	Pipe pipeView // the run-level pipeline bar
	// DumpRates/FlushRates are the headline rate cells, prebuilt by
	// progress.DumpRates/WriteRates (shared with nb status so the wording never
	// drifts): trailing-window "now" rate leading, busy-time average + utilization
	// after, "idle" naming a waiting flush lane. FlushRates covers the
	// single-landing run; a fan-out itemizes in FlushLanes instead.
	DumpRates, FlushRates string
	FlushLabel            string // "flush" for a draining lane, "writing" for a direct one
	Elapsed, ETA          string // formatted; ETA "" when unknown/terminal

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

// newActiveDLE builds one card. rateNow is the DLE's trailing-window dump rate
// (progress.DLERateNow); a card younger than a couple of samples falls back to its
// own lifetime average, which over so short a life means the same thing. mixed says
// the run stages other DLEs via a holding disk — only then does a direct dump earn
// its "direct" pill (it occupies a landing lane alongside the flushes, worth
// spotting); in an all-direct run the pill would be noise on every card.
func newActiveDLE(d progress.DLE, now time.Time, rateNow float64, mixed bool) activeDLE {
	a := activeDLE{Name: d.Name, Slug: d.Slug, Level: d.Level, State: string(d.State),
		Pipe: newPipeView([]progress.DLE{d}), Err: d.Err}
	var parts []string
	switch d.State {
	case progress.StateDumping:
		approx := "" // inferred DoneBytes (client-fused remote dump) reads as "~", like nb status
		if d.DoneApprox {
			approx = "~"
		}
		if d.EstBytes > 0 {
			parts = append(parts, fmt.Sprintf("%s%.0f%%", approx, d.Pct()),
				approx+sizeutil.FormatBytes(d.DoneBytes)+" of ~"+sizeutil.FormatBytes(d.EstBytes))
		} else if d.DoneBytes > 0 {
			parts = append(parts, approx+sizeutil.FormatBytes(d.DoneBytes))
		}
		if rateNow > 0 {
			a.Rate = int64(rateNow)
		} else if secs := now.Sub(d.StartedAt).Seconds(); !d.StartedAt.IsZero() && secs > 0 {
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

// estimateRows builds the /status sizing table, path-arranged like every other
// per-DLE table (a partitioned source's fifty children fold under one header);
// the actively-sizing rows read at a glance from their bright pill.
func estimateRows(dles []progress.DLE, now time.Time) []pathGroupRow[estimateRow] {
	rows := make([]estimateRow, 0, len(dles))
	items := make([]dletree.Item, 0, len(dles))
	for _, d := range dles {
		rows = append(rows, newEstimateRow(d, now))
		it, _ := dletree.Split(d.Name)
		it.Rest = d.Rest
		items = append(items, it)
	}
	return groupRowsByPath(rows, items)
}

// flushLaneView is one landing's flush backlog for the /status fan-out itemization —
// the web counterpart of progress.LandingDrain, with the percent and rate the
// template cannot compute pre-calculated.
type flushLaneView struct {
	Landing     string
	Done, Total int64
	Pct         float64
	Rates       string // prebuilt rate cell (progress.WriteRates), "" when nothing measurable
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
	v.DumpRates = progress.DumpRates(*snap, now)
	// The headline flush cell covers the single-lane run (one landing drained, or an
	// all-direct run's one metered lane); a fan-out itemizes per lane below instead.
	drains := snap.LandingDrains()
	switch {
	case len(drains) == 1:
		v.FlushLabel = "flush"
		v.FlushRates = progress.WriteRates(*snap, drains[0].Landing, now)
	case len(drains) == 0:
		// All-direct so far: no drain backlog to itemize, but the landing lanes are
		// writing — surface each metered lane's rates (named when there are several).
		landings := snap.Landings()
		var cells []string
		for _, l := range landings {
			if snap.WrittenTo(l) == 0 && !snap.WriteActive(l) {
				continue
			}
			cell := progress.WriteRates(*snap, l, now)
			if cell == "" {
				continue
			}
			if len(landings) > 1 && l != "" {
				cell = l + " " + cell
			}
			cells = append(cells, cell)
		}
		if len(cells) > 0 {
			v.FlushLabel = "writing"
			v.FlushRates = strings.Join(cells, " — ")
		}
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
	if len(drains) > 1 {
		for _, ld := range drains {
			v.FlushLanes = append(v.FlushLanes, flushLaneView{
				Landing: ld.Landing, Done: ld.Done, Total: ld.Total, Pct: pctOf(ld.Done, ld.Total),
				Rates: progress.WriteRates(*snap, ld.Landing, now),
			})
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
			v.ActiveDLEs = append(v.ActiveDLEs, newActiveDLE(d, now, snap.DLERateNow(d.Name, now), mixed))
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
	Grid     *placementGrid  // archives × placements drill-down; nil when no copy exists
	Dump     *dumpReportView // the run's dump-report section, or nil when the history holds no record for it
}

// dumpReportView is the /runs/<id> mirror of `nb report --dump`: the headline, the
// STATISTICS grid (Total/Full/Incr), and the per-DLE table. It reproduces
// report.RenderDump's arithmetic (report.headline/renderStats/renderDumpTable are
// unexported) so the template does none of it.
type dumpReportView struct {
	Headline string
	Warnings []string // the run's degradations (e.g. a tripped landing), each with its repair
	Grid     []dumpGridRow
	Rows     []pathGroupRow[dumpStatRow] // per-DLE rows, path-arranged like the CLI dump table
	// Promoted summarizes the run's promoted fulls ("N full(s), X pulled forward
	// to level the cycle"), empty when none — mirrors report.renderPromotions;
	// each promoted row carries its own why.
	Promoted string
	Flushed  bool // any DLE drained via a holding disk — show the flush columns
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
	Promoted   bool   // a full pulled forward by promotion
	Why        string // the promotion's reason, for the row's tooltip
	// FlushTime/FlushRate are the DLE's drain copy time and its compressed rate over
	// it (Amanda's per-DLE taper stats); dashes for a direct dump. Rendered only when
	// the view's Flushed says any DLE drained.
	FlushTime, FlushRate string
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
		Warnings: r.Warnings,
		Grid:     dumpStatsGrid(tot, full, incr, r.LandingStats, r.EndedAt.Sub(r.StartedAt)),
	}
	var promoted int
	var promotedBytes int64
	rows := make([]dumpStatRow, 0, len(r.DumpStats))
	items := make([]dletree.Item, 0, len(r.DumpStats))
	for _, d := range r.DumpStats {
		if d.Promoted {
			promoted++
			promotedBytes += d.Out
		}
		if d.FlushSeconds > 0 {
			v.Flushed = true
		}
		rows = append(rows, dumpStatRow{
			ID: d.ID(), Slug: d.DLE, Level: d.Level,
			Orig: sizeutil.FormatBytes(d.Orig), Out: sizeutil.FormatBytes(d.Out),
			Comp: dumpCompPct(d.Orig, d.Out), Files: d.Files,
			Time: dumpTimeCell(d.Seconds), Rate: dumpRateCell(d.Orig, d.Seconds),
			Promoted: d.Promoted, Why: report.PromotionWhy(d.Reason),
			FlushTime: dumpTimeCell(d.FlushSeconds), FlushRate: dumpRateCell(d.FlushBytes, d.FlushSeconds),
		})
		items = append(items, dumpStatItem(d))
	}
	v.Rows = groupRowsByPath(rows, items)
	if promoted > 0 {
		v.Promoted = fmt.Sprintf("%d full(s), %s pulled forward to level the cycle",
			promoted, sizeutil.FormatBytes(promotedBytes))
	}
	return v
}

// dumpStatItem is a dump-stat row's grouping identity; a record without host/path
// (a bare-slug fallback) stays flat.
func dumpStatItem(d report.DLEStat) dletree.Item {
	if d.Host == "" && d.Path == "" {
		return dletree.Item{Path: d.DLE}
	}
	return dletree.Item{Host: d.Host, Path: d.Path, Rest: d.Rest}
}

// dumpHeadline is the one-line "did it work" summary — mirrors report.headline.
func dumpHeadline(r report.Run, tot dumpAgg) string {
	sizes := fmt.Sprintf("%s -> %s (%s)", sizeutil.FormatBytes(tot.orig), sizeutil.FormatBytes(tot.out), dumpCompPct(tot.orig, tot.out))
	elapsed := sizeutil.FormatElapsed(r.EndedAt.Sub(r.StartedAt))
	if r.Failed() {
		return fmt.Sprintf("%d DLE(s) dumped, run FAILED [%s] · %s · %s elapsed", tot.n, r.ExitClass, sizes, elapsed)
	}
	if r.Warned() {
		return fmt.Sprintf("%d DLE(s) dumped, %d WARNING(s) · %s · %s elapsed", tot.n, len(r.Warnings), sizes, elapsed)
	}
	return fmt.Sprintf("%d DLE(s) dumped OK · %s · %s elapsed", tot.n, sizes, elapsed)
}

// dumpStatsGrid builds the STATISTICS grid rows — mirrors report.renderStats,
// including its dash rules for an empty column and the per-landing write pair
// (busy time with utilization, rate over busy time).
func dumpStatsGrid(tot, full, incr dumpAgg, landings []report.LandingStat, wall time.Duration) []dumpGridRow {
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
	for _, ls := range landings {
		name := ls.Landing
		if name == "" {
			name = "landing"
		}
		rows = append(rows,
			dumpGridRow{"Write time (" + name + ")", writeTimeCell(ls), "", ""},
			dumpGridRow{"Avg write rate (" + name + ")", writeRateCell(ls), "", ""})
	}
	return rows
}

// writeTimeCell renders a landing's busy time with its share of the run's wall
// clock — mirrors report.writeTimeCell.
func writeTimeCell(ls report.LandingStat) string {
	if ls.BusySeconds <= 0 {
		return "-"
	}
	cell := sizeutil.FormatElapsed(time.Duration(ls.BusySeconds * float64(time.Second)))
	if ls.WallSeconds > 0 {
		cell += fmt.Sprintf(" (%.0f%% busy)", ls.BusySeconds/ls.WallSeconds*100)
	}
	return cell
}

// writeRateCell renders a landing's throughput over its busy time — mirrors
// report.writeRateCell.
func writeRateCell(ls report.LandingStat) string {
	if ls.BusySeconds <= 0 || ls.Bytes <= 0 {
		return "-"
	}
	return sizeutil.FormatBytes(int64(float64(ls.Bytes)/ls.BusySeconds)) + "/s"
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

// copyRow is one medium's copy of a run judged against the config's expectation
// (engine.RunCoverage): what the copy holds, what its landing route owes it
// (Routed), and what sync rules promise it (Promised). A row can be synthetic —
// Placed false — for an expected medium with no copy at all (a lane that tripped
// before writing anything, or a landing/sync target added since the run).
type copyRow struct {
	RunID    string
	Medium   string
	Labels   string
	Placed   bool
	SyncFrom string // the promising sync rule's source ("" = resolved per run / none)
	engine.CopyJudgment
}

// State buckets the row for its pill: "partial" (missing archives its landing
// route owes — the real defect), "behind" (only sync lag), "complete" (every
// expectation held), "aged" (the run has rotated out of this medium's retention
// window; what is still held is history, what is gone was prunable), or "extra"
// (nothing ever expected this copy; it is a bonus).
func (c copyRow) State() string {
	switch {
	case c.MissingRouted() > 0:
		return "partial"
	case c.Behind() > 0:
		return "behind"
	case c.Expected() > 0:
		return "complete"
	case c.Aged > 0:
		return "aged"
	default:
		return "extra"
	}
}

// PillClass maps the state to the shared pill palette: only a routed gap is a
// warning — sync lag and bonus copies stay quiet.
func (c copyRow) PillClass() string {
	switch c.State() {
	case "partial":
		return "warn"
	case "complete":
		return "ok"
	default:
		return "dim"
	}
}

// CovText is the coverage fraction over what the medium is expected to hold; a
// bonus copy has no denominator and shows its plain archive count.
func (c copyRow) CovText() string {
	if c.Expected() > 0 {
		return fmt.Sprintf("%d/%d", c.ExpectedHeld(), c.Expected())
	}
	return fmt.Sprintf("%d", c.Held)
}

// PillText is the coverage pill: the state word plus the fraction, with the
// no-copy-at-all case named outright.
func (c copyRow) PillText() string {
	state := c.State()
	if state == "partial" && !c.Placed {
		state = "missing"
	}
	return state + " · " + c.CovText()
}

// placementGrid is the /runs/<id> archives × placements matrix: one row per archive
// of the run, one column per placement (including expected-but-absent media), each
// cell the archive's own position on that copy — the drill-down that answers "this
// DLE, this run: where exactly", and makes a partial copy's holes visible at a
// glance, colored by whether each hole is owed (routed), lagging (promised), or
// simply not that medium's to hold.
type placementGrid struct {
	Cols []copyRow                    // column headers: the same coverage rows the Copies table shows
	Rows []pathGroupRow[placementRow] // one per archive, path-arranged like the dump table
}

// placementRow is one archive's row of the placement grid.
type placementRow struct {
	DLE   string // slug, for the /dles link
	DLEID string // host:path display
	Level int
	Cells []placementCell // index-aligned with placementGrid.Cols
}

// placementCell is one archive × placement cell.
type placementCell struct {
	Held bool
	Pos  string // the archive's volume:file positions; "" for a label-less medium (render ✓)
	Gap  string // when !Held: "miss" (routed here, a defect), "lag" (awaiting sync), "" (not expected)
}

// archivePosText renders a placed archive's part positions compactly: consecutive
// file numbers on one volume collapse to a range ("NB-0007:3–5"), a span onto a
// later volume appends its own group ("+NB-0008:1"). Label-less media (disk/cloud)
// return "" — files there are addressed within the medium, positions mean nothing
// to an operator.
func archivePosText(pa catalog.PlacedArchive) string {
	type group struct {
		label string
		runs  [][2]int // consecutive [first,last] position runs
	}
	var groups []group
	for _, pt := range pa.Parts {
		if pt.Label == "" {
			return ""
		}
		if n := len(groups); n > 0 && groups[n-1].label == pt.Label {
			g := &groups[n-1]
			if last := &g.runs[len(g.runs)-1]; pt.Pos == last[1]+1 {
				last[1] = pt.Pos
			} else {
				g.runs = append(g.runs, [2]int{pt.Pos, pt.Pos})
			}
			continue
		}
		groups = append(groups, group{label: pt.Label, runs: [][2]int{{pt.Pos, pt.Pos}}})
	}
	var b strings.Builder
	for i, g := range groups {
		if i > 0 {
			b.WriteString(" +")
		}
		b.WriteString(g.label)
		b.WriteByte(':')
		for j, r := range g.runs {
			if j > 0 {
				b.WriteByte(',')
			}
			if r[0] == r[1] {
				fmt.Fprintf(&b, "%d", r[0])
			} else {
				fmt.Fprintf(&b, "%d–%d", r[0], r[1])
			}
		}
	}
	return b.String()
}

// dleRow is one line of the DLEs list — the DLE-major catalog rollup (catalog.DLESummary
// flattened for the template, with Media pre-joined). Rows are arranged by path
// (dletree): a partitioned source's DLEs render as one group — a header row
// (Header, with member count and aggregate cells) followed by member rows whose
// Label is base-relative — so long absolute paths never force the table to
// scroll and a source's fifty children read as one entry.
type dleRow struct {
	Slug      string
	Display   string // full host:path identity (member rows link-title it)
	Label     string // what the DLE cell shows: Display, a member's relative path, or a group header's host:base
	Twig      string // tree glyph before a member label ("├─"/"└─")
	Group     string // the group this row belongs to (its header's ID); "" for flat rows
	Header    bool   // a group header row: Label/Count plus aggregate Runs/LastFull/Bytes/Media
	Count     int    // header: member count
	RestNote  bool   // a flat partition-remainder row (position no longer says it, so a note must)
	Runs      int
	LastLevel int
	LastFull  string
	Bytes     int64
	Media     string
	// The Trend/Δ cells: the DLE's windowed full sizes as a sparkline (a header
	// row carries its members' summed series) with the percent change beside it,
	// in warn past the growth threshold. Empty with fewer than two windowed fulls.
	Spark     template.HTML
	Delta     string
	DeltaWarn bool
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

// arrangeDLEs turns a set of DLE summaries into path-arranged table and heatmap
// rows (dletree): flat rows for unrelated DLEs, and for each path group a header
// row — member count plus aggregate runs/newest-full/bytes/media — followed by
// its members under short base-relative labels, "(the rest)" for a partition's
// remainder. The heatmap gets the same arrangement (header rows carry no cells).
// hideHost drops the "host:" prefix from labels — set when the rows render
// inside a host section, whose header already names the host.
// trends carries every DLE's dump history (report.DLETrends) for the Trend/Δ
// cells; a header row sums its members' series so a partitioned source reads as
// one curve.
func arrangeDLEs(sums []catalog.DLESummary, heatBySlug map[string]heatRow, hideHost bool, trends map[string][]report.TrendPoint) ([]dleRow, []heatRow) {
	items := make([]dletree.Item, len(sums))
	for i, s := range sums {
		items[i], _ = dletree.Split(s.Display)
		items[i].Rest = s.Rest
	}
	// Inside a host section the section header already names the host, so labels
	// drop the "host:" prefix rather than repeat it on every row; a hostless
	// item (bare-slug fallback) has nothing to drop.
	flatLabel := func(i int, s catalog.DLESummary) string {
		if hideHost && items[i].Host != "" {
			return items[i].Path
		}
		return s.Display
	}
	headerLabel := func(g dletree.Group) string {
		if hideHost {
			return g.Base
		}
		return g.ID()
	}
	dataRow := func(s catalog.DLESummary) dleRow {
		r := dleRow{
			Slug: s.DLE, Display: s.Display, Runs: s.Runs,
			LastLevel: s.LastLevel, LastFull: s.LastFull, Bytes: s.Bytes,
			Media: strings.Join(s.Media, ", "),
		}
		r.Spark, r.Delta, r.DeltaWarn = sparkline(windowedFullOuts(trends[s.DLE]))
		return r
	}
	var rows []dleRow
	var hrows []heatRow
	for _, g := range dletree.Build(items) {
		if g.Children == nil {
			s := sums[g.Index]
			r := dataRow(s)
			r.Label = flatLabel(g.Index, s)
			r.RestNote = s.Rest
			rows = append(rows, r)
			if hr, ok := heatBySlug[s.DLE]; ok {
				hr.Label = flatLabel(g.Index, s)
				hrows = append(hrows, hr)
			}
			continue
		}
		hdr := dleRow{Header: true, Group: g.ID(), Label: headerLabel(g), Count: len(g.Children)}
		media := map[string]bool{}
		memberTrends := make([][]report.TrendPoint, 0, len(g.Children))
		for _, c := range g.Children {
			s := sums[c.Index]
			hdr.Runs += s.Runs
			hdr.Bytes += s.Bytes
			if s.LastFull > hdr.LastFull { // ISO dates: lexicographic max = newest
				hdr.LastFull = s.LastFull
			}
			for _, m := range s.Media {
				media[m] = true
			}
			memberTrends = append(memberTrends, trends[s.DLE])
		}
		hdr.Media = strings.Join(sortedKeys(media), ", ")
		hdr.Spark, hdr.Delta, hdr.DeltaWarn = sparkline(summedFullOuts(memberTrends))
		rows = append(rows, hdr)
		hrows = append(hrows, heatRow{Header: true, Group: g.ID(),
			Label: fmt.Sprintf("%s · %d DLEs", headerLabel(g), len(g.Children))})
		for i, c := range g.Children {
			s := sums[c.Index]
			r := dataRow(s)
			r.Label, r.Twig, r.Group = g.Label(c), dletree.Branch(i, len(g.Children)), g.ID()
			rows = append(rows, r)
			if hr, ok := heatBySlug[s.DLE]; ok {
				hr.Label, hr.Group = g.Label(c), g.ID()
				hrows = append(hrows, hr)
			}
		}
	}
	return rows, hrows
}

// pathGroupRow wraps one row of a per-DLE table with its path-arranged
// presentation (the same fold /dles uses): a group header row (Header, label +
// member count, no payload), a member row (Twig + base-relative Label), or a
// flat row (full identity as Label). Group is the collapse key shared by a
// header and its members.
type pathGroupRow[T any] struct {
	Header bool
	Label  string
	Twig   string
	Group  string
	Count  int
	Bad    int // members in a failing state — badged on the header (and forces the group open), so collapse cannot hide a failure
	Warn   int // members in a warning state (e.g. stale), badged on the header
	Row    T
}

// flagGroupHeaders rolls member health up onto each group header row: judge
// returns (bad, warn) per member and the header accumulates the counts. Large
// groups start collapsed, hiding member rows — without this rollup a failing DLE
// inside one would be invisible on the page.
func flagGroupHeaders[T any](rows []pathGroupRow[T], judge func(T) (bad, warn bool)) {
	hdr := -1
	for i := range rows {
		if rows[i].Header {
			hdr = i
			continue
		}
		if hdr < 0 || rows[i].Group == "" {
			continue
		}
		switch bad, warn := judge(rows[i].Row); {
		case bad:
			rows[hdr].Bad++
		case warn:
			rows[hdr].Warn++
		}
	}
}

// groupRowsByPath arranges per-DLE rows with dletree; items carries each row's
// identity, index-aligned with rows. Used by every per-DLE table outside /dles
// (run dump report, placement grid, live sizing, drill ledger), so a partitioned
// source folds the same way on every page.
func groupRowsByPath[T any](rows []T, items []dletree.Item) []pathGroupRow[T] {
	var out []pathGroupRow[T]
	for _, g := range dletree.Build(items) {
		if g.Children == nil {
			it := items[g.Index]
			label := it.Path
			if it.Host != "" {
				label = it.Host + ":" + it.Path
			}
			out = append(out, pathGroupRow[T]{Label: label, Row: rows[g.Index]})
			continue
		}
		out = append(out, pathGroupRow[T]{Header: true, Label: g.ID(), Group: g.ID(), Count: len(g.Children)})
		for i, c := range g.Children {
			out = append(out, pathGroupRow[T]{
				Label: g.Label(c), Twig: dletree.Branch(i, len(g.Children)),
				Group: g.ID(), Row: rows[c.Index],
			})
		}
	}
	return out
}

// sortedKeys returns a map's keys sorted — media unions for group header rows.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// groupDLEs builds the /dles page data from the catalog's DLE summaries, the
// currently-stale ones (for each host section's "K stale" count), and the
// pre-built heatmap. Rows are always path-arranged (arrangeDLEs); on top of
// that, whenever the summaries span more than one host they are sectioned into
// per-host groups (Groups/HeatGroups) — hosts are the failure domain. With at
// most one host the flat Rows/Heat render, exactly as before grouping existed.
func groupDLEs(sums []catalog.DLESummary, stale []catalog.StaleDLE, heat *heatmap, trends map[string][]report.TrendPoint) dlesData {
	heatBySlug := map[string]heatRow{}
	if heat != nil {
		for _, r := range heat.Rows {
			heatBySlug[r.Slug] = r
		}
	}

	data := dlesData{Heat: heat}
	order, byHost, hostless := partitionByHost(sums)
	var hrows []heatRow
	data.Rows, hrows = arrangeDLEs(sums, heatBySlug, false, trends)
	if len(order) <= 1 {
		if heat != nil {
			heat.Rows = hrows
		}
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

	build := func(members []catalog.DLESummary, host string) (dleGroup, heatGroup) {
		hs := dleHostSummary{Host: host, Count: len(members), Stale: staleByHost[host]}
		gr, hg := dleGroup{Host: hs}, heatGroup{Host: hs}
		var newest time.Time
		for _, m := range members {
			if m.LastBackupAt.After(newest) {
				newest = m.LastBackupAt
			}
		}
		gr.Rows, hg.Rows = arrangeDLEs(members, heatBySlug, true, trends)
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

// dleDetail backs the single-DLE page: the rollup, the size-evolution charts, the
// recovery points, and this DLE's per-run history.
type dleDetail struct {
	NotFound  bool
	Slug      string
	Display   string
	Runs      int
	Bytes     int64
	Media     string
	Footprint string          // schedule-aware projected retained storage at the horizon, e.g. "~1.2 GB"; "" when not forecast
	FootDays  int             // the forecast horizon in days (for the label)
	FootRise  bool            // the footprint is projected to grow from now (▲ styling)
	Evolution dleEvolution    // the size-evolution block; zero values omit their piece
	Recovery  []recoveryPoint // restorable points, newest first (capped unless ?all=1)
	RecTotal  int             // recovery points in total (for the show-all toggle)
	RecAll    bool            // true when ?all=1 lifted the cap
	Places    []string        // media holding any archive of the DLE — the history grid's cell columns
	History   []dleArchiveRow
	Physical  *physicalView // the physical panel: the ?run= archive, or the newest restore chain
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

// chainCopy is one copy of a chain member: its medium plus the volume labels THAT
// archive occupies (archive-granular — not the run copy's whole label set).
type chainCopy struct {
	Medium string
	Labels []string // empty for address-identified media (disk/cloud)
}

// name renders the copy as the compact "medium:label" identity the pages use.
func (c chainCopy) name() string {
	if len(c.Labels) > 0 {
		return c.Medium + ":" + strings.Join(c.Labels, "+")
	}
	return c.Medium
}

// chainMember is one archive in a recovery point's restore chain, with the copies
// currently holding it (archive-granular, so a reclaimed copy shows as unheld).
type chainMember struct {
	RunID  string
	Level  int
	Copies []chainCopy // copies holding this archive; empty = no surviving copy
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

// chainMedia answers "restore from one place": the MEDIA that hold every chain member
// (a medium counts only if it holds them all — the chain's archives sit at different
// positions, so the intersection is by medium, not by volume). A whole medium is named
// with the union of the labels the chain's archives occupy on it, in chain order —
// "tape:NB-0006+NB-0008" is exactly the set of tapes restoring this point needs. When
// no single medium holds the whole chain it falls back to a compact per-member listing
// so the split is still legible.
func chainMedia(members []chainMember) (onePlace bool, text string) {
	if len(members) == 0 {
		return false, "—"
	}
	count := map[string]int{}
	labels := map[string][]string{} // per medium: union of chain labels, first-seen order
	var order []string              // media in first-seen order
	for _, m := range members {
		for _, c := range m.Copies {
			if count[c.Medium] == 0 {
				order = append(order, c.Medium)
			}
			count[c.Medium]++
			for _, l := range c.Labels {
				if !slices.Contains(labels[c.Medium], l) {
					labels[c.Medium] = append(labels[c.Medium], l)
				}
			}
		}
	}
	var whole []string
	for _, medium := range order {
		if count[medium] == len(members) {
			whole = append(whole, chainCopy{Medium: medium, Labels: labels[medium]}.name())
		}
	}
	if len(whole) > 0 {
		sort.Strings(whole)
		return true, strings.Join(whole, ", ")
	}
	segs := make([]string, 0, len(members))
	for _, m := range members {
		media := "none"
		if len(m.Copies) > 0 {
			names := make([]string, len(m.Copies))
			for i, c := range m.Copies {
				names[i] = c.name()
			}
			media = strings.Join(names, ", ")
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
// number on Mondays, else ""). Future marks a projected column (past today), which the
// template dims so the forecast region reads as distinct from recorded activity.
type heatDay struct {
	Tick   string
	Future bool
}

// heatRow is one DLE's row of daily cells, in Days order. Like dleRow, rows are
// path-arranged: a group header row (Header, no cells) precedes its members,
// whose Label is base-relative and indented by the template.
type heatRow struct {
	Slug    string
	Display string
	Label   string // what the label cell shows (Display, relative path, or the header's rollup)
	Group   string // the group this row belongs to; "" for flat rows
	Header  bool   // a group header row (label only, no cells)
	Cells   []heatCell
}

// heatCell is one DLE-day: the accent class driving the fill, a tooltip, and — when a
// single run produced the day's archive — that run to link to. Ghost marks a PROJECTED
// cell (a future day from the offline forecast, not a recorded run): the template
// renders it as an outline rather than a fill so a plan can never be mistaken for a fact.
type heatCell struct {
	Class string // "full" | "incr" | "partial" | "none"
	Title string
	RunID string // set only when exactly one run produced the day's archive
	Ghost bool   // a projected future cell (no RunID; outlined, not filled)
}

// dleTrendPoint is one dump record's statistics for a single DLE, the per-DLE series
// drawn on /dles/<slug> — the DumpStats row (report.DLEStat) paired with the run's
// time, oldest first.
// dleEvolution is the "Size evolution" block of the /dles/<slug> page: fulls and
// incrementals charted separately — a dataset-size question and a churn question,
// which one shared axis would bury (GiB of churn is invisible under a hundred-GiB
// full). Each chart carries its own note line with the computed numbers, and
// Growth is the rollup-card figure. Zero values simply omit their piece.
type dleEvolution struct {
	Growth    string        // the growth card: "+736 MiB/day"; "" without enough windowed fulls
	GrowthSub string        // the card's caption, naming the window
	Fulls     template.HTML // dataset-size chart; "" with fewer than two fulls
	FullsNote template.HTML
	Incr      template.HTML // churn chart over the windowed incrementals; "" with fewer than two
	IncrNote  template.HTML
}

// newDLEEvolution builds the evolution block from a DLE's dump points
// (oldest-first). The fulls chart spans the whole recorded history — the long view
// is its point — while the growth figures and the churn chart cover the recent
// report.EvolutionWindow, matching SummarizeTrend so note and chart can't disagree.
func newDLEEvolution(pts []report.TrendPoint) dleEvolution {
	var v dleEvolution
	if len(pts) == 0 {
		return v
	}
	var fulls, incrs []report.TrendPoint
	cutoff := pts[len(pts)-1].At.Add(-report.EvolutionWindow)
	for _, p := range pts {
		if p.Level == 0 {
			fulls = append(fulls, p)
		} else if !p.At.Before(cutoff) {
			incrs = append(incrs, p)
		}
	}
	v.Fulls = fullsTrendSVG(fulls)
	if v.Fulls != "" {
		v.FullsNote = template.HTML(`<span style="color:var(--accent)">■</span> output (solid) · <span style="color:var(--muted)">◦◦</span> original (dashed)`)
	}
	ev, ok := report.SummarizeTrend(pts)
	if ok {
		v.Growth = signedBytes(ev.PerDay) + "/day"
		v.GrowthSub = fmt.Sprintf("fulls, last %d d", int(report.EvolutionWindow.Hours()/24))
		v.FullsNote += template.HTML(fmt.Sprintf(
			` · fulls <b>%s → %s</b> over %.0f d (%+d%%, %s/day)`,
			sizeutil.FormatBytes(ev.From.Out), sizeutil.FormatBytes(ev.To.Out),
			ev.Days, ev.Pct, signedBytes(ev.PerDay)))
	}
	v.Incr = incrTrendSVG(incrs)
	if v.Incr != "" {
		med := medianIncrOut(incrs)
		note := fmt.Sprintf(`<span style="color:%s">■</span> output per incremental · dotted line = recent median (%s)`,
			incrBarFill, sizeutil.FormatBytes(med))
		if spike := biggestSpike(incrs, med); spike != nil {
			note += fmt.Sprintf(` · <span class="warn">■ %s — %s, %.1f× median</span>`,
				spike.At.Format("2006-01-02"), sizeutil.FormatBytes(spike.Out), float64(spike.Out)/float64(med))
		}
		v.IncrNote = template.HTML(note)
	}
	return v
}

// signedBytes formats a byte delta with its sign — rates and deltas read wrong
// without the explicit "+".
func signedBytes(v int64) string {
	if v < 0 {
		return "−" + sizeutil.FormatBytes(-v)
	}
	return "+" + sizeutil.FormatBytes(v)
}

// medianIncrOut is the median output size of a set of incrementals (non-empty) —
// the churn chart's rule and the spike test's baseline.
func medianIncrOut(incrs []report.TrendPoint) int64 {
	outs := make([]int64, len(incrs))
	for i, p := range incrs {
		outs[i] = p.Out
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i] < outs[j] })
	return outs[len(outs)/2]
}

// isSpike says whether an incremental's size is anomalous against the window's
// median — the same blunt thresholds as the home rollup's size-anomaly nudge
// (dumpAnomalies): over 2× the baseline AND an absolute delta past the noise
// floor, so the chart and the rollup tell one story.
func isSpike(out, med int64) bool {
	return med > 0 && out > med*2 && out-med > anomalySizeFloor
}

// biggestSpike returns the largest anomalous incremental, or nil when none is.
func biggestSpike(incrs []report.TrendPoint, med int64) *report.TrendPoint {
	var spike *report.TrendPoint
	for i := range incrs {
		p := &incrs[i]
		if isSpike(p.Out, med) && (spike == nil || p.Out > spike.Out) {
			spike = p
		}
	}
	return spike
}

// fullsTrendSVG renders the dataset-size chart: the DLE's full dumps over its whole
// recorded history, output bytes as an area+line and original bytes as a dashed
// guide above it, so growth and compression stability read at a glance. Mirrors
// usageChartSVG's conventions (baseline, ≤80 hover markers, end labels, date
// labels); "" for fewer than two fulls or a zero span.
func fullsTrendSVG(fulls []report.TrendPoint) template.HTML {
	if len(fulls) < 2 {
		return ""
	}
	first, last := fulls[0].At, fulls[len(fulls)-1].At
	span := last.Sub(first)
	if span <= 0 {
		return ""
	}
	var peak int64
	for _, p := range fulls {
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
	const vw, vh = 760.0, 200.0
	const padL, padR, padT, padB = 8.0, 8.0, 14.0, 26.0
	plotW, plotH := vw-padL-padR, vh-padT-padB
	baseY := padT + plotH
	scale := int64(float64(peak) * 1.08)
	x := func(t time.Time) float64 { return padL + float64(t.Sub(first))/float64(span)*plotW }
	y := func(v int64) float64 { return padT + (1-float64(v)/float64(scale))*plotH }

	path := func(get func(report.TrendPoint) int64) string {
		var b strings.Builder
		for i, p := range fulls {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&b, "%s%.1f %.1f ", cmd, x(p.At), y(get(p)))
		}
		return strings.TrimSpace(b.String())
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" style="width:100%%;height:auto;display:block" role="img" aria-label="full dump size over time">`, vw, vh)
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--line)" stroke-width="1"/>`, padL, baseY, vw-padR, baseY)
	fmt.Fprintf(&b, `<path d="%s L%.1f %.1f L%.1f %.1f Z" fill="var(--accent)" fill-opacity="0.15"/>`,
		path(func(p report.TrendPoint) int64 { return p.Out }), x(last), baseY, x(first), baseY)
	fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="var(--muted)" stroke-width="1.5" stroke-dasharray="5 4"/>`,
		path(func(p report.TrendPoint) int64 { return p.Orig }))
	fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="var(--accent)" stroke-width="2"/>`,
		path(func(p report.TrendPoint) int64 { return p.Out }))
	if len(fulls) <= 80 {
		for _, p := range fulls {
			title := fmt.Sprintf("%s — L0 — orig %s, out %s%s", p.At.Format("2006-01-02 15:04"),
				sizeutil.FormatBytes(p.Orig), sizeutil.FormatBytes(p.Out), dumpTimeSuffix(p.Seconds))
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" fill="var(--accent)"><title>%s</title></circle>`, x(p.At), y(p.Out), title)
		}
	}
	end := fulls[len(fulls)-1]
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--fg)" font-size="11" text-anchor="end">%s</text>`, x(end.At), y(end.Out)-6, sizeutil.FormatBytes(end.Out))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11" text-anchor="end">%s orig</text>`, x(end.At), y(end.Orig)-6, sizeutil.FormatBytes(end.Orig))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11">%s</text>`, padL, vh-8, first.Format("2006-01-02"))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11" text-anchor="end">%s</text>`, vw-padR, vh-8, last.Format("2006-01-02"))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// incrBarFill is the churn chart's bar color — the same faded accent the heatmap
// and volume maps already use for "incremental".
const incrBarFill = "color-mix(in srgb, var(--accent) 45%, var(--panel))"

// incrTrendSVG renders the churn chart: each windowed incremental as a bar on its
// own scale, a dotted rule at the window's median, and any anomalous dump (isSpike,
// the home rollup's thresholds) in warn with its size labeled. "" for fewer than
// two incrementals or a zero span.
func incrTrendSVG(incrs []report.TrendPoint) template.HTML {
	if len(incrs) < 2 {
		return ""
	}
	first, last := incrs[0].At, incrs[len(incrs)-1].At
	span := last.Sub(first)
	if span <= 0 {
		return ""
	}
	var peak int64
	for _, p := range incrs {
		if p.Out > peak {
			peak = p.Out
		}
	}
	if peak <= 0 {
		return ""
	}
	med := medianIncrOut(incrs)
	const vw, vh = 760.0, 130.0
	const padL, padR, padT, padB = 8.0, 8.0, 10.0, 24.0
	plotW, plotH := vw-padL-padR, vh-padT-padB
	baseY := padT + plotH
	scale := int64(float64(peak) * 1.12)
	x := func(t time.Time) float64 { return padL + float64(t.Sub(first))/float64(span)*plotW }
	y := func(v int64) float64 { return padT + (1-float64(v)/float64(scale))*plotH }
	// One bar per dump, sized to the cadence so daily dumps tile the width with a
	// 2px gap; a sparse series keeps a readable minimum.
	bw := plotW/(span.Hours()/24) - 2
	if bw < 3 {
		bw = 3
	}
	if bw > 24 {
		bw = 24
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" style="width:100%%;height:auto;display:block" role="img" aria-label="incremental dump size over time">`, vw, vh)
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--line)" stroke-width="1"/>`, padL, baseY, vw-padR, baseY)
	for _, p := range incrs {
		fill := incrBarFill
		suffix := ""
		if isSpike(p.Out, med) {
			fill = "var(--warn)"
			suffix = fmt.Sprintf(" — %.1f× recent median", float64(p.Out)/float64(med))
		}
		title := fmt.Sprintf("%s — %s — out %s%s%s", p.At.Format("2006-01-02 15:04"), levelTag(p.Level),
			sizeutil.FormatBytes(p.Out), dumpTimeSuffix(p.Seconds), suffix)
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="1.5" fill="%s"><title>%s</title></rect>`,
			x(p.At)-bw/2, y(p.Out), bw, baseY-y(p.Out), fill, title)
	}
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--muted)" stroke-width="1" stroke-dasharray="2 4"/>`,
		padL, y(med), vw-padR, y(med))
	if spike := biggestSpike(incrs, med); spike != nil {
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--warn)" font-size="11" text-anchor="end">%s</text>`,
			x(spike.At)-bw/2-4, y(spike.Out)+4, sizeutil.FormatBytes(spike.Out))
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11">%s</text>`, padL, vh-8, first.Format("2006-01-02"))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11" text-anchor="end">%s</text>`, vw-padR, vh-8, last.Format("2006-01-02"))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// sparkGrowthWarnPct is the /dles Δ column's attention threshold: a DLE whose
// windowed fulls grew at least this much renders its delta (and sparkline) in
// warn. Growth only — the column's question is "which DLE is growing".
const sparkGrowthWarnPct = 20

// sparkline summarizes a DLE's windowed full sizes for the /dles Trend/Δ cells:
// the tiny chart, the formatted percent change, and whether it crossed the warn
// threshold. Fewer than two fulls yield an empty cell — no fake flatline.
func sparkline(outs []int64) (svg template.HTML, delta string, warn bool) {
	if len(outs) < 2 {
		return "", "", false
	}
	pct := 0
	if outs[0] > 0 {
		pct = int(float64(outs[len(outs)-1]-outs[0]) / float64(outs[0]) * 100)
	}
	switch {
	case pct > 0:
		delta = fmt.Sprintf("+%d%%", pct)
	case pct < 0:
		delta = fmt.Sprintf("−%d%%", -pct)
	default:
		delta = "±0%"
	}
	warn = pct >= sparkGrowthWarnPct
	return sparkSVG(outs, warn), delta, warn
}

// sparkSVG draws the Trend-cell line: 120×26, evenly spaced points. The y-range is
// floored at 15% of the series mean so a flat DLE draws flat instead of
// min-max-stretching its noise into a false zigzag; an end dot marks "now".
func sparkSVG(vals []int64, warn bool) template.HTML {
	const w, h, pad = 120.0, 26.0, 3.0
	lo, hi := vals[0], vals[0]
	var sum float64
	for _, v := range vals {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
		sum += float64(v)
	}
	span := float64(hi - lo)
	if floor := sum / float64(len(vals)) * 0.15; span < floor {
		span = floor
	}
	if span <= 0 {
		span = 1
	}
	mid := float64(hi+lo) / 2
	x := func(i int) float64 { return pad + float64(i)/float64(len(vals)-1)*(w-2*pad) }
	y := func(v int64) float64 { return pad + (1-((float64(v)-mid)/span+0.5))*(h-2*pad) }
	stroke := "var(--accent)"
	if warn {
		stroke = "var(--warn)"
	}
	var d strings.Builder
	for i, v := range vals {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&d, "%s%.1f %.1f ", cmd, x(i), y(v))
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" role="img" aria-label="full size trend" style="vertical-align:middle">`, w, h, w, h)
	fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="1.5"/>`, strings.TrimSpace(d.String()), stroke)
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2.5" fill="%s"/>`, x(len(vals)-1), y(vals[len(vals)-1]), stroke)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// windowedFullOuts extracts a DLE's spark series from its trend points: the output
// sizes of its fulls inside the evolution window (anchored, like SummarizeTrend, at
// the DLE's own newest point).
func windowedFullOuts(pts []report.TrendPoint) []int64 {
	if len(pts) == 0 {
		return nil
	}
	cutoff := pts[len(pts)-1].At.Add(-report.EvolutionWindow)
	var outs []int64
	for _, p := range pts {
		if p.Level == 0 && !p.At.Before(cutoff) {
			outs = append(outs, p.Out)
		}
	}
	return outs
}

// summedFullOuts merges several DLEs' trend points into one group series for a
// path-group header's sparkline: at each member's full, the sum of every member's
// last-known full size (carry-forward; a member with no full yet contributes 0).
// It answers "how big is the group's dataset as known at time t", so a partitioned
// source reads as one curve.
func summedFullOuts(members [][]report.TrendPoint) []int64 {
	type event struct {
		at     time.Time
		member int
		out    int64
	}
	var events []event
	var newest time.Time
	for m, pts := range members {
		for _, p := range pts {
			if p.Level == 0 {
				events = append(events, event{p.At, m, p.Out})
				if p.At.After(newest) {
					newest = p.At
				}
			}
		}
	}
	cutoff := newest.Add(-report.EvolutionWindow)
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	last := make(map[int]int64, len(members))
	var series []int64
	for i := 0; i < len(events); {
		// One sample per timestamp: a run records its members' dumps at the same
		// instant, and a per-event sum would count the first member's full before
		// its siblings' land — fake growth on the group's first sample.
		at := events[i].at
		for ; i < len(events) && events[i].at.Equal(at); i++ {
			last[events[i].member] = events[i].out
		}
		if at.Before(cutoff) {
			continue // pre-window fulls only seed the carry-forward state
		}
		var sum int64
		for _, v := range last {
			sum += v
		}
		series = append(series, sum)
	}
	return series
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
	UtilPct   float64 // 0 when unbounded
	Over      bool    // used past a bounded capacity (sync/copy can land runs over)
	ProjFull  string  // byte media: "~Nd"/"clear"/"—"; tape pool: "run out ~Nd"/"~M/N carts"
	ProjSched bool    // ProjFull is schedule-aware (forecast) rather than naive-linear growth
	ProjOver  bool    // the projection is a warning (fills / runs out of tapes) — for styling
	Pool      bool    // this medium's pool holds one or more labeled volumes
	PoolRoom  string  // "K of N with room" — set only when Pool
}

// newMediaRows merges the medium summaries with each one's growth into the /media
// list's view rows. It prefers the SCHEDULE-AWARE fill forecast (fills, from the offline
// simulation — accounts for the dump cycle, projected growth, and pruning) for the
// "projected full" date, falling back to the naive linear projection from recorded usage
// (MediumStats.Growth) for a medium the forecast doesn't cover. stats/fills are lookups
// rather than the whole Source, so this stays a pure view function callable from a test.
func newMediaRows(media []engine.MediumInfo, stats func(string) (engine.MediumStats, bool), forecasts []engine.MediumForecast, now time.Time) []mediaRow {
	fc := map[string]engine.MediumForecast{}
	for _, mf := range forecasts {
		fc[mf.Medium] = mf
	}
	rows := make([]mediaRow, 0, len(media))
	for _, m := range media {
		row := mediaRow{MediumInfo: m, ProjFull: "—"}
		if m.Capacity > 0 {
			row.UtilPct = float64(m.Used) / float64(m.Capacity) * 100
			row.Over = m.Used > m.Capacity
		}
		st, ok := stats(m.Name)
		switch {
		case m.Volumes > 0: // tape pool — projected in CARTRIDGES, not bytes
			row.Pool = true
			withRoom, total := poolRoomCount(st.PerVolume, st.PoolVolumes)
			row.PoolRoom = fmt.Sprintf("%d of %d with room", withRoom, total)
			if mf, has := fc[m.Name]; has && len(mf.Volumes) > 0 {
				row.ProjSched = true
				if over, need := mf.VolumeOver(); over != "" {
					if t, err := time.Parse("2006-01-02", over); err == nil {
						row.ProjFull = fmt.Sprintf("out ~%dd (%d>%d)", projDays(t, now), need, mf.VolumeCeiling)
						row.ProjOver = true
					}
				} else {
					row.ProjFull = "within slots"
				}
			}
		case len(fc[m.Name].Points) > 0:
			// Schedule-aware: the first day the medium can't fit its retained set under
			// capacity even after pruning. No such day means it stays within the horizon.
			row.ProjSched = true
			if over := firstOverCapacityDate(fc[m.Name].Points); !over.IsZero() {
				row.ProjFull = fmt.Sprintf("~%dd", projDays(over, now))
				row.ProjOver = true
			} else {
				row.ProjFull = "clear" // stays within capacity across the forecast horizon
			}
		case ok && !st.Growth.ProjFull.IsZero():
			row.ProjFull = fmt.Sprintf("~%dd", projDays(st.Growth.ProjFull, now))
		}
		rows = append(rows, row)
	}
	return rows
}

// firstOverCapacityDate returns the first day a medium's fill forecast cannot fit its
// retained set under capacity, or the zero time if it stays within it.
func firstOverCapacityDate(points []engine.ForecastPoint) time.Time {
	for _, p := range points {
		if p.OverCapacity() {
			if t, err := time.Parse("2006-01-02", p.Date); err == nil {
				return t
			}
		}
	}
	return time.Time{}
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

// weeksLabel renders a restore-depth in weeks: whole weeks once past a couple, a decimal
// below (where "1.5 weeks" carries more than a rounded "2").
func weeksLabel(w float64) string {
	if w >= 2 {
		return fmt.Sprintf("%.0f weeks", w)
	}
	return fmt.Sprintf("%.1f weeks", w)
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
	VolMap   *volMap       // the pool's volumes with everything stored on them; nil for address-identified media
	Syncs    []syncLagView // sync rules targeting this medium, each with its live backlog

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

	// CapacityOutlook is the SCHEDULE-AWARE forecast headline (dump cycle + projected
	// growth + pruning), distinct from the naive-linear ProjFull above: a dated warning
	// when the medium is projected to outgrow capacity, or a reassurance that it fits
	// across the horizon. "" when the medium isn't covered (unbounded, tape, or no route).
	CapacityOutlook string
	CapacityOver    bool // the outlook is a warning, for styling

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

// syncLagView is the medium page's quiet "sync target" line: this medium is a
// sync rule's target, this is how far behind it runs. Lag, not error — it renders
// muted, never as an alert.
type syncLagView struct {
	From  string // the rule's source ("" = resolved per run)
	Runs  int    // runs the target has not fully mirrored yet (0 = in sync)
	Bytes string // formatted backlog size
}

// newMediumData flattens a medium's usage picture (engine.MediumStats: the retained
// composition, the catalog's recorded usage ledger, and the growth statistics over
// it) into the view model — display percentages plus the used-over-time chart drawn
// from the ledger samples, which show the prune/relabel declines the retained
// picture cannot.
// newMediumData flattens a medium's stats into the detail view. mf (zero if none) is the
// schedule-aware forecast for this medium: a byte medium overlays it on the used-capacity
// chart; a tape pool draws a cartridges-in-use chart from it instead (the byte usage chart
// is structurally misleading for a rotating pool).
func newMediumData(st engine.MediumStats, mf engine.MediumForecast, now time.Time) mediumData {
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
	if mf.VolumeStructured {
		d.Chart = volumesChartSVG(mf.Volumes, mf.VolumeCeiling, now)
	} else {
		// The protected floor (minimum capacity) is one line across the reconstructed
		// history and the projection's per-day floor.
		protected := append([]engine.ForecastPoint(nil), mf.History...)
		for _, p := range mf.Points {
			protected = append(protected, engine.ForecastPoint{Date: p.Date, Bytes: p.Protected})
		}
		d.Chart = usageChartSVG(st.Usage, mf.Points, protected, mf.Depth.Marks, now, st.Capacity, capLabel)
	}
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

// usageChartSVG renders a medium's used-capacity over time as a self-contained inline
// SVG: no external assets (the strict artifact-style CSP the webui keeps), colors driven
// by the page's CSS variables so it tracks light/dark, geometry computed here so the
// template stays declarative. The SOLID filled curve is recorded HISTORY (the catalog's
// usage ledger, so a prune shows as it falling); the DASHED curve past the "now" divider
// is the schedule-aware PROJECTION (forecast, if any) — the same fill forecast the
// /media column reads, drawn on the same axes and through the capacity ceiling, with the
// crossing point marked. It returns "" when there is neither ≥2 history samples nor a
// projection to draw, or a zero time span. Coordinates are safe (numbers + datestamps),
// so template.HTML is sound. capLabel names the dashed ceiling line ("capacity", or
// "pool capacity" for a labeled pool).
func usageChartSVG(series []catalog.UsageSample, forecast, protected []engine.ForecastPoint, depth []engine.DepthMark, now time.Time, capacity int64, capLabel string) template.HTML {
	// Parse the projection dates once (skip anything unparseable rather than fail).
	type pt struct {
		t time.Time
		v int64
	}
	parse := func(ps []engine.ForecastPoint) []pt {
		out := make([]pt, 0, len(ps))
		for _, p := range ps {
			if t, err := time.Parse("2006-01-02", p.Date); err == nil {
				out = append(out, pt{t, p.Bytes})
			}
		}
		return out
	}
	fc := parse(forecast)
	prot := parse(protected) // the retention-floor (minimum-capacity) line across history + projection
	hasHist := len(series) >= 2
	hasProj := len(fc) >= 2
	if !hasHist && !hasProj {
		return "" // nothing worth a line
	}

	// The x-axis spans all history, all projection, and "now" (the divider); the y-axis
	// covers the taller of capacity or the highest point either series reaches.
	xMin, xMax := now, now
	var peak int64
	note := func(t time.Time, v int64) {
		if t.Before(xMin) {
			xMin = t
		}
		if t.After(xMax) {
			xMax = t
		}
		if v > peak {
			peak = v
		}
	}
	for _, s := range series {
		note(s.At, s.Used)
	}
	for _, p := range fc {
		note(p.t, p.v)
	}
	span := xMax.Sub(xMin)
	if span <= 0 {
		return ""
	}
	scale := capacity
	if peak > scale {
		scale = int64(float64(peak) * 1.08)
	}
	if scale <= 0 {
		return ""
	}

	const vw, vh = 760.0, 220.0
	const padL, padR, padT, padB = 8.0, 8.0, 12.0, 26.0
	plotW, plotH := vw-padL-padR, vh-padT-padB
	baseY := padT + plotH
	drawCap := capacity > 0 && capacity <= scale
	x := func(t time.Time) float64 { return padL + float64(t.Sub(xMin))/float64(span)*plotW }
	y := func(v int64) float64 { return padT + (1-float64(v)/float64(scale))*plotH }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" style="width:100%%;height:auto;display:block" role="img" aria-label="used capacity over time, with projection">`, vw, vh)
	// Baseline.
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--line)" stroke-width="1"/>`, padL, baseY, vw-padR, baseY)
	// Capacity ceiling, across the whole width so the projection's crossing is visible.
	if drawCap {
		cy := y(capacity)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--warn)" stroke-width="1" stroke-dasharray="4 3"/>`, padL, cy, vw-padR, cy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--warn)" font-size="11" text-anchor="end">%s %s</text>`, vw-padR, cy-3, capLabel, sizeutil.FormatBytes(capacity))
	}

	// History: solid filled area + line.
	if hasHist {
		var line, area strings.Builder
		for i, s := range series {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&line, "%s%.1f %.1f ", cmd, x(s.At), y(s.Used))
		}
		fmt.Fprintf(&area, "M%.1f %.1f ", x(series[0].At), baseY)
		for _, s := range series {
			fmt.Fprintf(&area, "L%.1f %.1f ", x(s.At), y(s.Used))
		}
		fmt.Fprintf(&area, "L%.1f %.1f Z", x(series[len(series)-1].At), baseY)
		fmt.Fprintf(&b, `<path d="%s" fill="var(--accent)" fill-opacity="0.15"/>`, strings.TrimSpace(area.String()))
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="var(--accent)" stroke-width="2"/>`, strings.TrimSpace(line.String()))
		if len(series) <= 80 {
			for _, s := range series {
				fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2.5" fill="var(--accent)"><title>%s — %s</title></circle>`,
					x(s.At), y(s.Used), s.At.Format("2006-01-02 15:04"), sizeutil.FormatBytes(s.Used))
			}
		}
	}

	// Restore-depth ticks on the right edge: the byte level each holds is the capacity to
	// keep restore points back that many weeks, so the ceiling's position among them reads
	// as "this capacity buys ~N weeks." Only those within the y-scale are drawn.
	for _, m := range depth {
		if m.Bytes <= 0 || m.Bytes > scale {
			continue
		}
		my := y(m.Bytes)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--muted)" stroke-width="1" stroke-dasharray="1 2" opacity="0.6"/>`, vw-padR-34, my, vw-padR, my)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="9" text-anchor="end" opacity="0.85"><title>capacity to keep %dw of restore points: %s</title>%dw</text>`, vw-padR-36, my+3, m.Weeks, sizeutil.FormatBytes(m.Bytes), m.Weeks)
	}

	// Protected floor: the retention minimum pruning can't reclaim, filled darker beneath
	// the footprint across history AND projection — the least capacity the medium needs.
	if len(prot) >= 2 {
		var area, line strings.Builder
		fmt.Fprintf(&area, "M%.1f %.1f ", x(prot[0].t), baseY)
		for i, p := range prot {
			fmt.Fprintf(&area, "L%.1f %.1f ", x(p.t), y(p.v))
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&line, "%s%.1f %.1f ", cmd, x(p.t), y(p.v))
		}
		fmt.Fprintf(&area, "L%.1f %.1f Z", x(prot[len(prot)-1].t), baseY)
		fmt.Fprintf(&b, `<path d="%s" fill="var(--accent)" fill-opacity="0.28"/>`, strings.TrimSpace(area.String()))
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="var(--accent)" stroke-width="1.5" opacity="0.7"/>`, strings.TrimSpace(line.String()))
		var peakP int64
		for _, p := range prot {
			if p.v > peakP {
				peakP = p.v
			}
		}
		e := prot[len(prot)-1]
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--accent)" font-size="10" text-anchor="end" opacity="0.85"><title>minimum capacity needed — the retention floor cannot be pruned below this</title>min %s</text>`,
			x(e.t), y(e.v)+11, sizeutil.FormatBytes(peakP))
	}

	// Projection: a dashed line (no fill, so it reads as "not yet real"), continuing from
	// the last history point (or standing alone) through the daily forecast.
	if hasProj {
		var proj strings.Builder
		fmt.Fprint(&proj, "M")
		if hasHist {
			last := series[len(series)-1]
			fmt.Fprintf(&proj, "%.1f %.1f L", x(last.At), y(last.Used))
		}
		for _, p := range fc {
			fmt.Fprintf(&proj, "%.1f %.1f ", x(p.t), y(p.v))
		}
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="var(--accent)" stroke-width="2" stroke-dasharray="5 4" opacity="0.85"/>`, strings.TrimSpace(proj.String()))
		// Mark the first day the projection pierces capacity — the dated "full" point.
		if drawCap {
			for _, p := range fc {
				if p.v > capacity {
					fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3.5" fill="var(--bad)"><title>projected over capacity — %s</title></circle>`, x(p.t), y(p.v), p.t.Format("2006-01-02"))
					fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--bad)" font-size="11" text-anchor="middle">full ~%s</text>`, x(p.t), y(p.v)-7, p.t.Format("Jan 2"))
					break
				}
			}
		}
		// Projection end value label.
		e := fc[len(fc)-1]
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11" text-anchor="end">%s</text>`, x(e.t), y(e.v)-6, sizeutil.FormatBytes(e.v))
	} else if hasHist {
		end := series[len(series)-1]
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--fg)" font-size="11" text-anchor="end">%s</text>`, x(end.At), y(end.Used)-6, sizeutil.FormatBytes(end.Used))
	}

	// "now" divider between recorded and projected — only when the projection extends
	// past it (otherwise the whole chart is history and the divider is noise).
	if hasProj && now.After(xMin) && now.Before(xMax) {
		nx := x(now)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--muted)" stroke-width="1" stroke-dasharray="2 3"/>`, nx, padT, nx, baseY)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="10" text-anchor="middle">now</text>`, nx, padT-2)
	}

	// X-axis end dates.
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11">%s</text>`, padL, vh-8, xMin.Format("2006-01-02"))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11" text-anchor="end">%s</text>`, vw-padR, vh-8, xMax.Format("2006-01-02"))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// volumesChartSVG renders a tape pool's CARTRIDGES-in-use over time — the volume-
// structured peer of usageChartSVG. Solid history (reconstructed from the catalog) runs
// to the "now" divider, then a dashed projection continues; the slot ceiling is a dashed
// line and the first day the projection needs more reels than slots is marked in red
// ("run out"). Counts are integers, so the y-axis is whole cartridges.
func volumesChartSVG(points []engine.VolumePoint, ceiling int64, now time.Time) template.HTML {
	type pt struct {
		t    time.Time
		v    int64
		proj bool
	}
	var pts []pt
	var peak int64
	for _, p := range points {
		t, err := time.Parse("2006-01-02", p.Date)
		if err != nil {
			continue
		}
		pts = append(pts, pt{t, p.InUse, t.After(now)})
		if p.InUse > peak {
			peak = p.InUse
		}
	}
	if len(pts) < 2 {
		return ""
	}
	xMin, xMax := pts[0].t, pts[len(pts)-1].t
	span := xMax.Sub(xMin)
	if span <= 0 {
		return ""
	}
	scale := ceiling
	if peak >= scale {
		scale = peak + 1 // keep the ceiling line and the peak both on-canvas
	}
	if scale <= 0 {
		return ""
	}

	const vw, vh = 760.0, 200.0
	const padL, padR, padT, padB = 8.0, 8.0, 12.0, 26.0
	plotW, plotH := vw-padL-padR, vh-padT-padB
	baseY := padT + plotH
	x := func(t time.Time) float64 { return padL + float64(t.Sub(xMin))/float64(span)*plotW }
	y := func(v int64) float64 { return padT + (1-float64(v)/float64(scale))*plotH }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" style="width:100%%;height:auto;display:block" role="img" aria-label="cartridges in use over time, with projection">`, vw, vh)
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--line)" stroke-width="1"/>`, padL, baseY, vw-padR, baseY)
	if ceiling > 0 {
		cy := y(ceiling)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--warn)" stroke-width="1" stroke-dasharray="4 3"/>`, padL, cy, vw-padR, cy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--warn)" font-size="11" text-anchor="end">%d slots</text>`, vw-padR, cy-3, ceiling)
	}
	// One polyline; the segment past "now" is dashed. Emitting per-segment keeps the
	// solid/dashed boundary crisp without two near-duplicate paths.
	for i := 1; i < len(pts); i++ {
		a, c := pts[i-1], pts[i]
		dash := ""
		if c.proj {
			dash = ` stroke-dasharray="5 4"`
		}
		fmt.Fprintf(&b, `<path d="M%.1f %.1f L%.1f %.1f" fill="none" stroke="var(--accent)" stroke-width="2"%s/>`,
			x(a.t), y(a.v), x(c.t), y(c.v), dash)
	}
	// "now" divider and the first over-slots day.
	if now.After(xMin) && now.Before(xMax) {
		nx := x(now)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="var(--muted)" stroke-width="1" stroke-dasharray="2 3"/>`, nx, padT, nx, baseY)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="10" text-anchor="middle">now</text>`, nx, padT-2)
	}
	if ceiling > 0 {
		for _, p := range pts {
			if p.proj && p.v > ceiling {
				fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3.5" fill="var(--bad)"><title>needs %d cartridges — %s</title></circle>`, x(p.t), y(p.v), p.v, p.t.Format("2006-01-02"))
				fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--bad)" font-size="11" text-anchor="middle">out ~%s</text>`, x(p.t), y(p.v)-7, p.t.Format("Jan 2"))
				break
			}
		}
	}
	end := pts[len(pts)-1]
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11" text-anchor="end">%d in use</text>`, x(end.t), y(end.v)-6, end.v)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11">%s</text>`, padL, vh-8, xMin.Format("2006-01-02"))
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--muted)" font-size="11" text-anchor="end">%s</text>`, vw-padR, vh-8, xMax.Format("2006-01-02"))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// drillsData backs the drills page: the coverage rollup, the per-DLE ledger, and
// the recent drill runs.
type drillsData struct {
	Window                  string           // formatted coverage window (e.g. "30d")
	Passing, Stale, Failing int              // ledger records by current health
	FailingRows             []drillLedgerRow // failing records, leading the page: error + remedy + retry, never buried mid-ledger
	Never                   []dleLink        // configured DLEs never drilled; path siblings folded (Slug == "")
	Overdue                 int              // DLEs not covered within the window
	Ledger                  []drillLedgerSection
	Runs                    []drillRunRow
}

// drillLedgerSection is one host's slice of the ledger table. A fleet spanning
// more than one host is sectioned per host — hosts are the failure domain, the
// same sectioning /dles uses — with the host named once (Label set) and member
// labels host-echo-free; a single-host fleet stays one unlabeled section with
// full identities.
type drillLedgerSection struct {
	Label     string // host name for the section header ("(other)" for hostless); "" = no header (single host)
	Count     int    // DLEs in the section
	Bad, Warn int    // failing / stale members, badged on the header
	Rows      []pathGroupRow[drillLedgerRow]
}

// sectionDrillLedger arranges ledger rows for display: path-folded via
// groupRowsByPath, split into per-host sections when the fleet spans more than
// one host, each section's labels stripped of the host echo and its header
// carrying failing/stale rollups (so no section can hide a failure).
func sectionDrillLedger(rows []drillLedgerRow, items []dletree.Item) []drillLedgerSection {
	judge := func(r drillLedgerRow) (bool, bool) { return r.Failing, r.Stale }
	var hosts []string
	byHost := map[string][]int{}
	for i, it := range items {
		if _, ok := byHost[it.Host]; !ok {
			hosts = append(hosts, it.Host)
		}
		byHost[it.Host] = append(byHost[it.Host], i)
	}
	if len(hosts) <= 1 {
		g := groupRowsByPath(rows, items)
		flagGroupHeaders(g, judge)
		if g == nil {
			return nil
		}
		return []drillLedgerSection{{Count: len(rows), Rows: g}}
	}
	// Hostless (bare-slug) records trail the named hosts, like /dles' "(other)".
	sort.SliceStable(hosts, func(i, j int) bool {
		if (hosts[i] == "") != (hosts[j] == "") {
			return hosts[j] == ""
		}
		return hosts[i] < hosts[j]
	})
	var out []drillLedgerSection
	for _, host := range hosts {
		sec := drillLedgerSection{Label: host, Count: len(byHost[host])}
		if host == "" {
			sec.Label = "(other)"
		}
		hr := make([]drillLedgerRow, 0, sec.Count)
		hi := make([]dletree.Item, 0, sec.Count)
		for _, i := range byHost[host] {
			hr = append(hr, rows[i])
			hi = append(hi, items[i])
			switch bad, warn := judge(rows[i]); {
			case bad:
				sec.Bad++
			case warn:
				sec.Warn++
			}
		}
		sec.Rows = groupRowsByPath(hr, hi)
		for j := range sec.Rows {
			// The section names the host; row labels carry only the path. Group
			// keys keep the host-qualified ID, so same-named directories on two
			// hosts collapse independently.
			sec.Rows[j].Label = strings.TrimPrefix(sec.Rows[j].Label, host+":")
		}
		flagGroupHeaders(sec.Rows, judge)
		out = append(out, sec)
	}
	return out
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
	Retry          string // the re-drill command that clears the warning on a pass
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
	Detail    string // the actual error when failing
	Degrading bool   // passed before, failing now
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
	Chain   bool            // member of the newest restore chain (green edge)
	Cells   []placementCell // per-place cells, index-aligned with dleDetail.Places
}

// volMap is the volume map: one bar per labeled volume, its stored files as segments
// in file-position order, width proportional to stored bytes. Two pages render it —
// /media/<name> shows everything on the pool (shaded by run age), /dles/<slug> shows
// the same volumes with only that DLE's archives colored (other content greyed, the
// newest restore chain outlined). One renderer, so the two views cannot disagree.
type volMap struct {
	Rows  []volMapRow
	Total int  // rows before the cap (per-run maps); 0 = uncapped map
	All   bool // ?all=1 lifted the cap
}

// Capped reports whether rows were held back (drives the "show all" note).
func (m *volMap) Capped() bool { return m.Total > len(m.Rows) && !m.All }

// volMapRow is one bar of a placement map.
type volMapRow struct {
	Label string
	Href  string // optional: the row label links here (a run page, a volume's medium page)
	Pill  string // optional warn pill next to the label (a partial run copy's "held/total")
	Chain bool   // holds part of the newest restore chain (green label edge)
	Segs  []volMapSeg
}

// volMapSeg is one stored file (an archive part) on the bar.
type volMapSeg struct {
	WidthPct float64
	Class    string // g0/g1/g2 (run age, dark = newest), other (not the focus DLE), + " chain"
	Title    string // hover text: run · DLE L# [· part i/n] · bytes
}

// volKey names one row of a placement map: a labeled volume of a pool, or — for an
// address-identified medium (disk/cloud), which has no volumes — the medium itself
// (Label empty), so a cloud copy draws as one bar instead of being invisible.
type volKey struct {
	Medium string
	Label  string // "" = the medium's own bar
}

// display is the row's on-page name: the volume label, or the bare medium.
func (k volKey) display() string {
	if k.Label != "" {
		return k.Label
	}
	return k.Medium
}

// sortVolKeys orders rows for a cross-media map: address-identified media first
// (the landing reads before the vault), then labeled volumes, each alphabetical.
func sortVolKeys(keys []volKey) {
	sort.Slice(keys, func(i, j int) bool {
		if (keys[i].Label == "") != (keys[j].Label == "") {
			return keys[i].Label == ""
		}
		if keys[i].Label != keys[j].Label {
			return keys[i].Label < keys[j].Label
		}
		return keys[i].Medium < keys[j].Medium
	})
}

// volSeg is one placed archive part on a placement-map row — the catalog fact the
// map builders collect before rendering (positions order the bar; bytes set
// segment widths; run/DLE/level drive shading and hover text).
type volSeg struct {
	Pos   int
	RunID string
	DLE   string // slug
	DLEID string // host:path display
	Level int
	Bytes int64
	Part  int // 1-based part number when the archive spanned; 0 = whole archive
	Parts int // total parts when spanned
}

// title renders the segment's hover text.
func (v volSeg) title() string {
	span := ""
	if v.Parts > 1 {
		span = fmt.Sprintf(" · part %d/%d", v.Part, v.Parts)
	}
	// The file position locates the segment on its row — the sequential file
	// number on the volume (or within the archive on a label-less medium), the
	// same FILE the `nb run <id>` segment table names.
	return fmt.Sprintf("%s · %s %s%s · %s · file %d",
		v.RunID, v.DLEID, levelTag(v.Level), span, sizeutil.FormatBytes(v.Bytes), v.Pos)
}

// physicalView is the /dles/<slug> physical panel: every container holding any
// archive of the DLE, always on — this DLE's bytes colored in true position and
// proportion (the newest restore chain in green, tip brightest), neighbors greyed.
// Gaps are deliberately absent: the history grid above judges them by route.
type physicalView struct {
	Groups []physGroup // one per medium, in the grid's column order
}

// physGroup is one medium's containers in the physical panel.
type physGroup struct {
	Medium string
	Note   string // heading gloss: "one row per volume" / "one row per run"
	Total  int    // cloud groups: run rows before the cap (0 = uncapped)
	Rows   []volMapRow
}

// Capped reports whether run rows were held back (drives the "show all" note).
func (g physGroup) Capped() bool { return g.Total > len(g.Rows) }

// buildVolMap renders per-row segments into bars. keys fixes the row order; capOf
// resolves a row's capacity (0 = unknown: the bar is scaled to its content);
// classOf maps each segment to its color class. Keys with no segments still get a
// row — an empty labeled volume is a fact worth seeing on a pool page. A labeled
// volume's bar is ordered by on-volume file position; a medium's own bar (no
// meaningful global position across archives) is ordered by run, oldest first, so
// both read as a timeline.
func buildVolMap(segs map[volKey][]volSeg, keys []volKey, capOf func(volKey) int64, classOf func(volSeg) string) *volMap {
	if len(keys) == 0 {
		return nil
	}
	m := &volMap{}
	for _, key := range keys {
		ss := append([]volSeg(nil), segs[key]...)
		if key.Label != "" {
			sort.Slice(ss, func(i, j int) bool { return ss[i].Pos < ss[j].Pos })
		} else {
			sort.Slice(ss, func(i, j int) bool {
				a, b := ss[i], ss[j]
				if a.RunID != b.RunID {
					return a.RunID < b.RunID
				}
				if a.DLE != b.DLE {
					return a.DLE < b.DLE
				}
				return a.Pos < b.Pos
			})
		}
		var sum int64
		for _, v := range ss {
			sum += v.Bytes
		}
		base := capOf(key)
		if sum > base {
			base = sum
		}
		row := volMapRow{Label: key.display()}
		for _, v := range ss {
			pct := 0.0
			if base > 0 {
				pct = float64(v.Bytes) / float64(base) * 100
			}
			row.Segs = append(row.Segs, volMapSeg{WidthPct: pct, Class: classOf(v), Title: v.title()})
		}
		m.Rows = append(m.Rows, row)
	}
	return m
}

// ageClass shades a segment by its run's recency: the pool's newest run is g0, the
// second g1, anything older g2 — run ids sort chronologically, so rank is lexical.
func ageClass(rank map[string]int, runID string) string {
	switch rank[runID] {
	case 0:
		return "g0"
	case 1:
		return "g1"
	default:
		return "g2"
	}
}

// runRank ranks the distinct run ids newest-first for ageClass.
func runRank(segs map[volKey][]volSeg, focusDLE string) map[string]int {
	var ids []string
	seen := map[string]bool{}
	for _, ss := range segs {
		for _, v := range ss {
			if focusDLE != "" && v.DLE != focusDLE {
				continue
			}
			if !seen[v.RunID] {
				seen[v.RunID] = true
				ids = append(ids, v.RunID)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	rank := make(map[string]int, len(ids))
	for i, id := range ids {
		rank[id] = i
	}
	return rank
}
