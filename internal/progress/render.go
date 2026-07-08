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
	if cell := DumpRates(s, now); cell != "" {
		fmt.Fprintf(w, "   %s", cell)
	}
	fmt.Fprintln(w)
	// One aggregate Flush line for the classic single-landing run; a fan-out (a route
	// with several landings) itemizes per landing instead — each with its own backlog
	// and rate, so a slow secondary is visible at a glance. The rates are the landing
	// lane's (trailing-window "now" + busy-time average), so in a mixed run they also
	// cover direct writes sharing the lane — the lane's speed, not just the drains'.
	if drains := s.LandingDrains(); len(drains) == 1 {
		fmt.Fprintf(w, "Flush:    %s of %s  (%.0f%%)",
			sizeutil.FormatBytes(s.TotalDrained()), sizeutil.FormatBytes(s.TotalToDrain()), s.DrainPct())
		if cell := WriteRates(s, drains[0].Landing, now); cell != "" {
			fmt.Fprintf(w, "   %s", cell)
		}
		fmt.Fprintln(w)
	} else if len(drains) > 1 {
		label := "Flush:"
		for _, ld := range drains {
			fmt.Fprintf(w, "%-9s %-8s  %s of %s  (%.0f%%)",
				label, ld.Landing, sizeutil.FormatBytes(ld.Done), sizeutil.FormatBytes(ld.Total), pct(ld.Done, ld.Total))
			if cell := WriteRates(s, ld.Landing, now); cell != "" {
				fmt.Fprintf(w, "   %s", cell)
			}
			fmt.Fprintln(w)
			label = ""
		}
	} else if len(s.Meters) > 0 {
		// No drains at all — an all-direct run (no holding disk, or nothing staged
		// yet). The landing lanes are still writing; show each one's throughput.
		label := "Landing:"
		for _, name := range s.Landings() {
			if _, ok := s.Meters[name]; !ok && s.WrittenTo(name) == 0 {
				continue
			}
			fmt.Fprintf(w, "%-9s %-8s  %s written", label, name, sizeutil.FormatBytes(s.WrittenTo(name)))
			if cell := WriteRates(s, name, now); cell != "" {
				fmt.Fprintf(w, "   %s", cell)
			}
			fmt.Fprintln(w)
			label = ""
		}
	}
	fmt.Fprintf(w, "Volume:   %s written\n", sizeutil.FormatBytes(s.TotalOnVolume()))
	if eta, ok := s.ETA(now); ok {
		fmt.Fprintf(w, "ETA:      %s\n", sizeutil.FormatElapsed(eta))
	}
	for _, sk := range s.Skipped {
		if sk.Tripped {
			fmt.Fprintf(w, "TRIPPED landing %s mid-run: %s — copies from before the failure landed, the rest are missing; repair: %s\n", sk.Landing, sk.Reason, sk.Repair(s.RunID))
		} else {
			fmt.Fprintf(w, "SKIPPED landing %s: %s — its copies are missing this run; repair: %s\n", sk.Landing, sk.Reason, sk.Repair(s.RunID))
		}
	}
	for _, d := range s.DLEs {
		if d.State == StateFailed {
			fmt.Fprintf(w, "FAILED %s: %s\n", d.Name, d.Err)
		}
	}
	if s.Err != "" {
		fmt.Fprintf(w, "FAILED: %s\n", s.Err)
	}
}

// DumpRates renders the dump line's rate cell — shared by `nb status` and the web
// /status so the wording never drifts: the trailing-window rate first ("is it
// moving right now"), the whole-window average for context. Once dumping is over
// (or the run is), only the average remains — "now" has nothing left to say.
func DumpRates(s Snapshot, now time.Time) string {
	avg := s.Rate(now)
	if !s.Phase.Terminal() && s.DumpEndedAt.IsZero() {
		if r := s.DumpRateNow(now); r > 0 {
			cell := sizeutil.FormatBytes(int64(r)) + "/s now"
			if avg > 0 {
				cell += " · avg " + sizeutil.FormatBytes(int64(avg)) + "/s"
			}
			return cell
		}
	}
	if avg > 0 {
		return "avg " + sizeutil.FormatBytes(int64(avg)) + "/s"
	}
	return ""
}

