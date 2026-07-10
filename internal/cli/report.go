package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/report"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// newReportCmd implements `nb report`: the run digest. It reads the run history
// (run-log.jsonl) and the drill ledger (drill-ledger.json) the other commands write
// and renders a summary — what ran, what failed, and which
// DLEs' recovery health is degrading or stale. It needs no engine and takes no lock
// (read-only, like `nb status`), so it is cheap to run from cron after the nightly
// `nb dump && nb sync && nb drill`. With --notify it also sends the digest through
// the configured `digest:` notification backends.
func newReportCmd(a *app) *cobra.Command {
	var last int
	var asJSON, notify, dump bool
	var runID string
	cmd := &cobra.Command{
		Use:   "report [run-id]",
		Short: "Summarize recent runs, or print one dump's per-DLE report",
		Long: "Render a digest of recent runs from the run history every dump/sync/verify/drill/prune " +
			"writes: a per-run table, a failure summary, and a recovery-health section flagging DLEs whose " +
			"drills are failing, degrading (passed before, failing now), stale, or never run. With --dump it " +
			"instead prints a per-DLE report for one dump (the latest, or a run id — positional or --run): each DLE's " +
			"level, original/output size, compression %, files, dump time, and rate, with full/incremental " +
			"totals. Reads only — no engine, no lock — so it is cheap to run from cron. With --notify it sends " +
			"the digest through the config's `notify.digest` backends (e.g. a nightly email); with --json it " +
			"emits the raw run records.",
		Example: "  nb report\n  nb report --last 30\n  nb report --dump\n  nb report --dump run-2026-06-21.020000\n  nb dump && nb sync && nb drill --unattended; nb report --notify",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadOrDefaultCatalog()
			if err != nil {
				return err
			}
			dir := cfg.WorkdirPath()
			// A positional run id is the inspection-noun form (`nb run <id>`, `nb verify <id>`),
			// equivalent to --run; reject two ids that disagree rather than guessing.
			if len(args) == 1 {
				if runID != "" && runID != args[0] {
					return fmt.Errorf("two different run ids given: %q (positional) and %q (--run); pass one", args[0], runID)
				}
				runID = args[0]
			}
			// A run id implies the per-dump report.
			if runID != "" {
				dump = true
			}
			if dump {
				return runDumpReport(dir, runID, asJSON)
			}
			runs, err := report.Last(dir, last)
			if err != nil {
				return err
			}
			if asJSON {
				stale, _ := staleDLEs(cfg, time.Now())
				return encodeJSON(struct {
					Runs  []report.Run       `json:"runs"`
					Stale []catalog.StaleDLE `json:"stale,omitempty"`
				}{runs, stale})
			}
			report.Render(os.Stdout, runs, time.Now())
			renderDrillLedger(os.Stdout, cfg, time.Now())
			renderStaleness(os.Stdout, cfg, time.Now())
			if notify {
				a.dispatchDigest(cfg, runs)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&last, "last", 10, "summarize the last N runs (0 = all)")
	cmd.Flags().BoolVar(&dump, "dump", false, "print the per-DLE dump report for the latest dump")
	cmd.Flags().StringVar(&runID, "run", "", "report on this run id instead of the latest (same as passing it positionally)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw run records as JSON instead of a text report")
	cmd.Flags().BoolVar(&notify, "notify", false, "also send the digest through the config's notify.digest backends")
	return cmd
}

// runDumpReport prints the per-DLE report for one dump from the run
// history: the latest dump when runID is empty, else the named run. The per-DLE
// timing it shows is only in the run history (not the seal), so a run that predates
// the history — or was compacted out — points the operator at `nb run <id>`.
func runDumpReport(dir, runID string, asJSON bool) error {
	runs, err := report.Load(dir)
	if err != nil {
		return err
	}
	var target *report.Run
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].Command != report.CommandDump {
			continue
		}
		if runID == "" || runs[i].RunID == runID {
			target = &runs[i]
			break
		}
	}
	if target == nil {
		if runID != "" {
			return fmt.Errorf("no dump report for run %q in the run history (try `nb run %s` for its sizes)", runID, runID)
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
	// Failing DLEs render as blocks, not table rows: the recorded error is the fact
	// an operator diagnoses from, and it is too long for a column.
	for _, r := range failing {
		name := disp(r.DLE)
		fmt.Fprintf(w, "  FAILING %s [%s] — last drill %s (tier %s on %q)\n", name, r.Class, drillWhen(r.LastDrill), r.Tier, r.Medium)
		if r.Detail != "" {
			fmt.Fprintf(w, "    error:  %s\n", r.Detail)
		}
		fmt.Fprintf(w, "    remedy: %s\n", drill.ParseClass(r.Class).Remedy())
		fmt.Fprintf(w, "    retry:  `nb drill %s` — a pass clears this warning; `nb verify --dle %s --deep` cross-checks every copy\n", name, name)
	}
	if len(stale) > 0 {
		names := make([]string, 0, len(stale))
		for _, r := range stale {
			names = append(names, fmt.Sprintf("%s (%s ago)", disp(r.DLE), sizeutil.FormatDaysHours(now.Sub(r.LastDrill))))
		}
		fmt.Fprintf(w, "  stale (overdue past %s): %s\n", sizeutil.FormatDuration(window), strings.Join(names, ", "))
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
	return sizeutil.FormatStamp(t.Local())
}

// staleDLEs computes the staleness alert for the report/digest/--json surfaces: the
// configured DLEs whose newest backup (at any level) predates one dump cycle — the
// documented promise that "a full never ages past one cycle", so an older backup
// has provably broken it, independent of cron cadence — or that have never been
// backed up at all. It reads the catalog cache directly (like renderDrillLedger
// reads the drill ledger directly) rather than building an engine, so `nb report`
// stays lock-free and cheap. Always on: there is no config key to disable it.
func staleDLEs(cfg *config.Config, now time.Time) (stale []catalog.StaleDLE, window time.Duration) {
	window = cfg.CycleDuration()
	cat, err := catalog.Open(cfg.WorkdirPath())
	if err != nil {
		return nil, window
	}
	return engine.StaleConfiguredDLEs(cfg, cat, window, now), window
}

// renderStaleness prints the staleness section: DLEs older than one dump cycle (or
// never backed up), each with how long since their last backup (or "never"). It
// always runs — there is no config key to disable it — and prints an all-clear
// line when nothing is overdue.
func renderStaleness(w io.Writer, cfg *config.Config, now time.Time) {
	stale, window := staleDLEs(cfg, now)
	if len(stale) == 0 {
		fmt.Fprintf(w, "\nStaleness: all configured DLE(s) backed up within one cycle (%s).\n", sizeutil.FormatDuration(window))
		return
	}
	fmt.Fprintf(w, "\nSTALE DLEs (older than one cycle, %s)\n", sizeutil.FormatDuration(window))
	tw := newTab(w)
	fmt.Fprintln(tw, "  DLE\tLAST BACKUP")
	for _, s := range stale {
		last := "never"
		if !s.LastBackup.IsZero() {
			last = sizeutil.FormatDaysHours(now.Sub(s.LastBackup)) + " ago"
		}
		fmt.Fprintf(tw, "  %s\t%s\n", s.Display, last)
	}
	tw.Flush()
}
