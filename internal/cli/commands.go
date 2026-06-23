package cli

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

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
				slot.DateString(date), plan.Interval, sizeutil.FormatBytes(plan.Target), cfg.Landing)
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
			return nil
		},
	}
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today)")
	return cmd
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
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
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
	fmt.Fprintln(tw, "SLOT\tSTATUS\tARCHIVES\tSIZE\tSEALED")
	for _, s := range slots {
		sealed := "-"
		if !s.SealedAt.IsZero() {
			sealed = s.SealedAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", s.ID, s.Status, len(s.Archives), sizeutil.FormatBytes(s.TotalBytes), sealed)
	}
	tw.Flush()
	return nil
}

func newSlotShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "show <slot-id>",
		Short:   "Show a single slot's archives",
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
			eng, err := newEngine(cfg)
			if err != nil {
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
	var to string
	cmd := &cobra.Command{
		Use:     "copy <slot-id>",
		Short:   "Copy a slot to another medium (e.g. disk -> tape)",
		Long:    "Stream a slot from the landing medium to another configured medium. The destination medium is selected with --to.",
		Example: "  nb copy --to tape slot-2026-06-21",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			if err := eng.CopySlot(args[0], to, a.logf()); err != nil {
				return err
			}
			fmt.Println("copy complete")
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "destination medium name (required)")
	cmd.MarkFlagRequired("to")
	return cmd
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
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			return eng.LabelVolume(args[0], args[1], relabel, force, time.Now().UTC(), a.logf())
		},
	}
	cmd.Flags().BoolVar(&relabel, "relabel", false, "reuse a volume already labeled by NBackup")
	cmd.Flags().BoolVar(&force, "force", false, "override safety refusals (foreign data / still-active volume)")
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
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
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
