package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/report"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// newPlanCmd implements `nb plan`: show what the next run would do, or — with
// --days — an extended forecast of running daily for that many days.
func newPlanCmd(a *app) *cobra.Command {
	var dateStr string
	var days int
	cmd := &cobra.Command{
		Use:     "plan",
		Short:   "Show what the next run would do",
		Long:    "Preview the next run: which DLEs would be dumped at which level, estimated sizes, and capacity/budget status. With --days N, forecast N consecutive daily runs instead, projecting the schedule forward day-by-day (when fulls land, how incrementals climb). Reads only; nothing is written.",
		Example: "  nb plan\n  nb plan --date 2026-06-21\n  nb plan --days 30",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if days < 1 {
				return fmt.Errorf("--days must be at least 1")
			}
			cfg, err := a.load()
			if err != nil {
				return err
			}
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			date, err := ParseDate(dateStr)
			if err != nil {
				return err
			}
			if err := errPastPlan(date); err != nil {
				return err
			}
			warnings, err := eng.ValidatePlan()
			if err != nil {
				return err
			}
			for _, w := range warnings {
				fmt.Printf("WARNING: %s\n", w)
			}
			if len(warnings) > 0 {
				fmt.Println()
			}
			if days > 1 {
				return runPlanForecast(eng, date, days)
			}

			plan := eng.PlanWithProgress(date, estimateProgress(a.quiet))
			fmt.Printf("Plan for run %s  (cycle %dd, landing %q)\n\n",
				record.DateString(date), plan.Interval, eng.Landing())
			for _, w := range plan.Warnings {
				fmt.Printf("WARNING: %s\n", w)
			}
			if len(plan.Warnings) > 0 {
				fmt.Println()
			}

			estTotal := fprintPlanItems(os.Stdout, plan)

			current := eng.StoredBytes()
			capacity := eng.Capacity()
			fmt.Printf("\nCatalog currently stored: %s\n", sizeutil.FormatBytes(current))
			fmt.Printf("This run (estimated): ~%s\n", sizeutil.FormatBytes(estTotal))
			if capacity > 0 {
				over, pct := eng.CapacityStatus(current)
				fmt.Printf("Capacity: %s (%.1f%% used)\n", sizeutil.FormatBytes(capacity), pct)
				if over {
					fmt.Printf("WARNING: over capacity; run `nb prune` to reclaim oldest runs\n")
				} else if current+estTotal > capacity {
					// "% used" counts only what is already stored, so a run far larger
					// than capacity can still show a small percentage — call out that
					// this run would not fit rather than leaving the reader to compare.
					fmt.Printf("WARNING: this run (~%s) would exceed the %s capacity — grow capacity or lengthen the cycle\n",
						sizeutil.FormatBytes(estTotal), sizeutil.FormatBytes(capacity))
				}
			} else {
				fmt.Printf("Capacity: unbounded\n")
			}
			if cs := eng.CostSummary(plan); cs.Priced {
				fmt.Printf("Est. storage cost (%s): %s/month for %s stored\n",
					cs.Provider, formatUSD(cs.Monthly), sizeutil.FormatBytes(cs.Bytes))
				fmt.Printf("This run adds: ~%s/month (%s)\n",
					formatUSD(cs.Marginal), sizeutil.FormatBytes(cs.RunBytes))
			}
			if exp, ok := eng.ExpectedVolume(date); ok {
				fmt.Printf("Tape: %s\n", describeExpectation(exp, date))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today); planning a date behind the latest committed run may show a full, since incremental state reflects the most recent dump")
	cmd.Flags().IntVar(&days, "days", 1, "forecast this many consecutive daily runs from --date")
	return cmd
}

// fprintPlanItems writes a plan's per-DLE level/size/reason table to w and returns
// the total estimated bytes. Shared by `nb plan` and the `nb dump --dry-run`
// preview so a single run renders identically in both.
func fprintPlanItems(w io.Writer, plan *planner.Plan) int64 {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DLE\tLEVEL\tEST. SIZE\tFULL SIZE\tREASON")
	var estTotal int64
	for _, item := range plan.Items {
		levelStr := fmt.Sprintf("L%d (full)", item.Level)
		// For an incremental, show the full-dump size alongside the chosen size so a
		// small incremental does not hide a large full waiting at the cycle deadline.
		// For a full the two are identical, so leave the column blank to avoid noise.
		fullStr := "-"
		if item.Level >= 1 {
			levelStr = fmt.Sprintf("L%d (incr)", item.Level)
			fullStr = "~" + sizeutil.FormatBytes(item.FullBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t~%s\t%s\t%s\n", item.DLE.ID(), levelStr, sizeutil.FormatBytes(item.EstBytes), fullStr, item.Reason)
		estTotal += item.EstBytes
	}
	tw.Flush()
	return estTotal
}

// runPlanForecast renders an extended plan: one row per simulated daily run,
// projecting the level schedule forward. Estimates are sampled once and held
// constant (see engine.Simulate), so the per-day size tracks the chosen levels.
func runPlanForecast(eng *engine.Engine, start time.Time, days int) error {
	plans := eng.Simulate(start, days)
	fmt.Printf("Forecast: %d daily runs from %s  (cycle %dd, landing %q)\n\n",
		days, record.DateString(start), plans[0].Interval, eng.Landing())

	// Structural warnings (e.g. a recovery set that won't fit capacity) are
	// constant across the window; surface each one once, above the schedule.
	seen := map[string]bool{}
	for _, p := range plans {
		for _, w := range p.Warnings {
			if !seen[w] {
				seen[w] = true
				fmt.Printf("WARNING: %s\n", w)
			}
		}
	}
	if len(seen) > 0 {
		fmt.Println()
	}

	// The cost curve overlays the schedule: the projected $/month footprint at the
	// end of each day as runs land and pruning reclaims. Only shown for a priced
	// (cloud) landing medium; a local disk has no recurring bill.
	curve := eng.ForecastCost(start, days)
	priced := eng.CostSummary(nil).Priced

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if priced {
		fmt.Fprintln(tw, "DATE\tFULL\tINCR\tEST. SIZE\t$/MONTH\tFULLS")
	} else {
		fmt.Fprintln(tw, "DATE\tFULL\tINCR\tEST. SIZE\tFULLS")
	}
	var windowTotal int64
	var totalIncrs int
	for i, p := range plans {
		var fulls, incrs int
		var est int64
		var fullNames []string
		for _, it := range p.Items {
			est += it.EstBytes
			if it.Level == 0 {
				fulls++
				fullNames = append(fullNames, it.DLE.ID())
			} else {
				incrs++
			}
		}
		windowTotal += est
		totalIncrs += incrs
		names := strings.Join(fullNames, ", ")
		if names == "" {
			names = "-"
		}
		if priced {
			fmt.Fprintf(tw, "%s\t%d\t%d\t~%s\t%s\t%s\n",
				record.DateString(p.Date), fulls, incrs, sizeutil.FormatBytes(est),
				formatUSD(curve[i].Monthly), names)
		} else {
			fmt.Fprintf(tw, "%s\t%d\t%d\t~%s\t%s\n",
				record.DateString(p.Date), fulls, incrs, sizeutil.FormatBytes(est), names)
		}
	}
	tw.Flush()
	if totalIncrs > 0 {
		// The simulation replays the level schedule but never mutates the filesystem,
		// so no bytes change between simulated runs and every forecast incremental sizes
		// to 0 B. Say so, lest a reader read "~0 B" as a broken estimate; a real
		// incremental is a fraction of its full (shown in the FULL/INCR counts).
		fmt.Println("\nNote: a forecast incremental over a simulated full shows ~0 B because the simulation makes no filesystem changes between runs; a real incremental is a fraction of its full.")
	}
	fmt.Printf("\nWindow total (estimated): ~%s over %d run(s)\n", sizeutil.FormatBytes(windowTotal), days)
	if priced && len(curve) > 0 {
		last := curve[len(curve)-1]
		fmt.Printf("Projected storage cost at end of window: %s/month (%s stored)\n",
			formatUSD(last.Monthly), sizeutil.FormatBytes(last.Bytes))
	}
	return nil
}

// describeExpectation renders the tape the next run will write to for `nb plan`,
// with the volume's age relative to the run date.
func describeExpectation(exp engine.VolumeExpectation, date time.Time) string {
	if exp.Appendable {
		if exp.FreshVolume {
			return fmt.Sprintf("expects a fresh tape (pool %q is empty)", exp.Medium)
		}
		if exp.VolumeBytes > 0 {
			return fmt.Sprintf("appends to %q (%s of %s used, %s free on this reel)",
				exp.Label, sizeutil.FormatBytes(exp.UsedBytes), sizeutil.FormatBytes(exp.VolumeBytes),
				sizeutil.FormatBytes(exp.VolumeBytes-exp.UsedBytes))
		}
		return fmt.Sprintf("appends to %q", exp.Label)
	}
	if exp.FreshVolume {
		return fmt.Sprintf("expects a fresh tape (no reusable volume in pool %q)", exp.Medium)
	}
	age := int(date.Sub(exp.WrittenAt).Hours() / 24)
	detail := fmt.Sprintf("recycles %d aged run(s)", exp.Recycles)
	if exp.Recycles == 0 {
		detail = "empty, ready to write"
	}
	return fmt.Sprintf("expects %q (labeled %s, %dd ago; %s) — or a fresh tape",
		exp.Label, record.DateString(exp.WrittenAt), age, detail)
}

// newDumpCmd implements `nb dump`: execute a run and seal a run, or — with
// --dry-run — plan the run for --date and print it without writing anything.
func newDumpCmd(a *app) *cobra.Command {
	var dateStr string
	var dryRun bool
	cmd := &cobra.Command{
		Use:     "dump",
		Short:   "Execute a run and commit its archives",
		Long:    "Execute a planner run, dumping each scheduled DLE and committing exactly one immutable run's archives. Use --quiet to suppress progress output. --date sets the run date (with or without --dry-run). With --dry-run the run for --date is planned against the current catalog and printed, exactly as a real dump would decide it, but nothing is written.",
		Example: "  nb dump\n  nb dump --date 2026-06-21\n  nb dump --dry-run --date 2026-07-15\n  nb -c prod.yaml dump -q",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			date, err := ParseDate(dateStr)
			if err != nil {
				return err
			}
			// The run's single time source: the instant it is stamped committed, the
			// moment retention is judged against, and the wall clock its id is minted
			// from. An explicit --date pins the instant to that date's midnight — a
			// coarse but reproducible override; the run date (used for guards and
			// planning) is only the instant's day.
			now := date
			if dateStr == "" {
				now = time.Now().UTC()
			}
			if dryRun {
				if err := errPastPlan(date); err != nil {
					return err
				}
				eng, err := newEngine(cfg)
				if err != nil {
					return err
				}
				warnings, err := eng.ValidatePlan()
				if err != nil {
					return err
				}
				return runDumpDryRun(eng, date, now, warnings)
			}
			if err := errPastDump(date); err != nil {
				return err
			}
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			attachOperator(eng)
			eng.SetEstimateProgress(estimateProgress(a.quiet))
			eng.SetRunProgress(runProgress(a.quiet))
			return a.runReported(cfg, report.Run{Command: report.CommandDump, ExitClass: "dump-failed"}, func() (report.Run, error) {
				s, err := eng.Run(cmd.Context(), now, a.logf())
				if err != nil {
					// A canceled run is operator-initiated, not a dump failure: record it under its
					// own exit class so the run log distinguishes the two.
					if errors.Is(err, conductor.ErrCanceled) {
						return report.Run{Command: report.CommandDump, ExitClass: "canceled"}, err
					}
					// A failed run may still have committed archives (a partial dump commits a
					// valid archive of what was readable). Record what landed — run id and per-DLE
					// stats — so `nb report --dump --run <id>` finds the run in the history.
					rec := report.Run{Command: report.CommandDump}
					if s != nil && len(s.Archives) > 0 {
						rec.RunID = s.ID
						rec.Archives = len(s.Archives)
						rec.BytesMoved = s.TotalBytes()
						rec.DumpStats = dumpStats(s, cfg.WorkdirPath())
					}
					return rec, err
				}
				// The blank line separates the commit line from the progress stream
				// above it; --quiet printed no stream, so don't lead with one.
				sep := "\n"
				if a.quiet {
					sep = ""
				}
				fmt.Printf(sep+"Committed %s: %d archive(s), %s total\n", s.ID, len(s.Archives), sizeutil.FormatBytes(s.TotalBytes()))
				return report.Run{
					Command:    report.CommandDump,
					RunID:      s.ID,
					Archives:   len(s.Archives),
					BytesMoved: s.TotalBytes(),
					DumpStats:  dumpStats(s, cfg.WorkdirPath()),
				}, nil
			})
		},
	}
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today); a --dry-run for a date behind the latest committed run may show a full, since incremental state reflects the most recent dump")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "plan the run for --date and print it without writing anything")
	return cmd
}

// dumpStats builds the per-DLE statistics for a sealed run's record: sizes,
// level, and files come from the seal (authoritative); the dump duration comes from
// the run-status snapshot the tracker just flushed (the same file `nb status` reads),
// matched by DLE name and level. When the snapshot is missing or stale, sizes are
// still recorded and timing is left zero (rendered as a dash).
func dumpStats(s *catalog.Run, workdir string) []report.DLEStat {
	type key struct {
		name  string
		level int
	}
	durations := map[key]float64{}
	if snap, err := progress.Load(workdir); err == nil && snap.RunID == s.ID {
		for _, d := range snap.DLEs {
			if !d.StartedAt.IsZero() && !d.EndedAt.IsZero() {
				durations[key{d.Name, d.Level}] = d.EndedAt.Sub(d.StartedAt).Seconds()
			}
		}
	}
	stats := make([]report.DLEStat, 0, len(s.Archives))
	for _, a := range s.Archives {
		stats = append(stats, report.DLEStat{
			DLE:     a.DLE,
			Host:    a.Host,
			Path:    a.Path,
			Level:   a.Level,
			Orig:    a.Uncompressed,
			Out:     a.Compressed,
			Files:   a.FileCount,
			Seconds: durations[key{a.DLEID(), a.Level}], // progress is keyed by host:path
		})
	}
	return stats
}

// runDumpDryRun previews the dump on `date` without writing: it plans that run
// exactly as `nb dump --date <date>` would — against the current catalog, the same
// decision logic a real run uses — and prints it. Nothing is sealed. `now` is the
// instant the run id is minted from (see newDumpCmd); a real run started later
// would carry a later time suffix.
func runDumpDryRun(eng *engine.Engine, date, now time.Time, validationWarnings []string) error {
	plan := eng.Plan(date)

	fmt.Println("DRY RUN — no data is written.")
	fmt.Printf("This is the run on %s.\n\n", record.DateString(date))
	warnings := append(validationWarnings, plan.Warnings...)
	for _, w := range warnings {
		fmt.Printf("WARNING: %s\n", w)
	}
	if len(warnings) > 0 {
		fmt.Println()
	}

	estTotal := fprintPlanItems(os.Stdout, plan)
	fmt.Printf("\nThis run (estimated): ~%s\n", sizeutil.FormatBytes(estTotal))
	fmt.Printf("Would commit %s. Run without --dry-run to execute.\n", eng.PlannedRunID(now))
	return nil
}

// newStatusCmd implements `nb status`: show the progress of the current (or most
// recent) run by reading the run-status file `nb dump` writes. It needs no
// engine, only the catalog workdir, so it is cheap to poll.
func newStatusCmd(a *app) *cobra.Command {
	var watch time.Duration
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "Show the progress of the current or most recent run",
		Long:    "Read the run-status file `nb dump` maintains and render a progress report: each DLE's state and percent of estimate, totals, throughput, and ETA. With --watch it refreshes on an interval. Reflects an in-flight run, or the last finished one.",
		Example: "  nb status\n  nb status --watch 2s",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Like plan/dump/check, a missing config is a hard error: a synthesized
			// default catalog would report "no run in progress" (exit 0) from a
			// directory nothing ever dumps to — reading as "backups idle" to a
			// monitor. --catalog still points status at an existing catalog directly.
			cfg, err := a.loadRORequire()
			if err != nil {
				return err
			}
			dir := cfg.WorkdirPath()
			if watch <= 0 {
				return renderStatus(dir)
			}
			for {
				fmt.Print("\033[H\033[2J") // home cursor + clear screen
				if err := renderStatus(dir); err != nil {
					return err
				}
				snap, err := progress.Load(dir)
				if err == nil && snap.Phase.Terminal() {
					return nil // run finished; stop watching
				}
				// Watching is a read-only poll, so Ctrl-C just quits the viewer: exit cleanly
				// (nil, no "canceled" notice — there's nothing in flight to cancel) rather than
				// sleeping out the interval first.
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(watch):
				}
			}
		},
	}
	cmd.Flags().DurationVar(&watch, "watch", 0, "refresh every interval (e.g. 2s) until the run finishes")
	return cmd
}

