package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// liveProgressRows caps how many active DLEs the in-place region lists, so the
// frame stays a fixed bounded height (it must not scroll, or the cursor rewind
// desyncs). Excess actives collapse into a "+N more" line.
const liveProgressRows = 8

// liveNameMax caps a DLE name in the live frame, so a partitioned source's long
// absolute path can't push the numbers after it off the terminal edge (the
// frame's own truncation cuts the line's tail — exactly the part that matters).
const liveNameMax = 44

// liveName renders a DLE name for a live row, truncated to liveNameMax when
// needed: the host prefix and the path's tail are kept and the cut is marked
// with a leading "-" — amreport's convention, hosts being told apart by their
// head and paths by their leaf.
func liveName(name string) string {
	if len(name) <= liveNameMax {
		return name
	}
	host := ""
	path := name
	if i := strings.IndexByte(name, ':'); i >= 0 {
		host, path = name[:i+1], name[i+1:]
	}
	keep := liveNameMax - len(host) - 1 // -1 for the "-" marker
	if r := []rune(path); keep > 0 && keep < len(r) {
		return host + "-" + string(r[len(r)-keep:])
	}
	r := []rune(name)
	return "-" + string(r[len(r)-(liveNameMax-1):])
}

// estimateProgress returns a live sink that paints estimate progress to stderr, or
// nil when output is quiet or stderr is not a terminal (a pipe/file/cron log), so
// non-interactive callers see clean output.
func estimateProgress(quiet bool) progress.Sink {
	if quiet || !stderrIsTerminal() {
		return nil
	}
	return progress.LiveSink(os.Stderr, estimateLines)
}

// runProgress returns a live sink that paints dump progress to stderr, so an
// operator sees per-DLE percent and totals without running `nb status`. Nil when
// quiet or stderr is not a terminal.
func runProgress(quiet bool) progress.Sink {
	if quiet || !stderrIsTerminal() {
		return nil
	}
	return progress.LiveSink(os.Stderr, runLines)
}

// estimateLines renders the estimate phase: a counter plus the DLEs currently being
// sized. Sizes appear in the final plan table, so the live view stays minimal.
func estimateLines(s progress.Snapshot) []string {
	active, done, _, _ := s.Counts()
	lines := []string{fmt.Sprintf("Estimating sizes… %d/%d done, %d running", done, len(s.DLEs), active)}
	return append(lines, activeRows(s, func(d progress.DLE) string {
		return "  ▸ " + liveName(d.Name)
	})...)
}

// runLines renders the dump phase: a header, the DLEs currently dumping with their
// progress against estimate, and a running total.
func runLines(s progress.Snapshot) []string {
	active, done, failed, pending := s.Counts()
	head := fmt.Sprintf("Dumping %s  %d done / %d active / %d pending", s.RunID, done, active, pending)
	if failed > 0 {
		head += fmt.Sprintf(" / %d FAILED", failed)
	}
	lines := []string{head}
	lines = append(lines, activeRows(s, func(d progress.DLE) string {
		if d.State == progress.StateFlushing { // dumped to a holding disk; flushing to the landing
			from := ""
			if d.Holding != "" {
				from = " from " + d.Holding
			}
			pct := ""
			if d.OutBytes > 0 {
				pct = fmt.Sprintf("  %3.0f%%", d.DrainPct())
			}
			return fmt.Sprintf("  ▸ %s L%d  flushing%s%s", liveName(d.Name), d.Level, from, pct)
		}
		pct := ""
		if d.EstBytes > 0 {
			pct = fmt.Sprintf("  %3.0f%%", d.Pct())
		}
		approx := ""
		if d.DoneApprox { // inferred from compressed flow (client-fused remote dump)
			approx = "~"
		}
		return fmt.Sprintf("  ▸ %s L%d  %s%s of ~%s%s", liveName(d.Name), d.Level,
			approx, sizeutil.FormatBytes(d.DoneBytes), sizeutil.FormatBytes(d.EstBytes), pct)
	})...)
	totalApprox := ""
	if s.DoneApproxAny() {
		totalApprox = "~"
	}
	lines = append(lines, fmt.Sprintf("Total: %s%s of ~%s  (%.0f%%)",
		totalApprox, sizeutil.FormatBytes(s.TotalDone()), sizeutil.FormatBytes(s.TotalEst()), s.Pct()))
	if toDrain := s.TotalToDrain(); toDrain > 0 {
		lines = append(lines, fmt.Sprintf("Flush: %s of %s  (%.0f%%)",
			sizeutil.FormatBytes(s.TotalDrained()), sizeutil.FormatBytes(toDrain), s.DrainPct()))
	}
	return lines
}

// activeRows formats up to liveProgressRows actively-running DLEs with row, folding
// any overflow into a single "+N more" line so the frame height stays bounded. Active
// means dumping or draining (StateFlushing) — matching Snapshot.Counts, so a DLE the
// drainer is still copying to the landing stays visible instead of vanishing from the
// list while the run waits on it. (In the estimate phase no DLE is flushing.)
func activeRows(s progress.Snapshot, row func(progress.DLE) string) []string {
	var rows []string
	overflow := 0
	for _, d := range s.DLEs {
		if d.State != progress.StateDumping && d.State != progress.StateFlushing {
			continue
		}
		if len(rows) >= liveProgressRows {
			overflow++
			continue
		}
		rows = append(rows, row(d))
	}
	if overflow > 0 {
		rows = append(rows, fmt.Sprintf("  … +%d more", overflow))
	}
	return rows
}
