package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/slot"
)

// newPlanCmd implements `nb plan`: show what the next run would do.
func newPlanCmd(a *app) *cobra.Command {
	var dateStr string
	cmd := &cobra.Command{
		Use:     "plan",
		Short:   "Show what the next run would do",
		Long:    "Preview the next run: which DLEs would be dumped at which level, estimated sizes, and capacity/budget status. Reads only; nothing is written.",
		Example: "  nb plan\n  nb plan --date 2026-06-21",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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

			plan := eng.Plan(date)
			fmt.Printf("Plan for run %s  (cycle %dd, balance target ~%s/run, landing %q)\n\n",
				slot.DateString(date), plan.Interval, sizeutil.FormatBytes(plan.Target), eng.Landing())
			for _, w := range plan.Warnings {
				fmt.Printf("WARNING: %s\n", w)
			}
			if len(plan.Warnings) > 0 {
				fmt.Println()
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
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

			current := eng.StoredBytes()
			capacity := eng.Capacity()
			fmt.Printf("\nCatalog currently stored: %s\n", sizeutil.FormatBytes(current))
			fmt.Printf("This run (estimated): ~%s\n", sizeutil.FormatBytes(estTotal))
			if capacity > 0 {
				over, pct := eng.BudgetStatus(current)
				fmt.Printf("Capacity: %s (%.1f%% used)\n", sizeutil.FormatBytes(capacity), pct)
				if over {
					fmt.Printf("WARNING: over capacity; run `nb slot prune` to reclaim oldest slots\n")
				}
			} else {
				fmt.Printf("Capacity: unbounded\n")
			}
			if exp, ok := eng.ExpectedTape(date); ok {
				fmt.Printf("Tape: %s\n", describeExpectation(exp, date))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today)")
	return cmd
}

// describeExpectation renders the tape the next run will write to (Amanda's
// "amdump will expect tape X") for `nb plan`, with the volume's age relative to
// the run date.
func describeExpectation(exp engine.TapeExpectation, date time.Time) string {
	if exp.Appendable {
		if exp.NewTape {
			return fmt.Sprintf("expects a fresh tape (pool %q is empty)", exp.Medium)
		}
		if exp.VolumeBytes > 0 {
			return fmt.Sprintf("appends to %q (%s of %s used, %s free on this reel)",
				exp.Label, sizeutil.FormatBytes(exp.UsedBytes), sizeutil.FormatBytes(exp.VolumeBytes),
				sizeutil.FormatBytes(exp.VolumeBytes-exp.UsedBytes))
		}
		return fmt.Sprintf("appends to %q", exp.Label)
	}
	if exp.NewTape {
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

// newDumpCmd implements `nb dump`: execute a run and seal a slot.
func newDumpCmd(a *app) *cobra.Command {
	var dateStr string
	cmd := &cobra.Command{
		Use:     "dump",
		Short:   "Execute a run and seal a slot",
		Long:    "Execute a planner run, dumping each scheduled DLE and sealing exactly one immutable slot. Use --quiet to suppress progress output.",
		Example: "  nb dump\n  nb dump --date 2026-06-21\n  nb -c prod.yaml dump -q",
		Args:    cobra.NoArgs,
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
			eng.SetOperator(stdinOperator{})
			date, err := ParseDate(dateStr)
			if err != nil {
				return err
			}
			s, err := eng.Run(date, a.logf())
			if err != nil {
				return err
			}
			fmt.Printf("\nSealed %s: %d archive(s), %s total\n", s.ID, len(s.Archives), sizeutil.FormatBytes(s.TotalBytes))
			return nil
		},
	}
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today)")
	return cmd
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

// newVerifyCmd implements `nb verify`: check archive checksums of one or all slots.
func newVerifyCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "verify [slot-id...]",
		Short:   "Verify slot checksums",
		Long:    "Re-check archive checksums against the catalog. With no arguments every slot is verified; otherwise only the named slots.",
		Example: "  nb verify\n  nb verify slot-2026-06-21",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadRO()
			if err != nil {
				return err
			}
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			failures, err := eng.Verify(args, a.logf())
			if err != nil {
				return err
			}
			if failures > 0 {
				return fmt.Errorf("%d slot(s) failed verification", failures)
			}
			return nil
		},
	}
}

