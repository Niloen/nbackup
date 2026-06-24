package progress

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Niloen/nbackup/internal/sizeutil"
)

// Render writes a one-shot amstatus-style report of a run snapshot to w. now is
// the reference instant for elapsed/rate/ETA of an in-flight run (ignored once the
// run is terminal, which uses its recorded end time).
func Render(w io.Writer, s Snapshot, now time.Time) {
	active, done, failed, pending := s.Counts()

	fmt.Fprintf(w, "Run %s  [%s]\n", s.SlotID, s.Phase)
	fmt.Fprintf(w, "  started:  %s  (elapsed %s)\n", s.StartedAt.Local().Format("2006-01-02 15:04:05"), sizeutil.FormatElapsed(s.Elapsed(now)))
	fmt.Fprintf(w, "  workers:  %d configured, %d active\n", s.Workers, active)
	fmt.Fprintf(w, "  dles:     %d done, %d active, %d pending", done, active, pending)
	if failed > 0 {
		fmt.Fprintf(w, ", %d FAILED", failed)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DLE\tLEVEL\tSTATE\tPROGRESS\tDONE\tEST\tWRITTEN")
	for _, d := range s.DLEs {
		fmt.Fprintf(tw, "%s\tL%d\t%s\t%s\t%s\t%s\t%s\n",
			d.Name, d.Level, d.State, progressCell(d),
			sizeutil.FormatBytes(d.DoneBytes), estCell(d.EstBytes), sizeutil.FormatBytes(d.OutBytes))
	}
	tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Total:    %s of ~%s  (%.0f%%)\n",
		sizeutil.FormatBytes(s.TotalDone()), sizeutil.FormatBytes(s.TotalEst()), s.Pct())
	fmt.Fprintf(w, "Written:  %s to volume\n", sizeutil.FormatBytes(s.TotalOut()))
	if rate := s.Rate(now); rate > 0 {
		fmt.Fprintf(w, "Rate:     %s/s\n", sizeutil.FormatBytes(int64(rate)))
	}
	if eta, ok := s.ETA(now); ok {
		fmt.Fprintf(w, "ETA:      %s\n", sizeutil.FormatElapsed(eta))
	}
	for _, d := range s.DLEs {
		if d.State == StateFailed {
			fmt.Fprintf(w, "FAILED %s: %s\n", d.Name, d.Err)
		}
	}
}

// progressCell renders a small text bar plus percent against the estimate, or a
// dash when there is nothing to show yet.
func progressCell(d DLE) string {
	switch d.State {
	case StatePending:
		return "-"
	case StateFailed:
		return "failed"
	}
	if d.EstBytes <= 0 {
		return "n/a"
	}
	const width = 10
	filled := int(d.Pct() / 100 * width)
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("#", filled) + strings.Repeat(".", width-filled)
	return fmt.Sprintf("[%s] %3.0f%%", bar, d.Pct())
}

func estCell(b int64) string {
	if b <= 0 {
		return "?"
	}
	return "~" + sizeutil.FormatBytes(b)
}
