package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Niloen/nbackup/internal/dletree"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// Render writes a digest of recent runs to w (oldest-first input): a summary
// line, a per-run table, a failure summary, and — when the window includes a
// drill — a recovery-health note. It is the one renderer
// shared by `nb report` and (via RenderRun) the notify body, so the terminal
// digest, the email, and the webhook payload never diverge.
func Render(w io.Writer, runs []Run, now time.Time) {
	if len(runs) == 0 {
		fmt.Fprintln(w, "No runs recorded yet.")
		return
	}

	var failures, warned int
	var bytesMoved int64
	for _, r := range runs {
		if r.Failed() {
			failures++
		} else if r.Warned() {
			warned++
		}
		bytesMoved += r.BytesMoved
	}
	first, last := runs[0].StartedAt, runs[len(runs)-1].StartedAt
	fmt.Fprintf(w, "NBackup report — %d run(s) from %s to %s\n",
		len(runs), sizeutil.FormatStamp(first.Local()), sizeutil.FormatStamp(last.Local()))
	switch {
	case failures > 0:
		fmt.Fprintf(w, "%d run(s) FAILED, %s moved\n\n", failures, sizeutil.FormatBytes(bytesMoved))
	case warned > 0:
		fmt.Fprintf(w, "%d run(s) with WARNINGS, %s moved\n\n", warned, sizeutil.FormatBytes(bytesMoved))
	default:
		fmt.Fprintf(w, "all runs OK, %s moved\n\n", sizeutil.FormatBytes(bytesMoved))
	}

	// Per-run table, newest first (the most recent run is the one an operator
	// reading a morning email cares about first).
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tCOMMAND\tOUTCOME\tDETAIL")
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			sizeutil.FormatStamp(r.StartedAt.Local()), r.Command, r.Status(), detailCell(r))
	}
	tw.Flush()

	// Failure summary: what broke and why.
	var failed []Run
	for _, r := range runs {
		if r.Failed() {
			failed = append(failed, r)
		}
	}
	if len(failed) > 0 {
		fmt.Fprintln(w, "\nFAILURES")
		for i := len(failed) - 1; i >= 0; i-- {
			r := failed[i]
			fmt.Fprintf(w, "  %s %s [%s]: %s\n",
				sizeutil.FormatStamp(r.StartedAt.Local()), r.Command, r.ExitClass, r.Error)
		}
	}
	// Warning summary: degraded runs and the repair each warning names.
	var hasWarnings bool
	for _, r := range runs {
		if len(r.Warnings) > 0 {
			hasWarnings = true
		}
	}
	if hasWarnings {
		fmt.Fprintln(w, "\nWARNINGS")
		for i := len(runs) - 1; i >= 0; i-- {
			r := runs[i]
			for _, warning := range r.Warnings {
				fmt.Fprintf(w, "  %s %s: %s\n",
					sizeutil.FormatStamp(r.StartedAt.Local()), r.Command, warning)
			}
		}
	}
	// The recovery-health picture is rendered separately by the caller from the live
	// drill ledger (see cli.renderDrillLedger), which reflects the current time and
	// carries per-class remedies — richer than what a single past record holds.
}

// RenderRun writes a single run's detail to w — the body of a per-run notification.
// It leads with the outcome so the first line of an alert says what happened.
func RenderRun(w io.Writer, r Run) {
	fmt.Fprintf(w, "%s %s — %s\n", r.Command, r.Status(), sizeutil.FormatStampSec(r.StartedAt.Local()))
	fmt.Fprintf(w, "  elapsed: %s\n", sizeutil.FormatElapsed(r.EndedAt.Sub(r.StartedAt)))
	if d := detailCell(r); d != "-" {
		fmt.Fprintf(w, "  detail:  %s\n", d)
	}
	if r.Failed() {
		fmt.Fprintf(w, "  error [%s]: %s\n", r.ExitClass, r.Error)
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(w, "  WARNING: %s\n", warning)
	}
	renderRecovery(w, r)
	// A dump notification carries the full per-DLE report,
	// so an operator sees what was backed up and how it compressed — not just totals.
	if r.Command == CommandDump && len(r.DumpStats) > 0 {
		fmt.Fprintln(w)
		renderStats(w, r.DumpStats, r.LandingStats, r.EndedAt.Sub(r.StartedAt))
		fmt.Fprintln(w)
		renderDumpTable(w, r.DumpStats)
		renderPromotions(w, r.DumpStats)
	}
}

