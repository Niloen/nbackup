package cli

import (
	"fmt"
	"os"

	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// liveProgressRows caps how many active DLEs the in-place region lists, so the
// frame stays a fixed bounded height (it must not scroll, or the cursor rewind
// desyncs). Excess actives collapse into a "+N more" line.
const liveProgressRows = 8

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
		return "  ▸ " + d.Name
	})...)
}

// runLines renders the dump phase: a header, the DLEs currently dumping with their
// progress against estimate, and a running total.
func runLines(s progress.Snapshot) []string {
	active, done, failed, pending := s.Counts()
	head := fmt.Sprintf("Dumping %s  %d done / %d active / %d pending", s.SlotID, done, active, pending)
	if failed > 0 {
		head += fmt.Sprintf(" / %d FAILED", failed)
	}
	lines := []string{head}
	lines = append(lines, activeRows(s, func(d progress.DLE) string {
		pct := ""
		if d.EstBytes > 0 {
			pct = fmt.Sprintf("  %3.0f%%", d.Pct())
		}
		return fmt.Sprintf("  ▸ %s L%d  %s of ~%s%s", d.Name, d.Level,
			sizeutil.FormatBytes(d.DoneBytes), sizeutil.FormatBytes(d.EstBytes), pct)
	})...)
	return append(lines, fmt.Sprintf("Total: %s of ~%s  (%.0f%%)",
		sizeutil.FormatBytes(s.TotalDone()), sizeutil.FormatBytes(s.TotalEst()), s.Pct()))
}

// activeRows formats up to liveProgressRows actively-running DLEs with row, folding
// any overflow into a single "+N more" line so the frame height stays bounded.
func activeRows(s progress.Snapshot, row func(progress.DLE) string) []string {
	var rows []string
	overflow := 0
	for _, d := range s.DLEs {
		if d.State != progress.StateDumping {
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
