package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
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
					fmt.Printf("WARNING: over capacity; run `nb prune` to reclaim oldest slots\n")
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
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today)")
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

// newDumpCmd implements `nb dump`: execute a run and seal a slot, or — with
// --dry-run — plan the run for --date and print it without writing anything.
func newDumpCmd(a *app) *cobra.Command {
	var dateStr string
	var dryRun bool
	cmd := &cobra.Command{
		Use:     "dump",
		Short:   "Execute a run and commit its archives",
		Long:    "Execute a planner run, dumping each scheduled DLE and committing exactly one immutable slot's archives. Use --quiet to suppress progress output. --date sets the run date (with or without --dry-run). With --dry-run the run for --date is planned against the current catalog and printed, exactly as a real dump would decide it, but nothing is written.",
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
				return runDumpDryRun(eng, date, warnings)
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
			eng.SetRunProgress(runProgress(a.quiet))
			return a.runReported(cfg, report.Run{Command: report.CommandDump, ExitClass: "dump-failed"}, func() (report.Run, error) {
				s, err := eng.Run(date, a.logf())
				if err != nil {
					return report.Run{}, err
				}
				fmt.Printf("\nCommitted %s: %d archive(s), %s total\n", s.ID, len(s.Archives), sizeutil.FormatBytes(s.TotalBytes))
				return report.Run{
					Command:    report.CommandDump,
					SlotID:     s.ID,
					Archives:   len(s.Archives),
					BytesMoved: s.TotalBytes,
					DumpStats:  dumpStats(s, cfg.WorkdirPath()),
				}, nil
			})
		},
	}
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today)")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "plan the run for --date and print it without writing anything")
	return cmd
}

// dumpStats builds the per-DLE statistics for a sealed slot's run record: sizes,
// level, and files come from the seal (authoritative); the dump duration comes from
// the run-status snapshot the tracker just flushed (the same file `nb status` reads),
// matched by DLE name and level. When the snapshot is missing or stale, sizes are
// still recorded and timing is left zero (rendered as a dash).
func dumpStats(s *record.Slot, workdir string) []report.DLEStat {
	type key struct {
		name  string
		level int
	}
	durations := map[key]float64{}
	if snap, err := progress.Load(workdir); err == nil && snap.SlotID == s.ID {
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
// decision logic a real run uses — and prints it. Nothing is sealed.
func runDumpDryRun(eng *engine.Engine, date time.Time, validationWarnings []string) error {
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
	fmt.Printf("Would commit %s. Run without --dry-run to execute.\n", eng.PlannedSlotID(date))
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
			cfg, err := a.loadRO()
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
				time.Sleep(watch)
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

// newVerifyCmd implements `nb verify`: check archive checksums of named slots, or
// every slot with --all. Verifying all slots can mount every volume in the pool
// (each tape in turn), so the whole-pool scan is gated behind an explicit flag
// rather than triggered by a bare `nb verify`.
func newVerifyCmd(a *app) *cobra.Command {
	var all, deep bool
	cmd := &cobra.Command{
		Use:   "verify [slot-id...]",
		Short: "Verify slot integrity (checksum, or --deep structural)",
		Long: "Verify archives against their commit footers. By default it re-checks payload checksums " +
			"(integrity). With --deep it also streams each archive through the real read " +
			"pipeline — decrypt, decompress, then `tar -t` (list, not extract) — and asserts the " +
			"members match the recorded index, proving the bytes are a valid restorable stream and " +
			"exercising the key and compression end-to-end. It writes nothing either way. Pass slot ids " +
			"to verify just those; with no ids it verifies every slot (which may mount every volume in the pool).",
		Example: "  nb verify slot-2026-06-21\n  nb verify --deep slot-2026-06-21\n  nb verify",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return fmt.Errorf("--all cannot be combined with explicit slot ids")
			}
			// Bare `nb verify` (no slot ids) verifies the whole catalog — the obvious
			// reading of "verify my backups". --all stays as an explicit synonym.
			if len(args) == 0 {
				all = true
			}
			// verify is an assertion (monitors gate on its exit code), so a missing
			// config is an error — not a green "0 slot(s) verified".
			cfg, err := a.loadRORequire()
			if err != nil {
				return err
			}
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			// Verifying reads media, so a spanned slot on a single-drive station needs
			// reel swaps — give it the operator so it prompts (and reassembles a spanned
			// slot) just like restore, rather than failing at the first volume boundary.
			attachOperator(eng)
			if all && !a.quiet {
				mode := "checksum"
				if deep {
					mode = "deep (checksum + structural)"
				}
				fmt.Printf("verifying %d slot(s) in the catalog [%s]\n", len(eng.Catalog().Slots()), mode)
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
					return rec, fmt.Errorf("%d slot(s) failed verification", vr.Failures)
				}
				return rec, nil
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "verify every slot in the catalog (the default when no slot ids are given)")
	cmd.Flags().BoolVar(&deep, "deep", false, "also validate structure: decrypt+decompress+tar-list, members vs the recorded index")
	return cmd
}

// newSlotCmd implements `nb slot`: list slots (default), show a slot, or prune.
func newSlotCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "slot",
		Short: "List or show slots",
		Long:  "Inspect the slot catalog. With no subcommand it lists slots; see the subcommands to show a single slot. (Reclaim slots with `nb prune`.)",
		Args:  cobra.NoArgs,
		// Bare `nb slot` lists slots, preserving prior behavior.
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSlotList(a)
		},
	}
	cmd.AddCommand(newSlotListCmd(a), newSlotShowCmd(a))
	return cmd
}

func newSlotListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List slots in the catalog",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSlotList(a)
		},
	}
}

func runSlotList(a *app) error {
	cfg, err := a.loadRO()
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	slots := eng.Catalog().Slots()
	if len(slots) == 0 {
		// A bare config (no sources) means no backup config was found — read-only
		// commands fall back to the default local catalog, so distinguish "configured
		// but nothing dumped yet" from "no config at all" to spare a newcomer the
		// false impression that an unconfigured `nb slot` succeeded meaningfully.
		if len(cfg.Sources) == 0 {
			fmt.Println("no slots in catalog (no backup config found — copy nbackup.example.yaml to nbackup.yaml and run `nb dump`, or pass -c <config>)")
		} else {
			fmt.Println("no slots in catalog")
		}
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SLOT\tSTATUS\tARCHIVES\tSIZE\tCOMMITTED\tCOPIES")
	for _, s := range slots {
		committed := "-"
		if !s.SealedAt.IsZero() {
			committed = s.SealedAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", s.ID, slotStatusDisplay(s.Status), len(s.Archives),
			sizeutil.FormatBytes(s.TotalBytes), committed, copiesSummary(eng.Catalog().Placements(s.ID)))
	}
	tw.Flush()
	return nil
}

// copiesSummary renders a slot's placements as a compact comma list, naming the
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

func newSlotShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "show <slot-id>",
		Short:   "Show a single slot's archives and copies",
		Example: "  nb slot show slot-2026-06-21",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("specify exactly one slot id, e.g. `nb slot show slot-2026-06-21` (list them with `nb slot`)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadRO()
			if err != nil {
				return err
			}
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			s, err := eng.Catalog().ReadSlot(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Slot %s  (%s)\n", s.ID, slotStatusDisplay(s.Status))
			fmt.Printf("  date:    %s\n", s.Date)
			fmt.Printf("  committed: %s\n", s.SealedAt.Format("2006-01-02 15:04:05 MST"))
			fmt.Printf("  total:   %s\n\n", sizeutil.FormatBytes(s.TotalBytes))
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "DLE\tLEVEL\tFILES\tSIZE\tCOMPRESS\tENCRYPT")
			for _, ar := range s.Archives {
				enc := ar.Encrypt
				if enc == "" {
					enc = "none"
				}
				fmt.Fprintf(tw, "%s\tL%d\t%d\t%s\t%s\t%s\n", ar.DLEID(), ar.Level, ar.FileCount, sizeutil.FormatBytes(ar.Compressed), ar.Compress, enc)
			}
			tw.Flush()

			placements := eng.Catalog().Placements(s.ID)
			fmt.Printf("\nCOPIES (%d)\n", len(placements))
			ptw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(ptw, "  MEDIUM\tVOLUMES\tPOSITIONS")
			for _, p := range placements {
				volumes := "-"
				if labels := p.Labels(); len(labels) > 0 {
					volumes = strings.Join(labels, "+")
				}
				positions := make([]string, 0, len(p.Archives))
				for _, ar := range p.Archives {
					locs := make([]string, 0, len(ar.Parts))
					for _, pt := range ar.Parts {
						locs = append(locs, fmt.Sprintf("%d", pt.Pos))
					}
					positions = append(positions, fmt.Sprintf("%s/L%d@%s", eng.DisplayDLE(ar.DLE), ar.Level, strings.Join(locs, ",")))
				}
				fmt.Fprintf(ptw, "  %s\t%s\t%s\n", p.Medium, volumes, strings.Join(positions, " "))
			}
			ptw.Flush()
			return nil
		},
	}
}