// RenderDump writes a dump report for one run: a header, a one-line headline, the
// overall statistics grid (Total/Full/Incr), and the per-DLE statistics table. It
// is what `nb report --dump` prints and shares renderStats/renderDumpTable with the
// dump notification body.
func RenderDump(w io.Writer, r Run) {
	fmt.Fprintf(w, "DUMP REPORT  %s", r.RunID)
	if !r.StartedAt.IsZero() {
		// StartedAt is the command's real wall-clock execution time, which can differ
		// from the run's logical date (nb run/nb medium) under a --date override —
		// label it explicitly so the two are never mistaken for each other.
		fmt.Fprintf(w, "  (executed %s)", sizeutil.FormatStamp(r.StartedAt.Local()))
	}
	fmt.Fprintln(w)
	if r.Failed() {
		fmt.Fprintf(w, "run FAILED [%s]: %s\n", r.ExitClass, r.Error)
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(w, "WARNING: %s\n", warning)
	}
	if len(r.DumpStats) == 0 {
		fmt.Fprintln(w, "no per-DLE statistics recorded for this run")
		return
	}
	fmt.Fprintln(w, headline(r))
	fmt.Fprintln(w)
	renderStats(w, r.DumpStats, r.LandingStats, r.EndedAt.Sub(r.StartedAt))
	fmt.Fprintln(w)
	renderDumpTable(w, r.DumpStats)
	renderPromotions(w, r.DumpStats)
}

// headline is the one-line "did it work" summary for a dump: DLE count, the
// original->output roll-up with its compression, and wall-clock elapsed — the
// first line an operator reads before the table. On a failed run it leads with
// the failure so an alert says what broke before it says how big.
func headline(r Run) string {
	var a agg
	for _, d := range r.DumpStats {
		a.add(d)
	}
	sizes := fmt.Sprintf("%s -> %s (%s)", sizeutil.FormatBytes(a.orig), sizeutil.FormatBytes(a.out), compPct(a.orig, a.out))
	elapsed := sizeutil.FormatElapsed(r.EndedAt.Sub(r.StartedAt))
	if r.Failed() {
		return fmt.Sprintf("%d DLE(s) dumped, run FAILED [%s] · %s · %s elapsed", a.n, r.ExitClass, sizes, elapsed)
	}
	if r.Warned() {
		return fmt.Sprintf("%d DLE(s) dumped, %d WARNING(s) · %s · %s elapsed", a.n, len(r.Warnings), sizes, elapsed)
	}
	return fmt.Sprintf("%d DLE(s) dumped OK · %s · %s elapsed", a.n, sizes, elapsed)
}

// agg accumulates one column of the statistics grid: how many DLEs, their
// uncompressed/compressed bytes, files, and summed dump time.
type agg struct {
	n     int
	orig  int64
	out   int64
	files int
	secs  float64
}

func (a *agg) add(d DLEStat) {
	a.n++
	a.orig += d.Orig
	a.out += d.Out
	a.files += d.Files
	a.secs += d.Seconds
}