// renderStatus loads and prints one run-status snapshot, or a friendly note when
// no run has written one yet.
func renderStatus(dir string) error {
	snap, err := progress.Load(dir)
	if progress.IsNotExist(err) {
		fmt.Println("no run in progress (no status recorded yet)")
		return nil
	}
	if err != nil {
		return err
	}
	progress.Render(os.Stdout, snap, time.Now())
	return nil
}

// newVerifyCmd implements `nb verify`: check archive checksums of named runs, or
// every run with --all. Verifying all runs can mount every volume in the pool
// (each tape in turn), so the whole-pool scan is gated behind an explicit flag
// rather than triggered by a bare `nb verify`.
func newVerifyCmd(a *app) *cobra.Command {
	var all, deep bool
	cmd := &cobra.Command{
		Use:   "verify [run-id...]",
		Short: "Verify run integrity (checksum, or --deep structural)",
		Long: "Verify archives against their commit footers. By default it re-checks payload checksums " +
			"(integrity). With --deep it also streams each archive through the real read " +
			"pipeline — decrypt, decompress, then `tar -t` (list, not extract) — and asserts the " +
			"members match the recorded index, proving the bytes are a valid restorable stream and " +
			"exercising the key and compression end-to-end. It writes nothing either way. Pass run ids " +
			"to verify just those; with no ids it verifies every run (which may mount every volume in the pool).",
		Example: "  nb verify run-2026-06-21.001\n  nb verify --deep run-2026-06-21.001\n  nb verify",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return fmt.Errorf("--all cannot be combined with explicit run ids")
			}
			// Bare `nb verify` (no run ids) verifies the whole catalog — the obvious
			// reading of "verify my backups". --all stays as an explicit synonym.
			if len(args) == 0 {
				all = true
			}
			// verify is an assertion (monitors gate on its exit code), so a missing
			// config is an error — not a green "0 run(s) verified".
			cfg, err := a.loadRORequire()
			if err != nil {
				return err
			}
			// Verify mounts media, so it takes the config lock like any medium-accessing
			// command — a verify mid-dump would fight the run for drives and the robot.
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			// Verifying reads media, so a spanned run on a single-drive station needs
			// reel swaps — give it the operator so it prompts (and reassembles a spanned
			// run) just like restore, rather than failing at the first volume boundary.
			attachOperator(eng)
			if all && !a.quiet {
				mode := "checksum"
				if deep {
					mode = "deep (checksum + structural)"
				}
				fmt.Printf("verifying %d run(s) in the catalog [%s]\n", len(eng.Catalog().Runs()), mode)
			}
			checks := engine.CheckChecksum
			if deep {
				checks |= engine.CheckStructural
			}
			return a.runReported(cfg, report.Run{Command: report.CommandVerify, ExitClass: "verify-failures"}, func() (report.Run, error) {
				vr, err := eng.Verify(args, engine.VerifyOptions{Checks: checks}, a.logf())
				if err != nil {
					return report.Run{}, err
				}
				rec := report.Run{Command: report.CommandVerify, Failures: vr.Failures}
				if vr.Failures > 0 {
					return rec, fmt.Errorf("%d run(s) failed verification", vr.Failures)
				}
				return rec, nil
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "verify every run in the catalog (the default when no run ids are given)")
	cmd.Flags().BoolVar(&deep, "deep", false, "also validate structure: decrypt+decompress+tar-list, members vs the recorded index")
	return cmd
}

// newRunCmd implements `nb run`: list runs, or — with a run id — detail one.
// Inspection follows the bare-noun convention (like `nb medium`, `nb dle`): no
// arg lists, a positional arg details that one. (Reclaim runs with `nb prune`.)
func newRunCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "run [run-id]",
		Short:   "List runs, or detail one",
		Long:    "Inspect the run catalog. With no argument it lists runs; pass a run id to show that run's archives and copies. (Reclaim runs with `nb prune`.)",
		Example: "  nb run\n  nb run run-2026-06-21.001",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runRunShow(a, args[0])
			}
			return runRunList(a)
		},
	}
}