// newDleCmd implements `nb dle`: inspect the catalog grouped by DLE (backup source)
// rather than by slot. The same archives the slot view groups by run, browsed instead
// by what was backed up — one row per DLE, then its archive timeline across slots.
func newDleCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dle",
		Short: "List or show DLEs (backup sources)",
		Long:  "Inspect the catalog grouped by DLE (a host:path backup source). With no subcommand it lists each DLE and its backup history; `nb dle show <dle>` shows one DLE's archive timeline across slots. (Reclaim with `nb prune`, which is per-DLE on disk/cloud.)",
		Args:  cobra.NoArgs,
		// Bare `nb dle` lists DLEs, mirroring `nb slot`.
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDleList(a)
		},
	}
	cmd.AddCommand(newDleListCmd(a), newDleShowCmd(a))
	return cmd
}

func newDleListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List DLEs and their backup history",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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
	slots := eng.Catalog().Slots()
	if len(slots) == 0 {
		if len(cfg.Sources) == 0 {
			fmt.Println("no DLEs in catalog (no backup config found — copy nbackup.example.yaml to nbackup.yaml and run `nb dump`, or pass -c <config>)")
		} else {
			fmt.Println("no DLEs in catalog")
		}
		return nil
	}

	// Aggregate per DLE across slots. Slots come in run order, so the last archive
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
	for _, s := range slots {
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
				g.lastFull = s.Date
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

func newDleShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "show <dle>",
		Short:   "Show one DLE's archive timeline across slots",
		Example: "  nb dle show localhost:/home",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("specify exactly one DLE, e.g. `nb dle show localhost:/home` (list them with `nb dle`)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadRO()
			if err != nil {
				return err
			}
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			slots := eng.Catalog().Slots()
			slug, display, ok := resolveDLE(slots, args[0])
			if !ok {
				return fmt.Errorf("no DLE %q in catalog (list them with `nb dle`)", args[0])
			}
			fmt.Printf("DLE %s\n\n", display)
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "SLOT\tDATE\tLEVEL\tSIZE\tBASE\tCOPIES")
			for _, s := range slots {
				for _, ar := range s.Archives {
					if ar.DLE != slug {
						continue
					}
					base := ar.BaseSlot
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
					fmt.Fprintf(tw, "%s\t%s\tL%d\t%s\t%s\t%s\n", s.ID, s.Date, ar.Level,
						sizeutil.FormatBytes(ar.Compressed), base, strings.Join(media, ", "))
				}
			}
			tw.Flush()
			return nil
		},
	}
}