// renderStats writes the overall statistics grid: each metric as a row, split
// into Total / Full / Incr columns (Amanda's STATISTICS block). Dump time is the
// *sum* of per-DLE dump times — it exceeds wall-clock run time under parallel
// workers — while run time is the single wall-clock span, shown only in Total.
// Each landing then gets its write pair (Amanda's taper stats): time actually
// spent writing with its share of the run, and the rate over that busy time —
// the lane's real speed, never diluted by the stretches it sat waiting for dumps.
func renderStats(w io.Writer, stats []DLEStat, landings []LandingStat, wall time.Duration) {
	var tot, full, incr agg
	for _, d := range stats {
		tot.add(d)
		if d.Level == 0 {
			full.add(d)
		} else {
			incr.add(d)
		}
	}

	count := func(a agg) string {
		if a.n == 0 {
			return "-"
		}
		return fmt.Sprintf("%d", a.n)
	}
	size := func(a agg) string {
		if a.n == 0 {
			return "-"
		}
		return sizeutil.FormatBytes(a.orig)
	}
	out := func(a agg) string {
		if a.n == 0 {
			return "-"
		}
		return sizeutil.FormatBytes(a.out)
	}
	files := func(a agg) string {
		if a.n == 0 {
			return "-"
		}
		return fmt.Sprintf("%d", a.files)
	}

	rows := [][4]string{
		{"DLEs dumped", count(tot), count(full), count(incr)},
		{"Original size", size(tot), size(full), size(incr)},
		{"Output size", out(tot), out(full), out(incr)},
		{"Avg compression", compPct(tot.orig, tot.out), compPct(full.orig, full.out), compPct(incr.orig, incr.out)},
		{"Files", files(tot), files(full), files(incr)},
		{"Dump time (sum)", dumpTime(tot.secs), dumpTime(full.secs), dumpTime(incr.secs)},
		{"Avg dump rate", dumpRate(tot.orig, tot.secs), dumpRate(full.orig, full.secs), dumpRate(incr.orig, incr.secs)},
	}
	if wall > 0 {
		rows = append(rows, [4]string{"Run time (wall)", sizeutil.FormatElapsed(wall), "", ""})
	}
	for _, ls := range landings {
		name := ls.Landing
		if name == "" {
			name = "landing"
		}
		rows = append(rows,
			[4]string{"Write time (" + name + ")", writeTimeCell(ls), "", ""},
			[4]string{"Avg write rate (" + name + ")", writeRateCell(ls), "", ""})
	}

	// Column widths: the label column is left-justified, the three value columns
	// right-justified so the numbers line up on their right edge (Amanda's grid).
	header := [4]string{"STATISTICS", "Total", "Full", "Incr"}
	var wLabel, w1, w2, w3 int
	for _, r := range append([][4]string{header}, rows...) {
		wLabel = max(wLabel, len(r[0]))
		w1 = max(w1, len(r[1]))
		w2 = max(w2, len(r[2]))
		w3 = max(w3, len(r[3]))
	}
	line := func(r [4]string) {
		s := fmt.Sprintf("%-*s  %*s  %*s  %*s", wLabel, r[0], w1, r[1], w2, r[2], w3, r[3])
		fmt.Fprintln(w, strings.TrimRight(s, " "))
	}
	line(header)
	for _, r := range rows {
		line(r)
	}
}