func runRunList(a *app) error {
	cfg, err := a.loadRO()
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	runs := eng.Catalog().Runs()
	if len(runs) == 0 {
		// A bare config (no sources) means no backup config was found — read-only
		// commands fall back to the default local catalog, so distinguish "configured
		// but nothing dumped yet" from "no config at all" to spare a newcomer the
		// false impression that an unconfigured `nb run` succeeded meaningfully.
		if len(cfg.Sources) == 0 {
			fmt.Println("no runs in catalog (no backup config found — copy nbackup.example.yaml to nbackup.yaml and run `nb dump`, or pass -c <config>)")
		} else {
			fmt.Println("no runs in catalog")
		}
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATUS\tARCHIVES\tSIZE\tCOMMITTED\tCOPIES")
	for _, s := range runs {
		committed := "-"
		if t := s.LastArchiveAt(); !t.IsZero() {
			committed = t.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", s.ID, runStatus(s), len(s.Archives),
			sizeutil.FormatBytes(s.TotalBytes()), committed, copiesSummary(eng.Catalog().Placements(s.ID)))
	}
	tw.Flush()
	return nil
}

// runStatus renders a run's status cell: every cataloged run is committed (the archive
// is the commit unit), with a partial marker when any archive omitted unreadable files.
func runStatus(s *catalog.Run) string {
	if s.Partial() {
		return "committed (partial)"
	}
	return "committed"
}

// copiesSummary renders a run's placements as a compact comma list, naming the
// volume label only when it differs from the medium (i.e. for labeled tapes).
func copiesSummary(ps []catalog.Placement) string {
	if len(ps) == 0 {
		return "-"
	}
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		if labels := p.Labels(); len(labels) > 0 {
			names = append(names, p.Medium+":"+strings.Join(labels, "+"))
		} else {
			names = append(names, p.Medium)
		}
	}
	return strings.Join(names, ", ")
}

