package cli

import (
	"fmt"
	"io"
	"os"
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
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/slot"
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

			plan := eng.Plan(date)
			fmt.Printf("Plan for run %s  (cycle %dd, landing %q)\n\n",
				slot.DateString(date), plan.Interval, eng.Landing())
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
	fmt.Fprintln(tw, "DLE\tLEVEL\tEST. SIZE\tREASON")
	var estTotal int64
	for _, item := range plan.Items {
		levelStr := fmt.Sprintf("L%d (full)", item.Level)
		if item.Level >= 1 {
			levelStr = fmt.Sprintf("L%d (incr)", item.Level)
		}
		fmt.Fprintf(tw, "%s\t%s\t~%s\t%s\n", item.Name, levelStr, sizeutil.FormatBytes(item.EstBytes), item.Reason)
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
		days, slot.DateString(start), plans[0].Interval, eng.Landing())

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
				fullNames = append(fullNames, it.Name)
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
				slot.DateString(p.Date), fulls, incrs, sizeutil.FormatBytes(est),
				formatUSD(curve[i].Monthly), names)
		} else {
			fmt.Fprintf(tw, "%s\t%d\t%d\t~%s\t%s\n",
				slot.DateString(p.Date), fulls, incrs, sizeutil.FormatBytes(est), names)
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

// describeExpectation renders the tape the next run will write to (Amanda's
// "amdump will expect tape X") for `nb plan`, with the volume's age relative to
// the run date.
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
		exp.Label, slot.DateString(exp.WrittenAt), age, detail)
}

// newDumpCmd implements `nb dump`: execute a run and seal a slot, or — with
// --dry-run — plan the run for --date and print it without writing anything.
func newDumpCmd(a *app) *cobra.Command {
	var dateStr string
	var dryRun bool
	cmd := &cobra.Command{
		Use:     "dump",
		Short:   "Execute a run and seal a slot",
		Long:    "Execute a planner run, dumping each scheduled DLE and sealing exactly one immutable slot. Use --quiet to suppress progress output. --date sets the run date (with or without --dry-run). With --dry-run the run for --date is planned against the current catalog and printed, exactly as a real dump would decide it, but nothing is written.",
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
			eng.SetOperator(stdinOperator{})
			s, err := eng.Run(date, a.logf())
			if err != nil {
				return err
			}
			fmt.Printf("\nSealed %s: %d archive(s), %s total\n", s.ID, len(s.Archives), sizeutil.FormatBytes(s.TotalBytes))
			return nil
		},
	}
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today)")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "plan the run for --date and print it without writing anything")
	return cmd
}

// runDumpDryRun previews the dump on `date` without writing: it plans that run
// exactly as `nb dump --date <date>` would — against the current catalog, the same
// decision logic a real run uses — and prints it. Nothing is sealed.
func runDumpDryRun(eng *engine.Engine, date time.Time, validationWarnings []string) error {
	plan := eng.Plan(date)

	fmt.Println("DRY RUN — no data is written.")
	fmt.Printf("This is the run on %s.\n\n", slot.DateString(date))
	warnings := append(validationWarnings, plan.Warnings...)
	for _, w := range warnings {
		fmt.Printf("WARNING: %s\n", w)
	}
	if len(warnings) > 0 {
		fmt.Println()
	}

	estTotal := fprintPlanItems(os.Stdout, plan)
	fmt.Printf("\nThis run (estimated): ~%s\n", sizeutil.FormatBytes(estTotal))
	fmt.Printf("Would seal %s. Run without --dry-run to execute.\n", slot.IDFromParts(slot.DateString(date), 1))
	return nil
}