// renderDumpTable writes the per-DLE statistics table. The full/incremental
// roll-up lives in the statistics grid (renderStats); this is the per-DLE detail.
// Rate is uncompressed bytes over dump time; a row with unknown timing shows a
// dash for time and rate. When any DLE went through a holding disk, two flush
// columns follow (Amanda's per-DLE taper stats beside its dumper stats): the
// drain's copy time and its compressed rate over that time — a direct dump shows
// dashes there, its landing write already being the dump itself.
//
// Rows are arranged by path (dletree): a partitioned source's many DLEs render
// as one group — a header row carrying the shared host:base prefix and the
// group's totals, then each member under a short base-relative label — so the
// long absolute paths never set the table's column width, and a source with
// fifty children reads as one entry with a subtotal instead of fifty repeats.
func renderDumpTable(w io.Writer, stats []DLEStat) {
	flushed := anyFlushed(stats)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	if flushed {
		fmt.Fprintln(tw, "DLE\tLVL\tORIG\tOUT\tCOMP%\tFILES\tTIME\tRATE\tFLUSH\tFL-RATE")
	} else {
		fmt.Fprintln(tw, "DLE\tLVL\tORIG\tOUT\tCOMP%\tFILES\tTIME\tRATE")
	}
	row := func(label string, d DLEStat) {
		lvl := fmt.Sprintf("%d", d.Level)
		if d.Promoted {
			lvl += "*" // a promoted full; explained by renderPromotions below the table
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s",
			label, lvl, sizeutil.FormatBytes(d.Orig), sizeutil.FormatBytes(d.Out),
			compPct(d.Orig, d.Out), d.Files, dumpTime(d.Seconds), dumpRate(d.Orig, d.Seconds))
		if flushed {
			fmt.Fprintf(tw, "\t%s\t%s", dumpTime(d.FlushSeconds), dumpRate(d.FlushBytes, d.FlushSeconds))
		}
		fmt.Fprintln(tw)
	}
	items := make([]dletree.Item, len(stats))
	for i, d := range stats {
		if d.Host == "" && d.Path == "" {
			items[i] = dletree.Item{Path: d.DLE} // bare-slug fallback: stays flat
		} else {
			items[i] = dletree.Item{Host: d.Host, Path: d.Path, Rest: d.Rest}
		}
	}
	for _, g := range dletree.Build(items) {
		if g.Children == nil {
			row(stats[g.Index].ID(), stats[g.Index])
			continue
		}
		var a agg
		var flushB int64
		var flushS float64
		for _, c := range g.Children {
			a.add(stats[c.Index])
			flushB += stats[c.Index].FlushBytes
			flushS += stats[c.Index].FlushSeconds
		}
		fmt.Fprintf(tw, "%s · %d DLEs\t\t%s\t%s\t%s\t%d\t%s\t%s",
			g.ID(), a.n, sizeutil.FormatBytes(a.orig), sizeutil.FormatBytes(a.out),
			compPct(a.orig, a.out), a.files, dumpTime(a.secs), dumpRate(a.orig, a.secs))
		if flushed {
			fmt.Fprintf(tw, "\t%s\t%s", dumpTime(flushS), dumpRate(flushB, flushS))
		}
		fmt.Fprintln(tw)
		for i, c := range g.Children {
			row("  "+dletree.Branch(i, len(g.Children))+" "+g.Label(c), stats[c.Index])
		}
	}
	tw.Flush()
}

// anyFlushed reports whether any DLE recorded a drain, so the flush columns are
// worth printing at all.
func anyFlushed(stats []DLEStat) bool {
	for _, d := range stats {
		if d.FlushSeconds > 0 {
			return true
		}
	}
	return false
}

// renderPromotions explains the run's promoted fulls (the `*` rows of the dump
// table): which DLEs the planner pulled forward of their cycle deadline, their
// total, and each one's reason — so a night that ran big says why. Prints
// nothing when the run had no promotions.
func renderPromotions(w io.Writer, stats []DLEStat) {
	var promoted []DLEStat
	var bytes int64
	for _, d := range stats {
		if d.Promoted {
			promoted = append(promoted, d)
			bytes += d.Out
		}
	}
	if len(promoted) == 0 {
		return
	}
	fmt.Fprintf(w, "\nPROMOTED FULLS (*) — %d full(s), %s pulled forward to level the cycle\n",
		len(promoted), sizeutil.FormatBytes(bytes))
	for _, d := range promoted {
		fmt.Fprintf(w, "  %s — %s\n", d.ID(), PromotionWhy(d.Reason))
	}
}

// PromotionWhy unwraps the planner's "promoted full (...)" reason to just the
// why for a promotions note, where the "promoted full" part is the heading.
// A reason in an unexpected shape (or missing) is shown as-is / as a dash.
// Exported for the web dump report, which renders the same note.
func PromotionWhy(reason string) string {
	if inner, ok := strings.CutPrefix(reason, "promoted full ("); ok {
		return strings.TrimSuffix(inner, ")")
	}
	if reason == "" {
		return "-"
	}
	return reason
}

// compPct renders the compression ratio (output as a percent of original), or a dash
// when there is no original size to measure against.
func compPct(orig, out int64) string {
	if orig <= 0 {
		return "-"
	}
	if out >= orig {
		// No space was saved (the `none` scheme, or incompressible data). A
		// percentage here reads backwards — "100%" looks like "fully compressed" —
		// so show a dash instead.
		return "-"
	}
	return fmt.Sprintf("%.0f%%", float64(out)/float64(orig)*100)
}

// dumpTime renders a dump duration, or a dash when timing was unavailable.
func dumpTime(secs float64) string {
	if secs <= 0 {
		return "-"
	}
	return sizeutil.FormatElapsed(time.Duration(secs * float64(time.Second)))
}