// WriteRates renders one landing lane's rate cell — shared by `nb status` and the
// web /status so the wording never drifts. Leading part while the run is
// live: the trailing-window rate when bytes are flowing, or the word "idle" when no
// writer is on the lane — the honest reading of a drainer waiting for dumps, where a
// wall-clock average would just quietly shrink. Then the busy-time average (the
// lane's real speed while writing) with its utilization — how much of the run the
// lane has actually spent writing.
func WriteRates(s Snapshot, landing string, now time.Time) string {
	var parts []string
	if !s.Phase.Terminal() {
		// The meter outranks the window: with no writer on the lane, "idle" is the
		// truth right now — the trailing window would keep showing a decaying tail
		// of the last burst for up to its whole width.
		if !s.WriteActive(landing) {
			parts = append(parts, "idle")
		} else if r := s.WriteRateNow(landing, now); r > 0 {
			parts = append(parts, sizeutil.FormatBytes(int64(r))+"/s now")
		}
	}
	if avg := s.WriteRate(landing, now); avg > 0 {
		cell := "avg " + sizeutil.FormatBytes(int64(avg)) + "/s"
		if u := s.WriteUtilization(landing, now); u > 0 {
			cell += fmt.Sprintf(" · busy %.0f%%", u*100)
		}
		parts = append(parts, cell)
	}
	return strings.Join(parts, " · ")
}

// renderEstimating reports the sizing prelude of a run: how many DLEs have been
// measured and the size accumulated so far. No bytes are dumped yet, so the dump
// table (progress against estimate) would be a wall of zeroes — instead the per-DLE
// table shows each DLE's sizing state, its measured size (which lands in DoneBytes
// as each is sized), and how long its measurement is taking/took — so a slow or
// stuck estimate names its culprit the same way the dump table does.
func renderEstimating(w io.Writer, s Snapshot, now time.Time) {
	active, done, failed, _ := s.Counts()
	fmt.Fprintf(w, "Run %s  [%s]\n", s.RunID, s.Phase)
	fmt.Fprintf(w, "  started:  %s  (elapsed %s)\n", sizeutil.FormatStampSec(s.StartedAt.Local()), sizeutil.FormatElapsed(s.Elapsed(now)))
	fmt.Fprintf(w, "  sizing:   %d of %d DLEs measured, %d running\n", done+failed, len(s.DLEs), active)
	fmt.Fprintf(w, "  estimate: ~%s so far\n", sizeutil.FormatBytes(s.TotalDone()))
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DLE\tSTATE\tSIZE\tTIME")
	for _, d := range s.DLEs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, estimateState(d), estimateSizeCell(d), estimateTimeCell(d, now))
	}
	tw.Flush()
}

// estimateState names a DLE's state during the sizing prelude: the tracker reuses
// the dump-state vocabulary (StateDumping = its estimate pass is running), which
// would mislead here — nothing is dumped while estimating.
func estimateState(d DLE) string {
	switch d.State {
	case StateDumping:
		return "sizing"
	case StateDone:
		return "sized"
	default:
		return string(d.State)
	}
}

// estimateSizeCell renders a DLE's measured size once sized ("n/a" when the archiver
// produced no estimate — e.g. its tool is missing), or a dash while pending/running.
func estimateSizeCell(d DLE) string {
	if d.State != StateDone {
		return "-"
	}
	if d.DoneBytes <= 0 {
		return "n/a"
	}
	return sizeutil.FormatBytes(d.DoneBytes)
}

// estimateTimeCell renders how long a DLE's measurement is taking (still running) or
// took (finished) — the column that points at a slow estimate's culprit.
func estimateTimeCell(d DLE, now time.Time) string {
	switch {
	case d.State == StateDumping && !d.StartedAt.IsZero():
		return sizeutil.FormatElapsed(now.Sub(d.StartedAt))
	case !d.StartedAt.IsZero() && !d.EndedAt.IsZero():
		return sizeutil.FormatElapsed(d.EndedAt.Sub(d.StartedAt))
	}
	return "-"
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