func runRunShow(a *app, runID string) error {
	cfg, err := a.loadRO()
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	s, err := eng.Catalog().ReadRun(runID)
	if err != nil {
		// `nb run list` (and friends) parse the word as a run id; a non-run-id argument
		// that isn't found is almost always a user reaching for a subcommand that does not
		// exist — point them at the bare list rather than at `nb rebuild`.
		if !strings.HasPrefix(runID, "run-") {
			return fmt.Errorf("%w (to list all runs, run `nb run` with no argument)", err)
		}
		return err
	}
	fmt.Printf("Run %s  (%s)\n", s.ID, runStatus(s))
	fmt.Printf("  date:    %s\n", s.Date())
	fmt.Printf("  committed: %s\n", s.LastArchiveAt().Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("  total:   %s\n\n", sizeutil.FormatBytes(s.TotalBytes()))
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DLE\tLEVEL\tFILES\tSIZE\tCOMPRESS\tENCRYPT")
	for _, ar := range s.Archives {
		enc := ar.Encrypt
		if enc == "" {
			enc = "none"
		}
		// A PARTIAL archive committed a valid backup of what was readable but omitted
		// unreadable source files — flag it so the gap is visible after the fact.
		partial := ""
		if ar.Partial() {
			partial = fmt.Sprintf("\tPARTIAL (%d file(s) unreadable, omitted)", ar.Unreadable)
		}
		fmt.Fprintf(tw, "%s\tL%d\t%d\t%s\t%s\t%s%s\n", ar.DLEID(), ar.Level, ar.FileCount, sizeutil.FormatBytes(ar.Compressed), ar.Compress, enc, partial)
	}
	tw.Flush()

	placements := eng.Catalog().Placements(s.ID)
	fmt.Printf("\nCOPIES (%d)\n", len(placements))
	// One row per segment (each data part, then the commit footer) rather than one
	// row per copy: a copy spanned across many volumes would otherwise pack every
	// volume name into one overflowing cell, and listing the data parts but not the
	// commit volume read like an off-by-one. A row per segment names where each piece
	// landed — volume + file number — so a spanned archive is legible and the commit
	// (written last, possibly on a later volume) is shown explicitly.
	ptw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(ptw, "  MEDIUM\tDLE\tLEVEL\tSEGMENT\tVOLUME\tFILE")
	spanned := false
	for _, p := range placements {
		for _, ar := range p.Archives {
			dle := eng.DisplayDLE(ar.DLE)
			level := fmt.Sprintf("L%d", ar.Level)
			n := len(ar.Parts)
			for i, pt := range ar.Parts {
				seg := "data"
				if n > 1 {
					seg = fmt.Sprintf("part %d/%d", i+1, n)
					spanned = true
				}
				fmt.Fprintf(ptw, "  %s\t%s\t%s\t%s\t%s\t%d\n", p.Medium, dle, level, seg, volumeOrDash(pt.Label), pt.Pos)
			}
			fmt.Fprintf(ptw, "  %s\t%s\t%s\t%s\t%s\t%d\n", p.Medium, dle, level, "commit", volumeOrDash(ar.Commit.Label), ar.Commit.Pos)
		}
	}
	ptw.Flush()
	fmt.Println("  FILE is the segment's sequential file number on VOLUME (VOLUME \"-\" = a label-less medium, e.g. disk/s3).")
	if spanned {
		fmt.Println("  A spanned archive lists each data part on its volume; the commit footer is written last and may land on a later volume.")
	}
	return nil
}

// volumeOrDash renders a volume label, or "-" for a label-less medium (disk/s3),
// where files are addressed within the single medium rather than by volume label.
func volumeOrDash(label string) string {
	if label == "" {
		return "-"
	}
	return label
}

// newDleCmd implements `nb dle`: inspect the catalog grouped by DLE (backup source)
// rather than by run. The same archives the run view groups by run, browsed instead
// by what was backed up — one row per DLE, then its archive timeline across runs.
func newDleCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "dle [dle]",
		Short:   "List DLEs (backup sources), or detail one",
		Long:    "Inspect the catalog grouped by DLE (a host:path backup source). With no argument it lists each DLE and its backup history; pass a DLE to show its archive timeline across runs. (Reclaim with `nb prune`, which is per-DLE on disk/cloud.)",
		Example: "  nb dle\n  nb dle localhost:/home",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runDleShow(a, args[0])
			}
			return runDleList(a)
		},
	}
}

func runDleList(a *app) error {
	cfg, err := a.loadRO()
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	runs := eng.Catalog().Runs()
	if len(runs) == 0 {
		if len(cfg.Sources) == 0 {
			fmt.Println("no DLEs in catalog (no backup config found — copy nbackup.example.yaml to nbackup.yaml and run `nb dump`, or pass -c <config>)")
		} else {
			fmt.Println("no DLEs in catalog")
		}
		return nil
	}

	// Aggregate per DLE across runs. Runs come in run order, so the last archive
	// seen for a DLE is its most recent run.
	type agg struct {
		display   string
		runs      int
		lastLevel int
		lastFull  string
		bytes     int64
		media     map[string]bool
	}
	aggs := map[string]*agg{}
	var order []string
	for _, s := range runs {
		ps := eng.Catalog().Placements(s.ID)
		for _, ar := range s.Archives {
			g := aggs[ar.DLE]
			if g == nil {
				g = &agg{display: ar.DLEID(), media: map[string]bool{}}
				aggs[ar.DLE] = g
				order = append(order, ar.DLE)
			}
			g.runs++
			g.bytes += ar.Compressed
			g.lastLevel = ar.Level
			if ar.Level == 0 {
				g.lastFull = s.Date()
			}
			for _, p := range ps {
				for _, pa := range p.Archives {
					if pa.DLE == ar.DLE {
						g.media[p.Medium] = true
					}
				}
			}
		}
	}
	sort.Slice(order, func(i, j int) bool { return aggs[order[i]].display < aggs[order[j]].display })

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DLE\tRUNS\tLAST FULL\tLAST\tSIZE\tCOPIES")
	for _, slug := range order {
		g := aggs[slug]
		lastFull := g.lastFull
		if lastFull == "" {
			lastFull = "never"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\tL%d\t%s\t%s\n", g.display, g.runs, lastFull, g.lastLevel,
			sizeutil.FormatBytes(g.bytes), strings.Join(sortedKeys(g.media), ", "))
	}
	tw.Flush()
	return nil
}

func runDleShow(a *app, arg string) error {
	cfg, err := a.loadRO()
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	runs := eng.Catalog().Runs()
	slug, display, ok := resolveDLE(runs, arg)
	if !ok {
		return fmt.Errorf("no DLE %q in catalog (list them with `nb dle`)", arg)
	}
	fmt.Printf("DLE %s\n\n", display)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tDATE\tLEVEL\tSIZE\tBASE\tCOPIES")
	for _, s := range runs {
		for _, ar := range s.Archives {
			if ar.DLE != slug {
				continue
			}
			base := ar.BaseRun
			if base == "" {
				base = "-"
			}
			var media []string
			for _, p := range eng.Catalog().Placements(s.ID) {
				for _, pa := range p.Archives {
					if pa.DLE == slug {
						media = append(media, p.Medium)
						break
					}
				}
			}
			sort.Strings(media)
			fmt.Fprintf(tw, "%s\t%s\tL%d\t%s\t%s\t%s\n", s.ID, s.Date(), ar.Level,
				sizeutil.FormatBytes(ar.Compressed), base, strings.Join(media, ", "))
		}
	}
	tw.Flush()
	return nil
}

