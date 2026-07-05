package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// materialEgressUSD is the threshold above which a cloud read's egress estimate is
// worth confirming. Below it the charge is noise (a few cents); above it the user
// sees the number and, interactively, confirms before paying it.
const materialEgressUSD = 1.00

// formatUSD renders a dollar amount with a leading $: extra precision for sub-cent
// sums (a small footprint's monthly cost) so they don't collapse to "$0.00".
func formatUSD(usd float64) string {
	switch {
	case usd == 0:
		return "$0.00"
	case usd > -0.10 && usd < 0.10:
		return fmt.Sprintf("$%.4f", usd)
	default:
		return fmt.Sprintf("$%.2f", usd)
	}
}

// printReadPlan renders a selective recovery's extraction plan — one line per archive, the
// EXPLAIN of the read the extract is about to run — before the confirmation prompt, so it
// shows regardless of whether the egress clears the confirm threshold (a ranged read is
// cheap in dollars but can still pull a large frame, hence slow: the plan makes that
// visible). Each row: the read strategy and fetch count, the encoded bytes pulled versus
// the whole-archive size, the copy and its egress price, and — on a whole read — why
// ranging was not possible.
func printReadPlan(rows []engine.ReadPlanRow) {
	if len(rows) == 0 {
		return
	}
	files := 0
	for _, r := range rows {
		files += r.Files
	}
	fmt.Printf("\nextraction plan — %s from %s:\n", plural(files, "file"), plural(len(rows), "archive"))
	tw := newTab(os.Stdout)
	fmt.Fprintln(tw, "  READ\tFETCHES\tPULLS\tEGRESS\tMEDIUM\tARCHIVE\tWHY")
	for _, r := range rows {
		read, fetches := "RANGED", fmt.Sprintf("%d", r.Fetches)
		if !r.Ranged {
			read, fetches = "WHOLE", "—"
		}
		pulls := sizeutil.FormatBytes(r.Read)
		if r.Ranged {
			pulls += " / " + sizeutil.FormatBytes(r.Whole)
		}
		egress := "local"
		if r.Priced {
			egress = "~" + formatUSD(r.Cost)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			read, fetches, pulls, egress, orDash(r.Medium),
			fmt.Sprintf("%s %s L%d", r.Ref.Run, r.DLE, r.Ref.Level), orDash(r.Reason))
	}
	tw.Flush()
}

// plural renders "1 file" / "3 files" — a count with its noun, pluralized by a trailing s.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// confirmRead surfaces the egress cost of reading bytes off a cloud medium and, when
// material and interactive, asks the operator to confirm before pulling them. It
// returns false only on an explicit decline at an interactive prompt: a
// non-interactive run (cron) prints the estimate and proceeds, never blocking — the
// estimate is informational, exactly as planning never blocks. yes skips the prompt.
func confirmRead(est engine.ReadEstimate, yes bool) bool {
	if !est.Priced || est.Bytes == 0 || est.Cost < materialEgressUSD {
		return true // a local medium, nothing to read, or an immaterial charge
	}
	scope := ""
	if est.Ranged {
		scope = " (a ranged read of only the selected file(s), not the whole archive)"
	}
	fmt.Printf("\nThis reads %s off %q (%s): estimated egress %s%s.\n",
		sizeutil.FormatBytes(est.Bytes), est.Medium, est.Provider, formatUSD(est.Cost), scope)
	if yes || !stdinIsTerminal() {
		return true
	}
	fmt.Print("proceed? (y/N): ")
	line, _ := stdinReader.ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "y", "yes":
		return true
	}
	fmt.Println("aborted")
	return false
}