// resolveDLE matches a user-typed DLE identifier against the catalog's archives,
// accepting either the internal slug or the host:path display id, and returns the
// slug plus a display string. Archives carry their own host/path, so the match needs
// no config — a DLE that was dumped but later removed from config still resolves.
func resolveDLE(slots []*record.Slot, arg string) (slug, display string, ok bool) {
	for _, s := range slots {
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

// newPruneCmd implements `nb prune`: reclaim slots past the cycle/capacity limits.
func newPruneCmd(a *app) *cobra.Command {
	var dryRun bool
	var dateStr string
	cmd := &cobra.Command{
		Use:     "prune <medium>",
		Short:   "Delete a medium's slots past its cycle/capacity limits",
		Long:    "Reclaim slots on the named medium that fall outside its own cycle and capacity limits. Retention is per-medium, so the medium to prune must be named explicitly (pruning one store never touches a copy on another). Deletes by default; pass --dry-run (-n) to preview.",
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
			// Dry-run prune only reads; a real run deletes slots, so lock it.
			eng, release, err := a.engineFor(cfg, !dryRun)
			if err != nil {
				return err
			}
			defer release()
			now, err := ParseDate(dateStr)
			if err != nil {
				return err
			}
			if dryRun {
				eligible, _, err := eng.Prune(args[0], now, false, a.logf())
				if err != nil {
					return err
				}
				if eligible > 0 {
					fmt.Printf("\n%d slot(s) eligible. Re-run without --dry-run to delete.\n", eligible)
				} else {
					printNothingToReclaim(eng, args[0])
				}
				warnIfOverCapacity(eng, args[0])
				return nil
			}
			return a.runReported(cfg, report.Run{Command: report.CommandPrune, ExitClass: "prune-error"}, func() (report.Run, error) {
				eligible, freed, err := eng.Prune(args[0], now, true, a.logf())
				if err != nil {
					return report.Run{}, err
				}
				if eligible > 0 {
					fmt.Printf("\n%s: deleted %d slot(s), freed %s\n", args[0], eligible, sizeutil.FormatBytes(freed))
				} else {
					printNothingToReclaim(eng, args[0])
				}
				warnIfOverCapacity(eng, args[0])
				return report.Run{Command: report.CommandPrune, SlotsPruned: eligible, BytesMoved: freed}, nil
			})
		},
	}
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview without deleting")
	cmd.Flags().StringVar(&dateStr, "date", "", "reference 'now' date YYYY-MM-DD (default today)")
	return cmd
}

// printNothingToReclaim explains a zero-reclaim prune. Tape reclaims whole volumes
// (relabel), never individual slots, so "fits capacity" would be misleading there —
// it is a no-op by design even when the library is over capacity. Say so, and point
// at the deliberate recycle path, so a user lowering tape capacity isn't told the
// slots "fit" when per-slot pruning simply does not apply to tape.
// slotStatusDisplay renders a slot's internal status with the user-facing
// vocabulary: a complete slot is "committed" (its archives' commit footers are all
// present), not "sealed" — NBackup has no slot-level seal.
func slotStatusDisplay(status string) string {
	if status == record.StatusSealed {
		return "committed"
	}
	return status
}

func printNothingToReclaim(eng *engine.Engine, name string) {
	if info, ok := eng.Medium(name); ok && info.Type == "tape" {
		if info.Capacity > 0 && info.Used > info.Capacity {
			fmt.Printf("\n%s: over capacity (%s of %s), but tape reclaims whole volumes, not slots — recycle an aged-out tape with `nb label --relabel` (per-slot pruning does not apply to tape)\n",
				name, sizeutil.FormatBytes(info.Used), sizeutil.FormatBytes(info.Capacity))
		} else {
			fmt.Printf("\n%s: nothing to reclaim — tape reclaims whole volumes, not slots (recycle an aged-out tape with `nb label --relabel`)\n", name)
		}
		return
	}
	fmt.Printf("\n%s: nothing to reclaim (all slots fit capacity or are protected)\n", name)
}

// warnIfOverCapacity closes the plan→prune loop: when a prune leaves a medium still
// over capacity it is because the protected recovery set alone exceeds capacity, so
// say so rather than reporting "freed N" and silently leaving the medium over budget.
// Tape is excluded — its whole-volume over-capacity case is covered by
// printNothingToReclaim's recycle hint.
func warnIfOverCapacity(eng *engine.Engine, medium string) {
	if info, ok := eng.Medium(medium); ok && info.Type == "tape" {
		return
	}
	if over, used, capacity, err := eng.MediumOverCapacity(medium); err == nil && over {
		fmt.Printf("WARNING: %q still holds %s, over its %s capacity — reclaiming every dead archive was not enough; the protected recovery set exceeds capacity, so raise its capacity or shorten minimum_age\n",
			medium, sizeutil.FormatBytes(used), sizeutil.FormatBytes(capacity))
	}
}

// newCopyCmd implements `nb copy`: stream a slot from the landing medium to
// another configured medium (e.g. disk -> tape).
func newCopyCmd(a *app) *cobra.Command {
	var from, to string
	var dryRun, force bool
	cmd := &cobra.Command{
		Use:     "copy <slot-id>",
		Short:   "Copy a slot from one medium to another (e.g. disk -> tape)",
		Long:    "Stream a slot from one configured medium to another. The destination is selected with --to; the source defaults to the landing medium and is overridden with --from (e.g. un-vault tape -> disk). Copies by default (like `nb sync`/`nb prune`); pass --dry-run (-n) to preview.",
		Example: "  nb copy --to tape slot-2026-06-21\n  nb copy --to tape --dry-run slot-2026-06-21\n  nb copy --from tape --to disk slot-2026-06-21",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			slotID := args[0]
			if dryRun {
				return runCopyDryRun(cfg, slotID, from, to, force)
			}
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			attachOperator(eng)
			// A slot already on the target is an idempotent no-op (exit 0), matching
			// `nb sync`'s "up to date" — re-running a copy in a script must not fail.
			plan, err := eng.PlanCopy(slotID, from, to, force)
			if err != nil {
				return err
			}
			if plan.AlreadyOnTarget {
				where := ""
				if len(plan.TargetLabels) > 0 {
					where = fmt.Sprintf(" (volume(s) %v)", plan.TargetLabels)
				}
				fmt.Printf("slot %s is already on medium %q%s; nothing to copy (use --force to copy again)\n", slotID, to, where)
				return nil
			}
			if err := eng.CopySlot(slotID, from, to, force, a.logf()); err != nil {
				return err
			}
			fmt.Println("copy complete")
			// Mirror `nb sync`'s over-capacity warning so the single-slot sibling does
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
	cmd.Flags().BoolVar(&force, "force", false, "re-copy even if the slot is already recorded on the target medium")
	cmd.MarkFlagRequired("to")
	return cmd
}

// runCopyDryRun previews `nb copy` without writing, rendering the engine's CopyPlan
// (the same resolve/validate/already-present rules CopySlot applies) — matching the
// dry-run shape of sync/prune.
func runCopyDryRun(cfg *config.Config, slotID, from, to string, force bool) error {
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	plan, err := eng.PlanCopy(slotID, from, to, force)
	if err != nil {
		return err
	}
	if plan.AlreadyOnTarget {
		fmt.Printf("%s -> %s: %s already on target; nothing to copy (use --force to re-copy)\n", plan.From, plan.To, slotID)
		return nil
	}
	fmt.Printf("%s -> %s: would copy %s (%d archive(s), %s). Re-run without --dry-run to copy.\n",
		plan.From, plan.To, slotID, plan.Archives, sizeutil.FormatBytes(plan.Bytes))
	if over, projected, capacity, perr := eng.ProjectedOverCapacity(plan.To, plan.Bytes); perr == nil && over {
		fmt.Printf("WARNING: %q would hold %s, over its %s capacity — run `nb prune %s` to reclaim, or raise its capacity\n",
			plan.To, sizeutil.FormatBytes(projected), sizeutil.FormatBytes(capacity), plan.To)
	}
	return nil
}

// newSyncCmd implements `nb sync`: the batch form of `nb copy`. It mirrors every
// landing slot a target is missing onto that target, oldest
// first. With --to it syncs one ad-hoc target; without --to it runs the rules in
// the config's `sync:` block. Copies by default (like `nb slot prune`).
func newSyncCmd(a *app) *cobra.Command {
	var from, to, sinceStr string
	var last int
	var dryRun, force bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Mirror one medium's slots onto another (e.g. disk -> tape/s3)",
		Long: "Copy every slot the target medium is missing from a source medium, oldest " +
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
						rec.SlotsCopied += sr.Copied()
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
	cmd.Flags().IntVar(&last, "last", 0, "copy only the N most recent slots (0 = all)")
	cmd.Flags().StringVar(&sinceStr, "since", "", "copy only slots created on/after this date YYYY-MM-DD")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview without copying")
	cmd.Flags().BoolVar(&force, "force", false, "re-copy slots already recorded on the target")
	return cmd
}

// printSyncReport renders one target's backlog, matching the prune dry-run style.
func printSyncReport(r *engine.SyncReport, apply bool) {
	if len(r.Items) == 0 {
		fmt.Printf("%s -> %s: up to date\n", r.From, r.To)
		return
	}
	if apply {
		// Report the bytes that actually landed (copied slots), not the whole backlog —
		// a sync that stops partway (e.g. the target filled) must not claim it moved
		// bytes for slots it never copied.
		fmt.Printf("%s -> %s: copied %d slot(s), %s\n", r.From, r.To, r.Copied(), sizeutil.FormatBytes(r.CopiedBytes()))
	} else {
		fmt.Printf("%s -> %s: %d slot(s) to copy, %s (dry-run; re-run without --dry-run to copy):\n",
			r.From, r.To, len(r.Items), sizeutil.FormatBytes(r.Bytes()))
		for _, it := range r.Items {
			fmt.Printf("  %-24s %2d archive(s)  %s\n", it.SlotID, it.Archives, sizeutil.FormatBytes(it.Bytes))
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
// against overwriting foreign data or a tape that still holds protected slots.
func newLabelCmd(a *app) *cobra.Command {
	var relabel, force bool
	cmd := &cobra.Command{
		Use:     "label <medium> <name>",
		Short:   "Label a volume (required for tape before first dump)",
		Long:    "Write a volume's identity label, making it writable. Refuses to overwrite foreign data, and (with --relabel) a tape that still holds protected slots — those within minimum_age or holding a DLE's last recovery path, including a slot spanned across tapes. --relabel reuses an NBackup-labeled volume and --force overrides safety refusals.\n\nOn a robotic library, a new label takes a blank bay; to recycle a specific tape to a new name, `nb load <bay>` it first, then `nb label --relabel <name>` — the relabel acts on the loaded bay. A single-drive station always labels whatever reel is in the drive.",
		Example: "  nb label tape DAILY-01\n  nb load lto bay-02\n  nb label --relabel lto DAILY-42   # recycle the loaded tape to a new name",
		Args:    cobra.ExactArgs(2),
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
			return eng.LabelVolume(args[0], args[1], relabel, force, time.Now().UTC(), a.logf())
		},
	}
	cmd.Flags().BoolVar(&relabel, "relabel", false, "reuse a volume already labeled by NBackup")
	cmd.Flags().BoolVar(&force, "force", false, "override safety refusals (foreign data / protected slots)")
	return cmd
}

// newMediumCmd implements `nb medium`: list media (default) or detail one.
func newMediumCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "medium [name]",
		Short:   "List media and their capacity/volumes, or detail one",
		Long:    "List every configured medium with its type, slot count, usage, capacity, and current volume. Pass a medium name to show its volume and the slots it holds.",
		Example: "  nb medium\n  nb medium lto",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadRO()
			if err != nil {
				return err
			}
			// A bare config (no sources) means no config file was found — read-only
			// commands fall back to a synthesized default catalog, so don't present its
			// phantom default disk medium as if storage were configured (matching `nb slot`).
			if len(cfg.Sources) == 0 {
				fmt.Println("no media (no backup config found — copy nbackup.example.yaml to nbackup.yaml and edit it, or pass -c <config>)")
				return nil
			}
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			if len(args) >= 1 {
				return mediumDetail(eng, args[0])
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
	fmt.Fprintln(tw, "MEDIUM\tTYPE\tSLOTS\tUSED\tCAPACITY\tVOLUME")
	for _, m := range media {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", m.Name, m.Type, m.Slots,
			sizeutil.FormatBytes(m.Used)+overMarker(m.Used, m.Capacity), capacityStr(m.Capacity), volumeStr(m))
	}
	tw.Flush()
	return nil
}

// overMarker flags usage that has run past a bounded medium's capacity, so an
// over-capacity medium does not read as healthy in `nb medium` listings (sync/copy
// can land slots past capacity; pruning reclaims them on its own schedule).
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
	printInventory(eng, name)
	fmt.Println()
	slots := eng.Catalog().SlotsOn(name)
	if len(slots) == 0 {
		fmt.Println("no slots on this medium")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SLOT\tSIZE\tARCHIVES\tCOMMITTED")
	for _, s := range slots {
		committed := "-"
		if !s.SealedAt.IsZero() {
			committed = s.SealedAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.ID, sizeutil.FormatBytes(s.TotalBytes), len(s.Archives), committed)
	}
	tw.Flush()
	return nil
}

// printInventory shows a medium's physical inventory beneath its `nb medium`
// detail: a robotic library's bays, or a single-drive station's drive and the
// reels on its shelf. Media with no changer (disk, s3) print nothing.
func printInventory(eng *engine.Engine, name string) {
	view, err := eng.ChangerView(name)
	if err != nil {
		return // address-identified medium: nothing physical to inventory
	}
	appendable := eng.MediumAppendable(name)
	if view.Library {
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "\n\tBAY\tLABEL\tSTATUS\tON VOLUME\tCAPACITY\tFILES")
		for _, b := range view.Bays {
			mark := " "
			if b.ID == view.Loaded {
				mark = "*"
			}
			label, status := volumeLabelStatus(b, name, appendable)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", mark, b.ID, label, status,
				sizeutil.FormatBytes(b.Used), capacityStr(b.Capacity), b.Files)
		}
		tw.Flush()
		return
	}
	if view.DriveOK {
		label, status := volumeLabelStatus(view.Drive, name, appendable)
		fmt.Printf("  drive:   %s (%s, %s on volume, %d files)\n", label, status,
			sizeutil.FormatBytes(view.Drive.Used), view.Drive.Files)
	} else {
		fmt.Println("  drive:   (empty)")
	}
	if len(view.Shelf) > 0 {
		fmt.Println("\nIn the room (load with `nb load`, or when prompted):")
		rw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(rw, "  REEL\tLABEL\tSTATUS\tON VOLUME\tCAPACITY\tFILES")
		for _, b := range view.Shelf {
			label, status := volumeLabelStatus(b, name, appendable)
			fmt.Fprintf(rw, "  %s\t%s\t%s\t%s\t%s\t%d\n", b.ID, label, status,
				sizeutil.FormatBytes(b.Used), capacityStr(b.Capacity), b.Files)
		}
		rw.Flush()
	}
}

func capacityStr(c int64) string {
	if c <= 0 {
		return "unbounded"
	}
	return sizeutil.FormatBytes(c)
}

// volumeLabelStatus renders a volume's display label and fill status for inventory
// listings (a blank volume, a full one, a wrong-pool reel, or an appendable labeled
// one). medium is the medium being inventoried, so a reel labeled for a different
// pool is flagged rather than shown as one of this medium's own volumes.
func volumeLabelStatus(b media.VolumeStatus, medium string, appendable bool) (label, status string) {
	switch {
	case b.Foreign:
		return "(foreign)", "foreign"
	case b.Blank:
		return "(blank)", "blank"
	case b.Pool != "" && b.Pool != medium:
		// A valid NBackup label, but for another pool: the write guard would refuse it
		// (wrong tape), so the inventory must not present it as this medium's own.
		return b.Label, fmt.Sprintf("wrong-pool:%s", b.Pool)
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
		Args:    cobra.ExactArgs(2),
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

// newRebuildCmd implements `nb rebuild`: rebuild the local slot-index cache by
// rescanning the self-describing media.
func newRebuildCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the catalog cache by rescanning media",
		Long:  "Rescan every configured medium and rebuild the local catalog cache (slot index and volume registry) from the commit footers and labels found there.",
		Args:  cobra.NoArgs,
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
			fmt.Printf("catalog cache rebuilt from media: %d slot(s) indexed\n", n)
			return nil
		},
	}
}