// resolveDLE matches a user-typed DLE identifier against the catalog's archives,
// accepting either the internal slug or the host:path display id, and returns the
// slug plus a display string. Archives carry their own host/path, so the match needs
// no config — a DLE that was dumped but later removed from config still resolves.
func resolveDLE(runs []*catalog.Run, arg string) (slug, display string, ok bool) {
	for _, s := range runs {
		for _, ar := range s.Archives {
			if ar.DLE == arg || ar.DLEID() == arg {
				return ar.DLE, ar.DLEID(), true
			}
		}
	}
	return "", "", false
}

// sortedKeys returns a set's keys sorted, for a stable rendering.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// newPruneCmd implements `nb prune`: reclaim runs past the cycle/capacity limits.
func newPruneCmd(a *app) *cobra.Command {
	var dryRun bool
	var dateStr string
	cmd := &cobra.Command{
		Use:     "prune <medium>",
		Short:   "Delete a medium's runs past its cycle/capacity limits",
		Long:    "Reclaim runs on the named medium that fall outside its own cycle and capacity limits. Retention is per-medium, so the medium to prune must be named explicitly (pruning one store never touches a copy on another). Deletes by default; pass --dry-run (-n) to preview. On a per-file medium (disk, cloud) it also sweeps crash leftovers — footer-less or torn files an interrupted run left behind, which no archive references — detected from the medium's own commit footers and bounded by minimum_age (so it never fights WORM/Object-Lock). If the protected recovery set alone exceeds capacity, prune reclaims what it can, prints a WARNING, and still exits 0 (recoverability outranks capacity — grow capacity or lengthen the cycle); watch for it via `nb report`/notify rather than the exit code.",
		Example: "  nb prune disk\n  nb prune disk --dry-run\n  nb prune offsite",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return nil
			}
			msg := "prune requires exactly one medium name, e.g. `nb prune disk` (retention is per-medium)"
			if cfg, err := a.load(); err == nil && len(cfg.Media) > 0 {
				names := make([]string, 0, len(cfg.Media))
				for name := range cfg.Media {
					names = append(names, name)
				}
				sort.Strings(names)
				msg += " — media: " + strings.Join(names, ", ")
			}
			return fmt.Errorf("%s", msg)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			// Dry-run prune only reads; a real run deletes runs, so lock it.
			eng, release, err := a.engineFor(cfg, !dryRun)
			if err != nil {
				return err
			}
			defer release()
			// Validate the medium name up front (before the run-reporting path): a typo'd
			// medium is an argument error, not a failed run — it must not land in the run log
			// or fire notify.on_failure.
			if _, ok := cfg.Media[args[0]]; !ok {
				names := make([]string, 0, len(cfg.Media))
				for name := range cfg.Media {
					names = append(names, name)
				}
				sort.Strings(names)
				return fmt.Errorf("unknown medium %q (configured: %s)", args[0], strings.Join(names, ", "))
			}
			// Retention measures age from each run's commit instant, so the
			// reference 'now' must be a real wall-clock time, not a date truncated
			// to midnight — otherwise a sub-day minimum_age can never elapse within
			// the run day. An explicit --date stays a coarse, reproducible override.
			now := time.Now().UTC()
			if dateStr != "" {
				var err error
				if now, err = ParseDate(dateStr); err != nil {
					return err
				}
			}
			if dryRun {
				eligible, swept, _, err := eng.Prune(args[0], now, false, a.logf())
				if err != nil {
					return err
				}
				if eligible > 0 {
					fmt.Printf("\n%d archive(s) eligible. Re-run without --dry-run to delete.\n", eligible)
				} else if swept == 0 {
					printNothingToReclaim(eng, args[0])
				}
				if swept > 0 {
					fmt.Printf("%d crash leftover(s) to sweep (orphaned by an interrupted run, no commit footer).\n", swept)
				}
				warnIfOverCapacity(eng, args[0], now)
				return nil
			}
			return a.runReported(cfg, report.Run{Command: report.CommandPrune, ExitClass: "prune-error"}, func() (report.Run, error) {
				eligible, swept, freed, err := eng.Prune(args[0], now, true, a.logf())
				if err != nil {
					return report.Run{}, err
				}
				if eligible > 0 {
					fmt.Printf("\n%s: deleted %d archive(s), freed %s\n", args[0], eligible, sizeutil.FormatBytes(freed))
				} else if swept == 0 {
					printNothingToReclaim(eng, args[0])
				}
				if swept > 0 {
					fmt.Printf("%s: swept %d crash leftover(s) (orphaned by an interrupted run, no commit footer)\n", args[0], swept)
				}
				warnIfOverCapacity(eng, args[0], now)
				return report.Run{Command: report.CommandPrune, ArchivesPruned: eligible, BytesMoved: freed}, nil
			})
		},
	}
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview without deleting")
	cmd.Flags().StringVar(&dateStr, "date", "", "reference 'now' date YYYY-MM-DD (default: the current time)")
	return cmd
}

// newFlushCmd implements `nb flush`: drain a crashed holding-disk run's leftover archives to
// the landing (Amanda's amflush). A normal `nb dump` already auto-flushes leftovers first, so
// this is the explicit, attended form.
func newFlushCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "flush",
		Short:   "Drain leftover holding-disk archives to the landing",
		Long:    "Copy any archives a crashed holding-disk run left on the holding disk to the landing, then reclaim the disk. The catalog already records what is on the holding disk, so no media scan is needed. `nb dump` runs this automatically before each run; use `nb flush` to drain explicitly. A no-op without a holding disk or when nothing is staged.",
		Example: "  nb flush   # after a crashed dump, before the next scheduled run",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			eng, release, err := a.engineFor(cfg, true) // writes media + catalog: lock it
			if err != nil {
				return err
			}
			defer release()
			attachOperator(eng)
			return a.runReported(cfg, report.Run{Command: report.CommandFlush, ExitClass: "flush-error"}, func() (report.Run, error) {
				n, err := eng.Flush(time.Now().UTC(), a.logf())
				if err != nil {
					return report.Run{}, err
				}
				if n == 0 {
					// Nothing staged (no holding disk, or already drained) — a no-op, not a run:
					// don't pollute the run log / nb report with an empty "OK" flush.
					fmt.Println("nothing to flush")
					return report.Run{}, skip(nil)
				}
				fmt.Printf("flushed %d archive(s) to the landing\n", n)
				return report.Run{Command: report.CommandFlush, RunsCopied: n}, nil
			})
		},
	}
	return cmd
}

// newResetCmd implements `nb reset <dle>`: schedule a DLE for a full on its next run. The
// escape hatch when an incremental chain has gone bad — an interrupted dump that left a
// dead snapshot, or a base that no longer matches the source.
func newResetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "reset <dle>",
		Short:   "Schedule a DLE for a full on its next run",
		Long:    "Mark a DLE so the next `nb dump` backs it up at level 0, starting a fresh incremental chain. Use this when an incremental chain has gone bad — e.g. a dump interrupted out of space left a dead snapshot, so incrementals would re-dump everything anyway. This records a force-full directive in the catalog that the planner honors (the archiver-independent peer of Amanda's `amadmin force`); it touches no incremental state, so the existing chain stays intact until the new full actually commits. The directive is consumed once the forced full runs. The DLE is named by its host:path identity (as `nb plan` shows) or its config name.",
		Example: "  nb reset web1:/var/www",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			eng, release, err := a.engineFor(cfg, true) // writes the catalog directive: lock out a concurrent dump
			if err != nil {
				return err
			}
			defer release()
			id, err := eng.ForceFull(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s will be fulled on its next run\n", id)
			return nil
		},
	}
}

// printNothingToReclaim explains a zero-reclaim prune. Tape reclaims whole volumes
// (relabel), never individual runs, so "fits capacity" would be misleading there —
// it is a no-op by design even when the library is over capacity. Say so, and point
// at the deliberate recycle path, so a user lowering tape capacity isn't told the
// runs "fit" when per-run pruning simply does not apply to tape.