// newSlotCmd implements `nb slot`: list slots (default), show a slot, or prune.
func newSlotCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "slot",
		Short: "List, show, or prune slots",
		Long:  "Inspect the slot catalog. With no subcommand it lists slots; see the subcommands to show a single slot or prune expired ones.",
		Args:  cobra.NoArgs,
		// Bare `nb slot` lists slots, preserving prior behavior.
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSlotList(a)
		},
	}
	cmd.AddCommand(newSlotListCmd(a), newSlotShowCmd(a), newSlotPruneCmd(a))
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
		if p.Volume != "" && p.Volume != p.Medium {
			names = append(names, p.Medium+":"+p.Volume)
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
		Args:    cobra.ExactArgs(1),
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
			fmt.Fprintln(tw, "DLE\tLEVEL\tFILES\tSIZE\tCODEC")
			for _, ar := range s.Archives {
				fmt.Fprintf(tw, "%s\tL%d\t%d\t%s\t%s\n", ar.DLE, ar.Level, ar.FileCount, sizeutil.FormatBytes(ar.Compressed), ar.Codec)
			}
			tw.Flush()

			placements := eng.Catalog().Placements(s.ID)
			fmt.Printf("\nCOPIES (%d)\n", len(placements))
			ptw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(ptw, "  MEDIUM\tVOLUME\tEPOCH\tPOSITIONS")
			for _, p := range placements {
				volume, epoch := "-", "-"
				if p.Volume != "" && p.Volume != p.Medium {
					volume = p.Volume
					epoch = fmt.Sprintf("%d", p.Epoch)
				}
				positions := make([]string, 0, len(p.Archives))
				for _, ar := range p.Archives {
					positions = append(positions, fmt.Sprintf("%s/L%d@%d", ar.DLE, ar.Level, ar.Pos))
				}
				fmt.Fprintf(ptw, "  %s\t%s\t%s\t%s\n", p.Medium, volume, epoch, strings.Join(positions, " "))
			}
			ptw.Flush()
			return nil
		},
	}
}

