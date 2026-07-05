package web

import (
	"fmt"
	"html/template"
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
	d.Chart = usageChartSVG(st.Usage, st.Capacity)
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
// timestamps), so template.HTML is sound.
func usageChartSVG(series []catalog.UsageSample, capacity int64) template.HTML {
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
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="var(--warn)" font-size="11" text-anchor="end">capacity %s</text>`, vw-padR, cy-3, sizeutil.FormatBytes(capacity))
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