// newStatusCmd implements `nb status`: show the progress of the current (or most
// recent) run by reading the run-status file `nb dump` writes — NBackup's
// amstatus. It needs no engine, only the catalog workdir, so it is cheap to poll.
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
		Long: "Verify archives against the seal. By default it re-checks payload checksums " +
			"(integrity). With --deep it also streams each archive through the real read " +
			"pipeline — decrypt, decompress, then `tar -t` (list, not extract) — and asserts the " +
			"members match the seal, proving the bytes are a valid restorable stream and " +
			"exercising the key and codec end-to-end. It writes nothing either way. Pass slot ids " +
			"to verify just those, or --all for every slot (which may mount every volume in the pool).",
		Example: "  nb verify slot-2026-06-21\n  nb verify --deep slot-2026-06-21\n  nb verify --all",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return fmt.Errorf("--all cannot be combined with explicit slot ids")
			}
			if !all && len(args) == 0 {
				return fmt.Errorf("specify slot ids to verify, or --all to verify every slot")
			}
			cfg, err := a.loadRO()
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
			eng.SetOperator(stdinOperator{})
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
			report, err := eng.Verify(args, engine.VerifyOptions{Checks: checks}, a.logf())
			if err != nil {
				return err
			}
			if report.Failures > 0 {
				return fmt.Errorf("%d slot(s) failed verification", report.Failures)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "verify every slot in the catalog")
	cmd.Flags().BoolVar(&deep, "deep", false, "also validate structure: decrypt+decompress+`tar -t`, members vs seal")
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
		fmt.Println("no slots in catalog")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SLOT\tSTATUS\tARCHIVES\tSIZE\tSEALED\tCOPIES")
	for _, s := range slots {
		sealed := "-"
		if !s.SealedAt.IsZero() {
			sealed = s.SealedAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", s.ID, s.Status, len(s.Archives),
			sizeutil.FormatBytes(s.TotalBytes), sealed, copiesSummary(eng.Catalog().Placements(s.ID)))
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
		if labeled := placementVolumes(p); len(labeled) > 0 {
			names = append(names, p.Medium+":"+strings.Join(labeled, "+"))
		} else {
			names = append(names, p.Medium)
		}
	}
	return strings.Join(names, ", ")
}

// placementVolumes lists a placement's volume labels that differ from the medium
// name — the labeled tapes a slot's copy spans (empty for address-identified media,
// whose volume label is just the medium name).
func placementVolumes(p catalog.Placement) []string {
	labeled := make([]string, 0, 2)
	for _, v := range p.Volumes() {
		if v != "" && v != p.Medium {
			labeled = append(labeled, v)
		}
	}
	return labeled
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
			fmt.Printf("Slot %s  (%s)\n", s.ID, s.Status)
			fmt.Printf("  date:    %s\n", s.Date)
			fmt.Printf("  sealed:  %s\n", s.SealedAt.Format("2006-01-02 15:04:05 MST"))
			fmt.Printf("  total:   %s\n\n", sizeutil.FormatBytes(s.TotalBytes))
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "DLE\tLEVEL\tFILES\tSIZE\tCODEC\tENCRYPT")
			for _, ar := range s.Archives {
				enc := ar.Encrypt
				if enc == "" {
					enc = "none"
				}
				fmt.Fprintf(tw, "%s\tL%d\t%d\t%s\t%s\t%s\n", ar.DLE, ar.Level, ar.FileCount, sizeutil.FormatBytes(ar.Compressed), ar.Codec, enc)
			}
			tw.Flush()

			placements := eng.Catalog().Placements(s.ID)
			fmt.Printf("\nCOPIES (%d)\n", len(placements))
			ptw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(ptw, "  MEDIUM\tVOLUMES\tPOSITIONS")
			for _, p := range placements {
				volumes := "-"
				if labeled := placementVolumes(p); len(labeled) > 0 {
					volumes = strings.Join(labeled, "+")
				}
				positions := make([]string, 0, len(p.Archives))
				for _, ar := range p.Archives {
					locs := make([]string, 0, len(ar.Parts))
					for _, pt := range ar.Parts {
						locs = append(locs, fmt.Sprintf("%d", pt.Pos))
					}
					positions = append(positions, fmt.Sprintf("%s/L%d@%s", ar.DLE, ar.Level, strings.Join(locs, ",")))
				}
				fmt.Fprintf(ptw, "  %s\t%s\t%s\n", p.Medium, volumes, strings.Join(positions, " "))
			}
			ptw.Flush()
			return nil
		},
	}
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
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			// Dry-run prune only reads; a real run deletes slots, so lock it.
			var eng *engine.Engine
			if !dryRun {
				var unlock func()
				eng, unlock, err = a.lockedEngine(cfg)
				if err != nil {
					return err
				}
				defer unlock()
			} else if eng, err = newEngine(cfg); err != nil {
				return err
			}
			now, err := ParseDate(dateStr)
			if err != nil {
				return err
			}
			eligible, freed, err := eng.Prune(args[0], now, !dryRun, a.logf())
			if err != nil {
				return err
			}
			if !dryRun {
				if eligible > 0 {
					fmt.Printf("\n%s: deleted %d slot(s), freed %s\n", args[0], eligible, sizeutil.FormatBytes(freed))
				} else {
					fmt.Printf("\n%s: nothing to reclaim (all slots fit capacity or are protected)\n", args[0])
				}
			} else if eligible > 0 {
				fmt.Printf("\n%d slot(s) eligible. Re-run without --dry-run to delete.\n", eligible)
			} else {
				fmt.Printf("\nnothing to reclaim: all slots fit capacity or are protected.\n")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview without deleting")
	cmd.Flags().StringVar(&dateStr, "date", "", "reference 'now' date YYYY-MM-DD (default today)")
	return cmd
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
			eng.SetOperator(stdinOperator{})
			if err := eng.CopySlot(slotID, from, to, force, a.logf()); err != nil {
				return err
			}
			fmt.Println("copy complete")
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
	return nil
}