func printNothingToReclaim(eng *engine.Engine, name string) {
	if info, ok := eng.Medium(name); ok && info.Type == "tape" {
		if info.Capacity > 0 && info.Used > info.Capacity {
			fmt.Printf("\n%s: over capacity (%s of %s), but tape reclaims whole volumes, not runs — recycle an aged-out tape with `nb label --relabel` (per-run pruning does not apply to tape)\n",
				name, sizeutil.FormatBytes(info.Used), sizeutil.FormatBytes(info.Capacity))
		} else {
			fmt.Printf("\n%s: nothing to reclaim — tape reclaims whole volumes, not runs (recycle an aged-out tape with `nb label --relabel`)\n", name)
		}
		return
	}
	fmt.Printf("\n%s: nothing to reclaim (all runs fit capacity or are protected)\n", name)
}

// warnIfOverCapacity closes the plan→prune loop: when a prune leaves a medium still
// over capacity it is because the protected recovery set alone exceeds capacity, so
// say so rather than reporting "freed N" and silently leaving the medium over budget.
// Tape is excluded — its whole-volume over-capacity case is covered by
// printNothingToReclaim's recycle hint.
func warnIfOverCapacity(eng *engine.Engine, medium string, now time.Time) {
	if info, ok := eng.Medium(medium); ok && info.Type == "tape" {
		return
	}
	// Use the post-reclamation residual (the protected set), not the raw catalog
	// total, so the dry-run preview and the real run report the same thing — a
	// dry-run still has the would-delete archives in the catalog.
	over, residual, capacity, err := eng.MediumProtectedOverCapacity(medium, now)
	if err != nil || !over {
		return
	}
	remedy := "raise its capacity or shorten minimum_age"
	if !eng.MediumProtectionIsAgeBound(medium, now) {
		// The binding pins are live recovery chains, which shortening minimum_age
		// cannot release — only more capacity or a longer cycle helps.
		remedy = "raise its capacity or lengthen the cycle"
	}
	fmt.Printf("WARNING: %q still holds %s, over its %s capacity — reclaiming every dead archive was not enough; the protected recovery set exceeds capacity, so %s\n",
		medium, sizeutil.FormatBytes(residual), sizeutil.FormatBytes(capacity), remedy)
}

// newCopyCmd implements `nb copy`: stream a run from the landing medium to
// another configured medium (e.g. disk -> tape).
func newCopyCmd(a *app) *cobra.Command {
	var from, to string
	var dryRun, force bool
	cmd := &cobra.Command{
		Use:     "copy <run-id>",
		Short:   "Copy a run from one medium to another (e.g. disk -> tape)",
		Long:    "Stream a run from one configured medium to another. The destination is selected with --to; the source defaults to the landing medium and is overridden with --from (e.g. un-vault tape -> disk). Copies by default (like `nb sync`/`nb prune`); pass --dry-run (-n) to preview.",
		Example: "  nb copy --to tape run-2026-06-21.001\n  nb copy --to tape --dry-run run-2026-06-21.001\n  nb copy --from tape --to disk run-2026-06-21.001",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			runID := args[0]
			if dryRun {
				return runCopyDryRun(cfg, runID, from, to, force)
			}
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			attachOperator(eng)
			// A run already on the target is an idempotent no-op (exit 0), matching
			// `nb sync`'s "up to date" — re-running a copy in a script must not fail.
			plan, err := eng.PlanCopy(runID, from, to, force)
			if err != nil {
				return err
			}
			if plan.AlreadyOnTarget {
				where := ""
				if len(plan.TargetLabels) > 0 {
					where = fmt.Sprintf(" (volume(s) %v)", plan.TargetLabels)
				}
				fmt.Printf("run %s is already on medium %q%s; nothing to copy (use --force to copy again)\n", runID, to, where)
				return nil
			}
			if err := eng.CopyRun(runID, from, to, force, a.logf()); err != nil {
				return err
			}
			fmt.Println("copy complete")
			// Mirror `nb sync`'s over-capacity warning so the single-run sibling does
			// not silently push a target past its budget.
			if over, used, capacity, cerr := eng.MediumOverCapacity(to); cerr == nil && over {
				fmt.Printf("WARNING: %q now holds %s, over its %s capacity — run `nb prune %s` to reclaim, or raise its capacity\n",
					to, sizeutil.FormatBytes(used), sizeutil.FormatBytes(capacity), to)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "destination medium name (required)")
	cmd.Flags().StringVar(&from, "from", "", "source medium name (default: the landing medium)")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview without copying")
	cmd.Flags().BoolVar(&force, "force", false, "re-copy even if the run is already recorded on the target medium")
	cmd.MarkFlagRequired("to")
	return cmd
}

// runCopyDryRun previews `nb copy` without writing, rendering the engine's CopyPlan
// (the same resolve/validate/already-present rules CopyRun applies) — matching the
// dry-run shape of sync/prune.
func runCopyDryRun(cfg *config.Config, runID, from, to string, force bool) error {
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	plan, err := eng.PlanCopy(runID, from, to, force)
	if err != nil {
		return err
	}
	if plan.AlreadyOnTarget {
		fmt.Printf("%s -> %s: %s already on target; nothing to copy (use --force to re-copy)\n", plan.From, plan.To, runID)
		return nil
	}
	fmt.Printf("%s -> %s: would copy %s (%d archive(s), %s). Re-run without --dry-run to copy.\n",
		plan.From, plan.To, runID, plan.Archives, sizeutil.FormatBytes(plan.Bytes))
	if over, projected, capacity, perr := eng.ProjectedOverCapacity(plan.To, plan.Bytes); perr == nil && over {
		fmt.Printf("WARNING: %q would hold %s, over its %s capacity — run `nb prune %s` to reclaim, or raise its capacity\n",
			plan.To, sizeutil.FormatBytes(projected), sizeutil.FormatBytes(capacity), plan.To)
	}
	return nil
}

// newSyncCmd implements `nb sync`: the batch form of `nb copy`. It mirrors every
// landing run a target is missing onto that target, oldest
// first. With --to it syncs one ad-hoc target; without --to it runs the rules in
// the config's `sync:` block. Copies by default (like `nb prune`).
func newSyncCmd(a *app) *cobra.Command {
	var from, to, sinceStr string
	var last int
	var dryRun, force bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Mirror one medium's runs onto another (e.g. disk -> tape/s3)",
		Long: "Copy every run the target medium is missing from a source medium, oldest " +
			"first. The batch, idempotent form of `nb copy`: an interrupted or repeated sync " +
			"resumes, copying only what is not yet on the target. The source defaults to the " +
			"landing medium and is overridden with --from. With --to it syncs one target; " +
			"without --to it runs the `sync:` rules from the config. Copies by default; pass " +
			"--dry-run (-n) to preview.",
		Example: "  nb sync\n  nb sync --to lto\n  nb sync --to glacier --last 4\n  nb sync --from lto --to disk --dry-run",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			since, err := ParseDate(sinceStr)
			if err != nil {
				return fmt.Errorf("invalid --since date %q: --since must be in YYYY-MM-DD format", sinceStr)
			}
			if sinceStr == "" {
				since = time.Time{} // ParseDate defaults to today; sync wants "no bound"
			}
			if last < 0 {
				return fmt.Errorf("--last must be 0 (all) or a positive count, got %d", last)
			}

			// Resolve the targets: an explicit --to (ad-hoc), or every configured rule.
			type target struct {
				from, name string
				sel        engine.SyncSelection
			}
			var targets []target
			if to != "" {
				targets = append(targets, target{from, to, engine.SyncSelection{Last: last, Since: since}})
			} else {
				for _, r := range cfg.Sync {
					targets = append(targets, target{r.From, r.To, engine.SyncSelection{Last: r.Last}})
				}
				if len(targets) == 0 {
					return fmt.Errorf("no sync target: pass --to <medium> or add a `sync:` block to the config")
				}
			}

			// Dry-run only reads; a real run writes media + catalog, so lock it.
			eng, release, err := a.engineFor(cfg, !dryRun)
			if err != nil {
				return err
			}
			defer release()
			if !dryRun {
				attachOperator(eng)
			}

			runSync := func() (report.Run, error) {
				rec := report.Run{Command: report.CommandSync}
				for _, t := range targets {
					sr, err := eng.SyncTo(t.from, t.name, t.sel, !dryRun, force, a.logf())
					if sr != nil {
						printSyncReport(sr, !dryRun)
						rec.RunsCopied += sr.Copied()
						rec.BytesMoved += sr.CopiedBytes()
					}
					if err != nil {
						return rec, err
					}
				}
				return rec, nil
			}
			if dryRun {
				_, err := runSync()
				return err
			}
			return a.runReported(cfg, report.Run{Command: report.CommandSync, ExitClass: "sync-error"}, runSync)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "target medium (omit to run the config's sync: rules)")
	cmd.Flags().StringVar(&from, "from", "", "source medium (default: the landing medium)")
	cmd.Flags().IntVar(&last, "last", 0, "copy only the N most recent runs (0 = all); combined with --since, the newest N of those on/after the date")
	cmd.Flags().StringVar(&sinceStr, "since", "", "copy only runs dated on/after this date YYYY-MM-DD (intersects with --last)")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview without copying")
	cmd.Flags().BoolVar(&force, "force", false, "re-copy runs already recorded on the target")
	return cmd
}

