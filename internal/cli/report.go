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
	var asJSON, notify, dump bool
	var slotID string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Summarize recent runs, or print one dump's per-DLE report",
		Long: "Render a digest of recent runs from the run history every dump/sync/verify/drill/prune " +
			"writes: a per-run table, a failure summary, and a recovery-health section flagging DLEs whose " +
			"drills are failing, degrading (passed before, failing now), stale, or never run. With --dump it " +
			"instead prints an Amanda-style per-DLE report for one dump (the latest, or --slot <id>): each DLE's " +
			"level, original/output size, compression %, files, dump time, and rate, with full/incremental " +
			"totals. Reads only — no engine, no lock — so it is cheap to run from cron. With --notify it sends " +
			"the digest through the config's `notify.digest` backends (e.g. a nightly email); with --json it " +
			"emits the raw run records.",
		Example: "  nb report\n  nb report --last 30\n  nb report --dump\n  nb report --dump --slot slot-2026-06-21\n  nb dump && nb sync && nb drill --unattended; nb report --notify",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadRO()
			if err != nil {
				return err
			}
			dir := cfg.WorkdirPath()
			// --slot implies the per-dump report.
			if slotID != "" {
				dump = true
			}
			if dump {
				return runDumpReport(dir, slotID, asJSON)
			}
			runs, err := report.Last(dir, last)
			if err != nil {
				return err
			}
			if asJSON {
				return encodeJSON(runs)
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
	cmd.Flags().BoolVar(&dump, "dump", false, "print the per-DLE dump report for the latest dump (Amanda-style)")
	cmd.Flags().StringVar(&slotID, "slot", "", "with --dump, report on this slot id instead of the latest")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw run records as JSON instead of a text report")
	cmd.Flags().BoolVar(&notify, "notify", false, "also send the digest through the config's notify.digest backends")
	return cmd
}

// runDumpReport prints the Amanda-style per-DLE report for one dump from the run
// history: the latest dump when slotID is empty, else the named slot. The per-DLE
// timing it shows is only in the run history (not the seal), so a slot that predates
// the history — or was compacted out — points the operator at `nb slot show`.
func runDumpReport(dir, slotID string, asJSON bool) error {
	runs, err := report.Load(dir)
	if err != nil {
		return err
	}
	var target *report.Run
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].Command != report.CommandDump {
			continue
		}
		if slotID == "" || runs[i].SlotID == slotID {
			target = &runs[i]
			break
		}
	}
	if target == nil {
		if slotID != "" {
			return fmt.Errorf("no dump report for slot %q in the run history (try `nb slot show %s` for its sizes)", slotID, slotID)
		}
		return fmt.Errorf("no dump recorded yet")
	}
	if asJSON {
		return encodeJSON(target)
	}
	report.RenderDump(os.Stdout, *target)
	return nil
}

// encodeJSON writes v as indented JSON to stdout.
func encodeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
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

	// The ledger keys DLEs by their internal slug; display them as host:path.
	idOf := map[string]string{}
	var dleNames []string
	for _, d := range cfg.DLEs() {
		dleNames = append(dleNames, d.Name())
		idOf[d.Name()] = d.ID()
	}
	disp := func(slug string) string {
		if id, ok := idOf[slug]; ok {
			return id
		}
		return slug
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
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", disp(r.DLE), r.Class, drillWhen(r.LastDrill), drill.ParseClass(r.Class).Remedy())
		}
		tw.Flush()
	}
	if len(stale) > 0 {
		names := make([]string, 0, len(stale))
		for _, r := range stale {
			names = append(names, fmt.Sprintf("%s (%s ago)", disp(r.DLE), ageString(now.Sub(r.LastDrill))))
		}
		fmt.Fprintf(w, "  stale (overdue past %s): %s\n", humanDur(window), strings.Join(names, ", "))
	}
	if len(never) > 0 {
		neverIDs := make([]string, len(never))
		for i, n := range never {
			neverIDs[i] = disp(n)
		}
		sort.Strings(neverIDs)
		fmt.Fprintf(w, "  never drilled: %s\n", strings.Join(neverIDs, ", "))
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