// dumpRate renders uncompressed throughput, or a dash without timing.
func dumpRate(orig int64, secs float64) string {
	if secs <= 0 || orig <= 0 {
		return "-"
	}
	return sizeutil.FormatBytes(int64(float64(orig)/secs)) + "/s"
}

// writeTimeCell renders a landing's busy time with its share of the run's wall
// clock — "12m34s (40% busy)" — or a dash when nothing was timed.
func writeTimeCell(ls LandingStat) string {
	if ls.BusySeconds <= 0 {
		return "-"
	}
	cell := sizeutil.FormatElapsed(time.Duration(ls.BusySeconds * float64(time.Second)))
	if ls.WallSeconds > 0 {
		cell += fmt.Sprintf(" (%.0f%% busy)", ls.BusySeconds/ls.WallSeconds*100)
	}
	return cell
}

// writeRateCell renders a landing's throughput over its busy time, or a dash.
func writeRateCell(ls LandingStat) string {
	if ls.BusySeconds <= 0 || ls.Bytes <= 0 {
		return "-"
	}
	return sizeutil.FormatBytes(int64(float64(ls.Bytes)/ls.BusySeconds)) + "/s"
}

// detailCell summarizes a run's per-command outcome for the table/notification: what
// it moved or how many checks failed.
func detailCell(r Run) string {
	switch r.Command {
	case CommandDump:
		if r.RunID == "" {
			return "-"
		}
		return fmt.Sprintf("%s, %d archive(s), %s", r.RunID, r.Archives, sizeutil.FormatBytes(r.BytesMoved))
	case CommandSync:
		return fmt.Sprintf("%d run(s) copied, %s", r.RunsCopied, sizeutil.FormatBytes(r.BytesMoved))
	case CommandPrune:
		return fmt.Sprintf("%d archive(s) pruned, %s freed", r.ArchivesPruned, sizeutil.FormatBytes(r.BytesMoved))
	case CommandVerify:
		if r.Failures > 0 {
			return fmt.Sprintf("%d run(s) failed verification", r.Failures)
		}
		return "all verified"
	case CommandDrill:
		parts := []string{fmt.Sprintf("%d failure(s)", r.Failures)}
		if r.Skipped > 0 {
			parts = append(parts, fmt.Sprintf("%d skipped", r.Skipped))
		}
		if r.Overdue > 0 {
			parts = append(parts, fmt.Sprintf("%d overdue", r.Overdue))
		}
		return strings.Join(parts, ", ")
	}
	return "-"
}

// renderRecovery prints the recovery-health note for a drill run: DLEs degrading
// (passed before, failing now), never drilled, or overdue — the "trending bad"
// answer. It prints nothing when r carries no drill signal. Each list is one
// line, so it folds path siblings and caps its length (foldDLEs) rather than
// growing into a paragraph on a partitioned source's fifty children.
func renderRecovery(w io.Writer, r Run) {
	var degrading, failing []string
	for _, h := range r.DrillHealth {
		if !h.OK && h.Drilled {
			if h.WasOK {
				degrading = append(degrading, h.Name())
			} else {
				failing = append(failing, fmt.Sprintf("%s [%s]", h.Name(), h.Class))
			}
		}
	}
	if len(degrading) == 0 && len(failing) == 0 && len(r.NeverDrilled) == 0 && r.Overdue == 0 {
		return
	}
	fmt.Fprintln(w, "\nRECOVERY HEALTH")
	if len(degrading) > 0 {
		fmt.Fprintf(w, "  DEGRADING (passed before, failing now): %s\n", dletree.FoldList(degrading))
	}
	if len(failing) > 0 {
		// Not folded: each entry carries its own failure class, which a group
		// rollup would erase. Capped all the same.
		sort.Strings(failing)
		fmt.Fprintf(w, "  failing: %s\n", dletree.CapList(failing))
	}
	if n := len(r.NeverDrilled); n > 0 {
		fmt.Fprintf(w, "  never drilled: %s\n", dletree.FoldList(r.NeverDrilled))
	}
	if r.Overdue > 0 {
		fmt.Fprintf(w, "  %d DLE(s) overdue for a drill\n", r.Overdue)
	}
}
