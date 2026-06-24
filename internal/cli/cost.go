package cli

import (
	"fmt"
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

// confirmRead surfaces the egress cost of reading bytes off a cloud medium and, when
// material and interactive, asks the operator to confirm before pulling them. It
// returns false only on an explicit decline at an interactive prompt: a
// non-interactive run (cron) prints the estimate and proceeds, never blocking — the
// estimate is informational, exactly as planning never blocks. yes skips the prompt.
func confirmRead(est engine.ReadEstimate, yes bool) bool {
	if !est.Priced || est.Bytes == 0 || est.Cost < materialEgressUSD {
		return true // a local medium, nothing to read, or an immaterial charge
	}
	fmt.Printf("\nThis reads %s off %q (%s): estimated egress %s.\n",
		sizeutil.FormatBytes(est.Bytes), est.Medium, est.Provider, formatUSD(est.Cost))
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
