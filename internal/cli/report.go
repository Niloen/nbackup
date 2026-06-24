package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/report"
)

// newReportCmd implements `nb report`: the run digest. It reads the run history
// (run-log.jsonl) and the drill ledger (drill-ledger.json) the other commands write
// and renders an Amanda-amreport-style summary — what ran, what failed, and which
// DLEs' recovery health is degrading or stale. It needs no engine and takes no lock
// (read-only, like `nb status`), so it is cheap to run from cron after the nightly
// `nb dump && nb sync && nb drill`. With --notify it also sends the digest through
// the configured `digest:` notification backends.
func newReportCmd(a *app) *cobra.Command {
	var last int
	var asJSON, notify bool
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Summarize the last N runs and recovery health",
		Long: "Render a digest of recent runs from the run history every dump/sync/verify/drill/prune " +
			"writes: a per-run table, a failure summary, and a recovery-health section flagging DLEs whose " +
			"drills are failing, degrading (passed before, failing now), stale, or never run. Reads only — no " +
			"engine, no lock — so it is cheap to run from cron. With --notify it sends the digest through the " +
			"config's `notify.digest` backends (e.g. a nightly email); with --json it emits the raw run records.",
		Example: "  nb report\n  nb report --last 30\n  nb report --json\n  nb dump && nb sync && nb drill --unattended; nb report --notify",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadRO()
			if err != nil {
				return err
			}
			dir := cfg.WorkdirPath()
			runs, err := report.Last(dir, last)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(runs)
			}
			report.Render(os.Stdout, runs, time.Now())
			renderDrillLedger(os.Stdout, cfg, time.Now())
			if notify {
				a.dispatchDigest(cfg, runs)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&last, "last", 10, "summarize the last N runs (0 = all)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw run records as JSON instead of a text digest")
	cmd.Flags().BoolVar(&notify, "notify", false, "also send the digest through the config's notify.digest backends")
	return cmd
}

// renderDrillLedger prints the live recovery-health picture from the drill ledger:
// DLEs whose last drill is failing (with the remedy for the class), stale (passed
// but overdue for a re-drill), or — for configured DLEs — never drilled. It reads
// the ledger directly so the picture reflects the current time, not just whatever
// the last drill run recorded.
func renderDrillLedger(w io.Writer, cfg *config.Config, now time.Time) {
	ledger, err := drill.Load(cfg.WorkdirPath())
	if err != nil || len(ledger.Records) == 0 {
		return
	}
	window := cfg.DrillWindow()

	var dleNames []string
	for _, d := range cfg.DLEs() {
		dleNames = append(dleNames, d.Name())
	}
	never, _ := ledger.Coverage(dleNames, window, now)

	var failing, stale []drill.Record
	for _, rec := range ledger.Sorted() {
		switch {
		case !rec.OK:
			failing = append(failing, rec)
		case now.Sub(rec.LastDrill) >= window:
			stale = append(stale, rec)
		}
	}
	if len(failing) == 0 && len(stale) == 0 && len(never) == 0 {
		fmt.Fprintf(w, "\nDrill coverage: all %d drilled DLE(s) passing and current.\n", len(ledger.Records))
		return
	}

	fmt.Fprintln(w, "\nDRILL COVERAGE")
	if len(failing) > 0 {
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  FAILING DLE\tCLASS\tLAST DRILL\tREMEDY")
		for _, r := range failing {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.DLE, r.Class, drillWhen(r.LastDrill), drill.ParseClass(r.Class).Remedy())
		}
		tw.Flush()
	}
	if len(stale) > 0 {
		names := make([]string, 0, len(stale))
		for _, r := range stale {
			names = append(names, fmt.Sprintf("%s (%s ago)", r.DLE, ageString(now.Sub(r.LastDrill))))
		}
		fmt.Fprintf(w, "  stale (overdue past %s): %s\n", humanDur(window), strings.Join(names, ", "))
	}
	if len(never) > 0 {
		sort.Strings(never)
		fmt.Fprintf(w, "  never drilled: %s\n", strings.Join(never, ", "))
	}
}

// drillWhen renders a ledger timestamp, or a dash for the never-drilled zero value.
func drillWhen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// ageString renders a duration as whole days when it is at least a day, else hours.
func ageString(d time.Duration) string {
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// runReported executes a run-producing command body, records its outcome to the run
// history, and (Phase 3) dispatches notifications — in one place, so each command
// neither re-implements recording nor risks letting it change the exit code.
//
// build does the actual work and returns a partially-populated report.Run (its
// command-specific fields) plus the run's error. runReported stamps the timing and
// outcome, persists the record, fires notifications, and then returns build's error
// verbatim: a failure to write the summary or send a notification is a stderr
// warning, never the cause — nor a suppressor — of the run's own exit code (the
// progress.NewFileSink contract).
func (a *app) runReported(cfg *config.Config, seed report.Run, build func() (report.Run, error)) error {
	start := time.Now().UTC()
	rec, runErr := build()
	if rec.Command == "" {
		rec.Command = seed.Command
	}
	rec.StartedAt, rec.EndedAt = start, time.Now().UTC()
	if runErr != nil {
		rec.Outcome = report.OutcomeFailure
		rec.Error = runErr.Error()
		// Prefer a class the body's record already set, else the seed's
		// command-specific class (for a body that failed early with a zero record),
		// else a generic fallback.
		switch {
		case rec.ExitClass != "":
		case seed.ExitClass != "":
			rec.ExitClass = seed.ExitClass
		default:
			rec.ExitClass = "error"
		}
	} else {
		// A successful run carries no exit class — the seed's failure class must never
		// leak onto a passing record.
		rec.Outcome = report.OutcomeSuccess
		rec.ExitClass = ""
	}

	if err := report.Append(cfg.WorkdirPath(), rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: write run summary: %v\n", err)
	}
	a.dispatchNotify(cfg, rec)
	return runErr
}