// printSyncReport renders one target's backlog, matching the prune dry-run style.
func printSyncReport(r *engine.SyncReport, apply bool) {
	if len(r.Items) == 0 {
		fmt.Printf("%s -> %s: up to date\n", r.From, r.To)
		return
	}
	if apply {
		// Report the bytes that actually landed (copied runs), not the whole backlog —
		// a sync that stops partway (e.g. the target filled) must not claim it moved
		// bytes for runs it never copied.
		fmt.Printf("%s -> %s: copied %d run(s), %s\n", r.From, r.To, r.Copied(), sizeutil.FormatBytes(r.CopiedBytes()))
	} else {
		fmt.Printf("%s -> %s: %d run(s) to copy, %s (dry-run; re-run without --dry-run to copy):\n",
			r.From, r.To, len(r.Items), sizeutil.FormatBytes(r.Bytes()))
		for _, it := range r.Items {
			fmt.Printf("  %-24s %2d archive(s)  %s\n", it.RunID, it.Archives, sizeutil.FormatBytes(it.Bytes))
		}
	}
	// Sync copies regardless, but a target it pushes past capacity is worth flagging:
	// otherwise the overshoot only surfaces later, at the next `nb plan`/`nb prune`.
	if r.OverCapacity() {
		fmt.Printf("WARNING: %q would hold %s, over its %s capacity — run `nb prune %s` to reclaim, or raise its capacity\n",
			r.To, sizeutil.FormatBytes(r.ProjectedBytes), sizeutil.FormatBytes(r.TargetCapacity), r.To)
	}
}

// newLabelCmd implements `nb label`: write (or rewrite) a volume's identity
// label. This is the deliberate act that makes a tape writable; it guards
// against overwriting foreign data or a tape that still holds protected runs.
func newLabelCmd(a *app) *cobra.Command {
	var relabel, force bool
	cmd := &cobra.Command{
		Use:     "label <medium> <name>",
		Short:   "Label a volume (required for tape before first dump)",
		Long:    "Write a volume's identity label, making it writable. Refuses to overwrite foreign data, and (with --relabel) a tape that still holds protected runs — those within minimum_age or holding a DLE's last recovery path, including a run spanned across tapes. --relabel reuses an NBackup-labeled volume and --force overrides safety refusals.\n\nOn a robotic library, a new label takes a blank bay; to recycle a specific tape to a new name, `nb load <bay>` it first, then `nb label --relabel <name>` — the relabel acts on the loaded bay. A single-drive station always labels whatever reel is in the drive.",
		Example: "  nb label tape DAILY-01\n  nb load lto bay-02\n  nb label --relabel lto DAILY-42   # recycle the loaded tape to a new name",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// An empty name would otherwise dead-end deep in the label protocol with a
			// misleading "no blank slot available"; reject it up front for what it is.
			if strings.TrimSpace(args[1]) == "" {
				return fmt.Errorf("label name required, e.g. `nb label %s DAILY-01`", args[0])
			}
			cfg, err := a.load()
			if err != nil {
				return err
			}
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			return eng.LabelVolume(args[0], args[1], relabel, force, time.Now().UTC(), a.logf())
		},
	}
	cmd.Flags().BoolVar(&relabel, "relabel", false, "reuse a volume already labeled by NBackup")
	cmd.Flags().BoolVar(&force, "force", false, "override safety refusals (foreign data / protected runs)")
	return cmd
}

// newMediumCmd implements `nb medium`: list media (default) or detail one.
func newMediumCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "medium [name]",
		Short:   "List media and their capacity/volumes, or detail one",
		Long:    "List every configured medium with its type, run count, usage, capacity, and current volume. Pass a medium name to show its volume and the runs it holds.",
		Example: "  nb medium\n  nb medium lto",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadRO()
			if err != nil {
				return err
			}
			// A bare config (no sources) means no config file was found — read-only
			// commands fall back to a synthesized default catalog, so don't present its
			// phantom default disk medium as if storage were configured (matching `nb run`).
			if len(cfg.Sources) == 0 {
				fmt.Println("no media (no backup config found — copy nbackup.example.yaml to nbackup.yaml and edit it, or pass -c <config>)")
				return nil
			}
			// The detail view inventories the changer (drive/slot status) — a device
			// access, so it locks like any medium-accessing command. The bare listing
			// reads only the cached catalog and stays lock-free.
			if len(args) >= 1 {
				eng, unlock, err := a.lockedEngine(cfg)
				if err != nil {
					return err
				}
				defer unlock()
				return mediumDetail(eng, args[0])
			}
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			return mediumList(eng)
		},
	}
}

func mediumList(eng *engine.Engine) error {
	media := eng.Media()
	if len(media) == 0 {
		fmt.Println("no media configured")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MEDIUM\tTYPE\tRUNS\tUSED\tCAPACITY\tVOLUME")
	for _, m := range media {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", m.Name, m.Type, m.Runs,
			sizeutil.FormatBytes(m.Used)+overMarker(m.Used, m.Capacity), capacityStr(m.Capacity), volumeStr(m))
	}
	tw.Flush()
	return nil
}

// overMarker flags usage that has run past a bounded medium's capacity, so an
// over-capacity medium does not read as healthy in `nb medium` listings (sync/copy
// can land runs past capacity; pruning reclaims them on its own schedule).
func overMarker(used, capacity int64) string {
	if capacity > 0 && used > capacity {
		return " (over!)"
	}
	return ""
}