// newSyncCmd implements `nb sync`: the batch form of `nb copy`. It mirrors every
// landing slot a target is missing onto that target (Amanda's vaulting), oldest
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
			var eng *engine.Engine
			if !dryRun {
				var unlock func()
				eng, unlock, err = a.lockedEngine(cfg)
				if err != nil {
					return err
				}
				defer unlock()
				eng.SetOperator(stdinOperator{})
			} else if eng, err = newEngine(cfg); err != nil {
				return err
			}

			for _, t := range targets {
				report, err := eng.SyncTo(t.from, t.name, t.sel, !dryRun, force, a.logf())
				if report != nil {
					printSyncReport(report, !dryRun)
				}
				if err != nil {
					return err
				}
			}
			return nil
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
		fmt.Printf("%s -> %s: copied %d slot(s), %s\n", r.From, r.To, r.Copied(), sizeutil.FormatBytes(r.Bytes()))
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
		Long:    "Write a volume's identity label, making it writable. Refuses to overwrite foreign data, and (with --relabel) a tape that still holds protected slots — those within minimum_age or holding a DLE's last recovery path, including a slot spanned across tapes. --relabel reuses an NBackup-labeled volume and --force overrides safety refusals.",
		Example: "  nb label tape DAILY-01\n  nb label --relabel tape DAILY-01",
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
	fmt.Fprintln(tw, "SLOT\tSIZE\tARCHIVES\tSEALED")
	for _, s := range slots {
		sealed := "-"
		if !s.SealedAt.IsZero() {
			sealed = s.SealedAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.ID, sizeutil.FormatBytes(s.TotalBytes), len(s.Archives), sealed)
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
	if view.Library {
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "\n\tBAY\tLABEL\tSTATUS\tUSED\tCAPACITY\tFILES")
		for _, b := range view.Bays {
			mark := " "
			if b.ID == view.Loaded {
				mark = "*"
			}
			label, status := volumeLabelStatus(b, name)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", mark, b.ID, label, status,
				sizeutil.FormatBytes(b.Used), capacityStr(b.Capacity), b.Files)
		}
		tw.Flush()
		return
	}
	if view.DriveOK {
		label, status := volumeLabelStatus(view.Drive, name)
		fmt.Printf("  drive:   %s (%s, %s used, %d files)\n", label, status,
			sizeutil.FormatBytes(view.Drive.Used), view.Drive.Files)
	} else {
		fmt.Println("  drive:   (empty)")
	}
	if len(view.Shelf) > 0 {
		fmt.Println("\nIn the room (load with `nb load`, or when prompted):")
		rw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(rw, "  REEL\tLABEL\tSTATUS\tUSED\tCAPACITY\tFILES")
		for _, b := range view.Shelf {
			label, status := volumeLabelStatus(b, name)
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
func volumeLabelStatus(b media.VolumeStatus, medium string) (label, status string) {
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
		Long:  "Rescan every configured medium and rebuild the local catalog cache (slot index and volume registry) from the seals and labels found there.",
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
