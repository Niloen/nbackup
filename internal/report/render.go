package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Niloen/nbackup/internal/sizeutil"
)

// Render writes a digest of recent runs to w (oldest-first input), modeled on
// Amanda's amreport: a summary line, a per-run table, a failure summary, and — when
// the window includes a drill — a recovery-health note. It is the one renderer
// shared by `nb report` and (via RenderRun) the notify body, so the terminal
// digest, the email, and the webhook payload never diverge.
func Render(w io.Writer, runs []Run, now time.Time) {
	if len(runs) == 0 {
		fmt.Fprintln(w, "No runs recorded yet.")
		return
	}

	var failures int
	var bytesMoved int64
	for _, r := range runs {
		if r.Failed() {
			failures++
		}
		bytesMoved += r.BytesMoved
	}
	first, last := runs[0].StartedAt, runs[len(runs)-1].StartedAt
	fmt.Fprintf(w, "NBackup report — %d run(s) from %s to %s\n",
		len(runs), first.Local().Format("2006-01-02 15:04"), last.Local().Format("2006-01-02 15:04"))
	if failures > 0 {
		fmt.Fprintf(w, "%d run(s) FAILED, %s moved\n\n", failures, sizeutil.FormatBytes(bytesMoved))
	} else {
		fmt.Fprintf(w, "all runs OK, %s moved\n\n", sizeutil.FormatBytes(bytesMoved))
	}

	// Per-run table, newest first (the most recent run is the one an operator
	// reading a morning email cares about first).
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tCOMMAND\tOUTCOME\tDETAIL")
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		outcome := "OK"
		if r.Failed() {
			outcome = "FAILED"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			r.StartedAt.Local().Format("2006-01-02 15:04"), r.Command, outcome, detailCell(r))
	}
	tw.Flush()

	// Failure summary — Amanda's FAILURE DUMP SUMMARY: what broke and why.
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
				r.StartedAt.Local().Format("2006-01-02 15:04"), r.Command, r.ExitClass, r.Error)
		}
	}
	// The recovery-health picture is rendered separately by the caller from the live
	// drill ledger (see cli.renderDrillLedger), which reflects the current time and
	// carries per-class remedies — richer than what a single past record holds.
}

// RenderRun writes a single run's detail to w — the body of a per-run notification.
// It leads with the outcome so the first line of an alert says what happened.
func RenderRun(w io.Writer, r Run) {
	outcome := "OK"
	if r.Failed() {
		outcome = "FAILED"
	}
	fmt.Fprintf(w, "%s %s — %s\n", r.Command, outcome, r.StartedAt.Local().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(w, "  elapsed: %s\n", sizeutil.FormatElapsed(r.EndedAt.Sub(r.StartedAt)))
	if d := detailCell(r); d != "-" {
		fmt.Fprintf(w, "  detail:  %s\n", d)
	}
	if r.Failed() {
		fmt.Fprintf(w, "  error [%s]: %s\n", r.ExitClass, r.Error)
	}
	renderRecovery(w, &r)
}

// detailCell summarizes a run's per-command outcome for the table/notification: what
// it moved or how many checks failed.
func detailCell(r Run) string {
	switch r.Command {
	case CommandDump:
		if r.SlotID == "" {
			return "-"
		}
		return fmt.Sprintf("%s, %d archive(s), %s", r.SlotID, r.Archives, sizeutil.FormatBytes(r.BytesMoved))
	case CommandSync:
		return fmt.Sprintf("%d slot(s) copied, %s", r.SlotsCopied, sizeutil.FormatBytes(r.BytesMoved))
	case CommandPrune:
		return fmt.Sprintf("%d slot(s) pruned, %s freed", r.SlotsPruned, sizeutil.FormatBytes(r.BytesMoved))
	case CommandVerify:
		if r.Failures > 0 {
			return fmt.Sprintf("%d slot(s) failed verification", r.Failures)
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
// answer. It prints nothing when r is nil or carries no drill signal.
func renderRecovery(w io.Writer, r *Run) {
	if r == nil {
		return
	}
	var degrading, failing []string
	for _, h := range r.DrillHealth {
		if !h.OK && h.Drilled {
			if h.WasOK {
				degrading = append(degrading, h.DLE)
			} else {
				failing = append(failing, fmt.Sprintf("%s [%s]", h.DLE, h.Class))
			}
		}
	}
	if len(degrading) == 0 && len(failing) == 0 && len(r.NeverDrilled) == 0 && r.Overdue == 0 {
		return
	}
	fmt.Fprintln(w, "\nRECOVERY HEALTH")
	if len(degrading) > 0 {
		sort.Strings(degrading)
		fmt.Fprintf(w, "  DEGRADING (passed before, failing now): %s\n", strings.Join(degrading, ", "))
	}
	if len(failing) > 0 {
		sort.Strings(failing)
		fmt.Fprintf(w, "  failing: %s\n", strings.Join(failing, ", "))
	}
	if n := len(r.NeverDrilled); n > 0 {
		fmt.Fprintf(w, "  never drilled: %s\n", strings.Join(r.NeverDrilled, ", "))
	}
	if r.Overdue > 0 {
		fmt.Fprintf(w, "  %d DLE(s) overdue for a drill\n", r.Overdue)
	}
}
