package progress

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Niloen/nbackup/internal/sizeutil"
)

// Render writes a one-shot status report of a run snapshot to w. now is
// the reference instant for elapsed/rate/ETA of an in-flight run (ignored once the
// run is terminal, which uses its recorded end time).
func Render(w io.Writer, s Snapshot, now time.Time) {
	if s.Phase == PhaseEstimating {
		renderEstimating(w, s, now)
		return
	}
	active, done, failed, pending := s.Counts()

	fmt.Fprintf(w, "Run %s  [%s]\n", s.RunID, s.Phase)
	fmt.Fprintf(w, "  started:  %s  (elapsed %s)\n", sizeutil.FormatStampSec(s.StartedAt.Local()), sizeutil.FormatElapsed(s.Elapsed(now)))
	fmt.Fprintf(w, "  workers:  %d configured, %d active\n", s.Workers, active)
	fmt.Fprintf(w, "  dles:     %d done, %d active, %d pending", done, active, pending)
	if failed > 0 {
		fmt.Fprintf(w, ", %d FAILED", failed)
	}
	if canceled := s.Canceled(); canceled > 0 {
		fmt.Fprintf(w, ", %d canceled", canceled)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	// Two bars per DLE: DUMP meters source -> holding/landing (uncompressed, against the
	// estimate); FLUSH meters holding -> landing (compressed, against the staged size). The
	// three size columns read as the dump's progression: EST is the planner estimate DUMPED
	// (the uncompressed source size) races toward, and VOLUME is what has landed authoritatively.
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	// The VOLUME column names the volume(s) each DLE's data reached — a removable medium's
	// self-identity (a tape label; several comma-joined when the archive spanned volumes or,
	// on a multi-drive library, drives). It is medium-neutral but only printed when the landing
	// identifies its volumes by label; an address-identified landing (disk, cloud) is its own
	// sole volume and carries none, so the column is dropped. LANDED is the bytes on the
	// authoritative volume so far.
	labeled := anyVolume(s.DLEs)
	if labeled {
		fmt.Fprintln(tw, "DLE\tLEVEL\tSTATE\tDUMP\tFLUSH\tEST\tDUMPED\tLANDED\tVOLUME")
	} else {
		fmt.Fprintln(tw, "DLE\tLEVEL\tSTATE\tDUMP\tFLUSH\tEST\tDUMPED\tLANDED")
	}
	for _, d := range s.DLEs {
		fmt.Fprintf(tw, "%s\tL%d\t%s\t%s\t%s\t%s\t%s\t%s",
			d.Name, d.Level, stateCell(d), dumpCell(d), drainCell(d),
			estCell(d), sizeutil.FormatBytes(d.DoneBytes), sizeutil.FormatBytes(d.OnVolume()))
		if labeled {
			fmt.Fprintf(tw, "\t%s", volumeCell(d))
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Dump:     %s of ~%s  (%.0f%%)",
		sizeutil.FormatBytes(s.TotalDone()), sizeutil.FormatBytes(s.TotalEst()), s.Pct())
	if rate := s.Rate(now); rate > 0 {
		fmt.Fprintf(w, "   %s/s", sizeutil.FormatBytes(int64(rate)))
	}
	fmt.Fprintln(w)
	if toDrain := s.TotalToDrain(); toDrain > 0 {
		fmt.Fprintf(w, "Flush:    %s of %s  (%.0f%%)",
			sizeutil.FormatBytes(s.TotalDrained()), sizeutil.FormatBytes(toDrain), s.DrainPct())
		if rate := s.DrainRate(now); rate > 0 {
			fmt.Fprintf(w, "   %s/s", sizeutil.FormatBytes(int64(rate)))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Volume:   %s written\n", sizeutil.FormatBytes(s.TotalOnVolume()))
	if eta, ok := s.ETA(now); ok {
		fmt.Fprintf(w, "ETA:      %s\n", sizeutil.FormatElapsed(eta))
	}
	for _, d := range s.DLEs {
		if d.State == StateFailed {
			fmt.Fprintf(w, "FAILED %s: %s\n", d.Name, d.Err)
		}
	}
}

// renderEstimating reports the sizing prelude of a run: how many DLEs have been
// measured and the size accumulated so far. No bytes are dumped yet, so the dump
// table (progress against estimate) would be a wall of zeroes — this shows the
// estimate filling in instead. The per-DLE size lands in DoneBytes as each is sized.
func renderEstimating(w io.Writer, s Snapshot, now time.Time) {
	var sized int
	for _, d := range s.DLEs {
		if d.State == StateDone || d.State == StateFailed {
			sized++
		}
	}
	fmt.Fprintf(w, "Run %s  [%s]\n", s.RunID, s.Phase)
	fmt.Fprintf(w, "  started:  %s  (elapsed %s)\n", sizeutil.FormatStampSec(s.StartedAt.Local()), sizeutil.FormatElapsed(s.Elapsed(now)))
	fmt.Fprintf(w, "  sizing:   %d of %d DLEs measured\n", sized, len(s.DLEs))
	fmt.Fprintf(w, "  estimate: ~%s so far\n", sizeutil.FormatBytes(s.TotalDone()))
}

// anyVolume reports whether any DLE has landed on a labelled volume, so the VOLUME column
// is worth printing (a removable-medium run) rather than a column of dashes (a disk/cloud
// landing, which is its own sole address-identified volume).
func anyVolume(dles []DLE) bool {
	for _, d := range dles {
		if d.Volume != "" {
			return true
		}
	}
	return false
}

// volumeCell renders the landing volume(s) a DLE reached, or a dash before it has committed.
func volumeCell(d DLE) string {
	if d.Volume == "" {
		return "-"
	}
	return d.Volume
}

// stateCell renders a DLE's state, annotating a draining DLE with the holding disk it
// landed on (so a multi-disk run shows where each buffered): "flushing←scratch".
func stateCell(d DLE) string {
	if d.State == StateFlushing && d.Holding != "" {
		return "flushing←" + d.Holding
	}
	return string(d.State)
}

// estCell renders the planner's uncompressed size estimate for a DLE, or "n/a" when none was
// produced (the same no-estimate condition that makes dumpCell unable to draw a bar).
func estCell(d DLE) string {
	if d.EstBytes <= 0 {
		return "n/a"
	}
	return sizeutil.FormatBytes(d.EstBytes)
}

// dumpCell renders the DUMP bar — progress against the estimate — or a dash/marker when
// there is nothing to meter (pending, failed, or no estimate).
func dumpCell(d DLE) string {
	switch d.State {
	case StatePending:
		return "-"
	case StateFailed:
		return "failed"
	case StateCanceled:
		return "canceled"
	}
	if d.EstBytes <= 0 {
		return "n/a"
	}
	return barCell(d.Pct())
}

// drainCell renders the FLUSH bar for a holding-disk DLE — bytes copied to the landing against the
// staged size. While such a DLE is still dumping to its holding disk it shows "staging": the flush
// has not begun and its bytes are not on the volume yet. A direct dump has no separate flush: it
// shows "direct" once done (it streamed straight to the volume) and a dash while it is still dumping.
func drainCell(d DLE) string {
	if d.State == StateFailed || d.State == StateCanceled {
		return "-"
	}
	if d.Drains() {
		return barCell(d.DrainPct())
	}
	if d.ToHolding {
		return "staging"
	}
	if d.State == StateDone {
		return "direct"
	}
	return "-"
}

// barCell renders a fixed-width text bar plus percent, e.g. "[####......]  40%".
func barCell(pct float64) string {
	const width = 10
	filled := int(pct / 100 * width)
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	bar := strings.Repeat("#", filled) + strings.Repeat(".", width-filled)
	return fmt.Sprintf("[%s] %3.0f%%", bar, pct)
}