func mediumDetail(eng *engine.Engine, name string) error {
	m, ok := eng.Medium(name)
	if !ok {
		return fmt.Errorf("unknown medium %q", name)
	}
	fmt.Printf("Medium %s  (%s)\n", m.Name, m.Type)
	fmt.Printf("  volume:  %s\n", volumeStr(m))
	fmt.Printf("  used:    %s / %s%s\n", sizeutil.FormatBytes(m.Used), capacityStr(m.Capacity), overMarker(m.Used, m.Capacity))
	fmt.Printf("  retention: minimum_age %s\n", sizeutil.FormatDuration(eng.MediumMinAge(name)))
	printInventory(eng, name)
	fmt.Println()
	runs := eng.Catalog().RunsOn(name)
	if len(runs) == 0 {
		fmt.Println("no runs on this medium")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSIZE\tARCHIVES\tCOMMITTED")
	for _, s := range runs {
		committed := "-"
		if t := s.LastArchiveAt(); !t.IsZero() {
			committed = t.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.ID, sizeutil.FormatBytes(s.TotalBytes()), len(s.Archives), committed)
	}
	tw.Flush()
	return nil
}

// printInventory shows a tape medium's physical inventory beneath its `nb medium`
// detail: each drive and what is loaded (with its label and fill), then the occupied
// slots by barcode (a real library reports barcodes without loading; the on-tape label
// is known only once a cartridge is in a drive). Media with no changer (disk, s3)
// print nothing.
func printInventory(eng *engine.Engine, name string) {
	view, err := eng.ChangerView(name)
	if err != nil {
		return // address-identified medium: nothing physical to inventory
	}
	appendable := eng.MediumAppendable(name)

	dw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(dw, "\n\tDRIVE\tBARCODE\tLABEL\tSTATUS\tON VOLUME\tFILES")
	for _, d := range view.Drives {
		if !d.Loaded {
			fmt.Fprintf(dw, "\t%d\t(empty)\t-\t-\t-\t-\n", d.Drive)
			continue
		}
		label, status := volumeLabelStatus(d.Volume, name, appendable, volumeHasRuns(eng, d.Volume.Label))
		fmt.Fprintf(dw, "\t%d\t%s\t%s\t%s\t%s\t%d\n", d.Drive, barcodeOr(d.Volume.Barcode), label, status,
			sizeutil.FormatBytes(d.Volume.Used), d.Volume.Files)
	}
	dw.Flush()

	var occupied []media.SlotStatus
	for _, s := range view.Slots {
		if s.Full && !s.ImportExport {
			occupied = append(occupied, s)
		}
	}
	if len(occupied) > 0 {
		heading := "Slots"
		if view.Manual {
			heading = "In the room (load with `nb load`, or when prompted)"
		}
		fmt.Printf("\n%s:\n", heading)
		sw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(sw, "  SLOT\tBARCODE\tLABEL")
		for _, s := range occupied {
			// The volume last seen on this cartridge — the catalog's learned
			// barcode↔label memory, not a fresh read; "-" until it has been loaded.
			lbl := "-"
			if name, ok := view.SlotLabels[s.Slot]; ok {
				lbl = name
			}
			fmt.Fprintf(sw, "  %d\t%s\t%s\n", s.Slot, s.Barcode, lbl)
		}
		sw.Flush()
	}
}

// barcodeOr renders a barcode, or a dash when the changer has no scanner.
func barcodeOr(bc string) string {
	if bc == "" {
		return "-"
	}
	return bc
}

func capacityStr(c int64) string {
	if c <= 0 {
		return "unbounded"
	}
	return sizeutil.FormatBytes(c)
}

// volumeHasRuns reports whether the catalog records any committed run on the named
// label — false for a blank tape or one holding only orphan parts from an aborted
// span/write. Used to mark such a tape reclaimable in the inventory.
func volumeHasRuns(eng *engine.Engine, label string) bool {
	if label == "" {
		return false
	}
	return len(eng.Catalog().RunsOnLabel(label)) > 0
}

// volumeLabelStatus renders a volume's display label and fill status for inventory
// listings (a blank volume, a full one, a wrong-pool reel, or an appendable labeled
// one). medium is the medium being inventoried, so a reel labeled for a different
// pool is flagged rather than shown as one of this medium's own volumes.
func volumeLabelStatus(b media.VolumeStatus, medium string, appendable, hasRuns bool) (label, status string) {
	switch {
	case b.Foreign:
		return "(foreign)", "foreign"
	case b.Blank:
		return "(blank)", "blank"
	case b.Pool != "" && b.Pool != medium:
		// A valid NBackup label, but for another pool: the write guard would refuse it
		// (wrong tape), so the inventory must not present it as this medium's own.
		return b.Label, fmt.Sprintf("wrong-pool:%s", b.Pool)
	case b.Files > 1 && !hasRuns:
		// The volume holds data past its label, but the catalog records no committed
		// run on it — orphan parts from an interrupted span/write. It is reusable as
		// is (no committed data to lose) via `nb label --relabel`, so present it as
		// reclaimable rather than "full"/"used", which imply it holds real backups.
		return b.Label, "reclaimable"
	case b.Capacity > 0 && b.Used >= b.Capacity:
		return b.Label, "full"
	case !appendable:
		// One run per volume: a reel that already holds a run cannot be appended, so
		// "append" would misrepresent it. b.Files counts the file-0 label plus any run
		// files, so >1 means it holds a run.
		if b.Files > 1 {
			return b.Label, "used"
		}
		return b.Label, "writable"
	default:
		return b.Label, "append"
	}
}

func volumeStr(m engine.MediumInfo) string {
	if m.Volume == "" {
		return "-"
	}
	if m.Epoch > 0 {
		return fmt.Sprintf("%s (epoch %d)", m.Volume, m.Epoch)
	}
	return m.Volume
}

// newLoadCmd implements `nb load`: mount a volume into a changer medium's drive —
// a robotic library bay, or a reel from a single-drive station's shelf — so the
// next read or write acts on it. The physical sibling of `nb label`; what's in the
// drive is shown by `nb medium <name>`.
func newLoadCmd(a *app) *cobra.Command {
	var byLabel bool
	cmd := &cobra.Command{
		Use:     "load <medium> <bay-reel-or-label>",
		Short:   "Load a volume into a medium's drive",
		Long:    "Load a volume into the medium's drive: a bay on a robotic library, or a reel from a single-drive station's shelf. By default the argument is a bay/reel id; with --label it is matched against volume labels instead. Inventory the medium with `nb medium <name>`.",
		Example: "  nb load lto bay-03\n  nb load --label lto DAILY-01\n  nb load vtape reel-02",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				return nil
			}
			// The natural mistake `nb load <medium>` (one arg) should explain that an
			// address-identified medium has nothing to load, rather than fall back to
			// cobra's bare "accepts 2 arg(s)".
			if len(args) == 1 {
				if cfg, err := a.load(); err == nil {
					if d, ok := cfg.Media[args[0]]; ok && d.Type != "tape" {
						return fmt.Errorf("medium %q is addressed directly, not by loading volumes (`nb load` applies only to a tape library or single-drive station)", args[0])
					}
				}
			}
			return fmt.Errorf("load requires a medium and a bay/reel/label, e.g. `nb load lto bay-03`")
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			return eng.LoadVolume(args[0], args[1], byLabel, a.logf())
		},
	}
	cmd.Flags().BoolVar(&byLabel, "label", false, "treat the argument as a volume label rather than a bay/reel id")
	return cmd
}

// newRebuildCmd implements `nb rebuild`: rebuild the local run-index cache by
// rescanning the self-describing media.
func newRebuildCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "rebuild",
		Short:   "Rebuild the catalog cache by rescanning media",
		Long:    "Rescan every configured medium and rebuild the local catalog cache (run index and volume registry) from the commit footers and labels found there.",
		Example: "  nb rebuild   # e.g. after losing the workdir on a new server",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadRO()
			if err != nil {
				return err
			}
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			n, err := eng.RebuildCatalog(a.logf())
			if err != nil {
				return err
			}
			fmt.Printf("catalog cache rebuilt from media: %d run(s) indexed\n", n)
			return nil
		},
	}
}