func newSlotPruneCmd(a *app) *cobra.Command {
	var apply bool
	var dateStr string
	cmd := &cobra.Command{
		Use:     "prune",
		Short:   "Delete slots past the cycle/capacity limits",
		Long:    "Reclaim slots that fall outside the cycle and per-medium capacity limits. Dry-run by default; pass --apply to actually delete.",
		Example: "  nb slot prune\n  nb slot prune --apply",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			// Dry-run prune only reads; --apply deletes slots, so lock it.
			var eng *engine.Engine
			if apply {
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
			eligible, err := eng.Prune(now, apply, a.logf())
			if err != nil {
				return err
			}
			if !apply && eligible > 0 {
				fmt.Printf("\n%d slot(s) eligible. Re-run with --apply to delete.\n", eligible)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "actually delete (default is dry-run)")
	cmd.Flags().StringVar(&dateStr, "date", "", "reference 'now' date YYYY-MM-DD (default today)")
	return cmd
}

// newCopyCmd implements `nb copy`: stream a slot from the landing medium to
// another configured medium (e.g. disk -> tape).
func newCopyCmd(a *app) *cobra.Command {
	var from, to string
	var force bool
	cmd := &cobra.Command{
		Use:     "copy <slot-id>",
		Short:   "Copy a slot from one medium to another (e.g. disk -> tape)",
		Long:    "Stream a slot from one configured medium to another. The destination is selected with --to; the source defaults to the landing medium and is overridden with --from (e.g. un-vault tape -> disk).",
		Example: "  nb copy --to tape slot-2026-06-21\n  nb copy --from tape --to disk slot-2026-06-21",
		Args:    cobra.ExactArgs(1),
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
			eng.SetOperator(stdinOperator{})
			if err := eng.CopySlot(args[0], from, to, force, a.logf()); err != nil {
				return err
			}
			fmt.Println("copy complete")
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "destination medium name (required)")
	cmd.Flags().StringVar(&from, "from", "", "source medium name (default: the landing medium)")
	cmd.Flags().BoolVar(&force, "force", false, "re-copy even if the slot is already recorded on the target medium")
	cmd.MarkFlagRequired("to")
	return cmd
}

// newSyncCmd implements `nb sync`: the batch form of `nb copy`. It mirrors every
// landing slot a target is missing onto that target (Amanda's vaulting), oldest
// first. With --to it syncs one ad-hoc target; without --to it runs the rules in
// the config's `sync:` block. Dry-run by default (like `nb slot prune`).
func newSyncCmd(a *app) *cobra.Command {
	var from, to, sinceStr string
	var last int
	var apply, force bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Mirror one medium's slots onto another (e.g. disk -> tape/s3)",
		Long: "Copy every slot the target medium is missing from a source medium, oldest " +
			"first. The batch, idempotent form of `nb copy`: an interrupted or repeated sync " +
			"resumes, copying only what is not yet on the target. The source defaults to the " +
			"landing medium and is overridden with --from. With --to it syncs one target; " +
			"without --to it runs the `sync:` rules from the config. Dry-run by default; pass " +
			"--apply to actually copy.",
		Example: "  nb sync\n  nb sync --to lto\n  nb sync --to glacier --last 4 --apply\n  nb sync --from lto --to disk --apply",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			since, err := ParseDate(sinceStr)
			if err != nil {
				return err
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

			// Dry-run only reads; --apply writes media + catalog, so lock it.
			var eng *engine.Engine
			if apply {
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
				report, err := eng.SyncTo(t.from, t.name, t.sel, apply, force, a.logf())
				if report != nil {
					printSyncReport(report, apply)
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
	cmd.Flags().BoolVar(&apply, "apply", false, "actually copy (default is dry-run)")
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
		return
	}
	fmt.Printf("%s -> %s: %d slot(s) to copy, %s (dry-run; --apply to copy):\n",
		r.From, r.To, len(r.Items), sizeutil.FormatBytes(r.Bytes()))
	for _, it := range r.Items {
		fmt.Printf("  %-24s %2d archive(s)  %s\n", it.SlotID, it.Archives, sizeutil.FormatBytes(it.Bytes))
	}
}

// newLabelCmd implements `nb label`: write (or rewrite) a volume's identity
// label. This is the deliberate act that makes a tape writable; it guards
// against overwriting foreign data or a still-active volume.
func newLabelCmd(a *app) *cobra.Command {
	var relabel, force bool
	cmd := &cobra.Command{
		Use:     "label <medium> <name>",
		Short:   "Label a volume (required for tape before first dump)",
		Long:    "Write a volume's identity label, making it writable. Refuses to overwrite foreign data or a still-active volume; --relabel reuses an NBackup-labeled volume and --force overrides safety refusals.",
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
	cmd.Flags().BoolVar(&force, "force", false, "override safety refusals (foreign data / still-active volume)")
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
			sizeutil.FormatBytes(m.Used), capacityStr(m.Capacity), volumeStr(m))
	}
	tw.Flush()
	return nil
}

func mediumDetail(eng *engine.Engine, name string) error {
	m, ok := eng.Medium(name)
	if !ok {
		return fmt.Errorf("unknown medium %q", name)
	}
	fmt.Printf("Medium %s  (%s)\n", m.Name, m.Type)
	fmt.Printf("  volume:  %s\n", volumeStr(m))
	fmt.Printf("  used:    %s / %s\n", sizeutil.FormatBytes(m.Used), capacityStr(m.Capacity))
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
			label, status := volumeLabelStatus(b)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", mark, b.ID, label, status,
				sizeutil.FormatBytes(b.Used), capacityStr(b.Capacity), b.Files)
		}
		tw.Flush()
		return
	}
	if view.DriveOK {
		label, status := volumeLabelStatus(view.Drive)
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
			label, status := volumeLabelStatus(b)
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
// listings (a blank volume, a full one, or an appendable labeled one).
func volumeLabelStatus(b media.VolumeStatus) (label, status string) {
	switch {
	case b.Blank:
		return "(blank)", "blank"
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

// newCatalogCmd implements `nb catalog`: maintain the local slot-index cache.
func newCatalogCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Maintain the local slot-index cache",
		Long:  "The catalog is a local cache of slot metadata. See the subcommands to rebuild it from the self-describing media.",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newCatalogRebuildCmd(a))
	return cmd
}

func newCatalogRebuildCmd(a *app) *cobra.Command {
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

// newRestoreCmd implements `nb restore`: rebuild a DLE (or all DLEs) from a slot.
func newRestoreCmd(a *app) *cobra.Command {
	var dleName, dest string
	cmd := &cobra.Command{
		Use:     "restore <slot-id>",
		Short:   "Restore a DLE from a slot",
		Long:    "Rebuild a DLE as of a slot into a destination directory. With no --dle every DLE in the slot is restored, each into its own subdirectory of --dest.",
		Example: "  nb restore --dle app01-home --dest /tmp/out slot-2026-06-21",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadRO()
			if err != nil {
				return err
			}
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			eng.SetOperator(stdinOperator{})
			slotID := args[0]
			s, err := eng.Catalog().ReadSlot(slotID)
			if err != nil {
				return err
			}

			var dles []string
			if dleName != "" {
				dles = []string{dleName}
			} else {
				dles = eng.DLEsInSlot(s)
			}

			for _, name := range dles {
				out := dest
				if len(dles) > 1 {
					out = fmt.Sprintf("%s/%s", dest, name)
				}
				fmt.Printf("restoring DLE %s as of %s -> %s\n", name, slotID, out)
				if err := eng.Restore(slotID, name, out, a.logf()); err != nil {
					return err
				}
			}
			fmt.Println("restore complete")
			return nil
		},
	}
	cmd.Flags().StringVar(&dleName, "dle", "", "DLE name to restore (default: all DLEs in the slot)")
	cmd.Flags().StringVar(&dest, "dest", "", "destination directory (required)")
	cmd.MarkFlagRequired("dest")
	return cmd
}
