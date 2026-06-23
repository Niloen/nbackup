package cli

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/slot"
)

// CmdPlan implements `nbplan`: show what the next run would do.
func CmdPlan(args []string) error {
	fs := flag.NewFlagSet("nbplan", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	dateStr := fs.String("date", "", "run date YYYY-MM-DD (default today)")
	fs.Parse(args)

	cfg, err := loadConfig(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	date, err := ParseDate(*dateStr)
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

	current := eng.Catalog().TotalBytes()
	capacity := eng.Capacity()
	fmt.Printf("\nCatalog currently stored: %s\n", sizeutil.FormatBytes(current))
	fmt.Printf("This run (estimated): ~%s\n", sizeutil.FormatBytes(estTotal))
	if capacity > 0 {
		over, pct := eng.BudgetStatus(current)
		fmt.Printf("Capacity: %s (%.1f%% used)\n", sizeutil.FormatBytes(capacity), pct)
		if over {
			fmt.Printf("WARNING: over capacity; run `nbslot prune` to reclaim oldest slots\n")
		}
	} else {
		fmt.Printf("Capacity: unbounded\n")
	}
	return nil
}

// CmdDump implements `nbdump`: execute a run and seal a slot.
func CmdDump(args []string) error {
	fs := flag.NewFlagSet("nbdump", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	dateStr := fs.String("date", "", "run date YYYY-MM-DD (default today)")
	quiet := fs.Bool("q", false, "suppress progress output")
	fs.Parse(args)

	cfg, err := loadConfig(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	date, err := ParseDate(*dateStr)
	if err != nil {
		return err
	}

	var logf engine.Logf
	if !*quiet {
		logf = logfStdout
	}
	s, err := eng.Run(date, logf)
	if err != nil {
		return err
	}
	fmt.Printf("\nSealed %s: %d archive(s), %s total\n", s.ID, len(s.Archives), sizeutil.FormatBytes(s.TotalBytes))
	return nil
}

// CmdVerify implements `nbverify`: check archive checksums of one or all slots.
func CmdVerify(args []string) error {
	fs := flag.NewFlagSet("nbverify", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	fs.Parse(args)

	cfg, err := loadConfigRO(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	failures, err := eng.Verify(fs.Args(), logfStdout)
	if err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("%d slot(s) failed verification", failures)
	}
	return nil
}

// CmdSlot implements `nbslot`: list slots, show a slot, or prune.
func CmdSlot(args []string) error {
	if len(args) > 0 && args[0] == "prune" {
		return cmdPrune(args[1:])
	}
	if len(args) > 0 && args[0] == "show" {
		return cmdSlotShow(args[1:])
	}
	return cmdSlotList(args)
}

func cmdSlotList(args []string) error {
	fs := flag.NewFlagSet("nbslot", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	fs.Parse(args)

	cfg, err := loadConfigRO(*cfgPath, *catalogFlag)
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

func cmdSlotShow(args []string) error {
	fs := flag.NewFlagSet("nbslot show", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	fs.Parse(args)

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: nbslot show <slot-id>")
	}
	cfg, err := loadConfigRO(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	s, err := eng.Catalog().ReadSlot(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("Slot %s  (%s)\n", s.ID, s.Status)
	fmt.Printf("  date:    %s\n", s.Date)
	fmt.Printf("  sealed:  %s\n", s.SealedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("  total:   %s\n\n", sizeutil.FormatBytes(s.TotalBytes))
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DLE\tLEVEL\tFILES\tSIZE\tCODEC")
	for _, a := range s.Archives {
		fmt.Fprintf(tw, "%s\tL%d\t%d\t%s\t%s\n", a.DLE, a.Level, a.FileCount, sizeutil.FormatBytes(a.Compressed), a.Codec)
	}
	tw.Flush()
	return nil
}

func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("nbslot prune", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	apply := fs.Bool("apply", false, "actually delete (default is dry-run)")
	dateStr := fs.String("date", "", "reference 'now' date YYYY-MM-DD (default today)")
	fs.Parse(args)

	cfg, err := loadConfig(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	now, err := ParseDate(*dateStr)
	if err != nil {
		return err
	}
	eligible, err := eng.Prune(now, *apply, logfStdout)
	if err != nil {
		return err
	}
	if !*apply && eligible > 0 {
		fmt.Printf("\n%d slot(s) eligible. Re-run with --apply to delete.\n", eligible)
	}
	return nil
}

// CmdCopy implements `nb copy`: stream a slot from the landing medium to another
// configured medium (e.g. disk -> tape).
func CmdCopy(args []string) error {
	fs := flag.NewFlagSet("nb copy", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	to := fs.String("to", "", "destination medium name (required)")
	fs.Parse(args)

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: nb copy --to <medium> <slot-id>")
	}
	if *to == "" {
		return fmt.Errorf("--to <medium> is required")
	}
	cfg, err := loadConfig(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	if err := eng.CopySlot(fs.Arg(0), *to, logfStdout); err != nil {
		return err
	}
	fmt.Println("copy complete")
	return nil
}

// CmdLabel implements `nb label`: write (or rewrite) a volume's identity label.
// This is the deliberate act that makes a tape writable; it guards against
// overwriting foreign data or a still-active volume.
func CmdLabel(args []string) error {
	fs := flag.NewFlagSet("nb label", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	relabel := fs.Bool("relabel", false, "reuse a volume already labeled by NBackup")
	force := fs.Bool("force", false, "override safety refusals (foreign data / still-active volume)")
	fs.Parse(args)

	if fs.NArg() < 2 {
		return fmt.Errorf("usage: nb label [--relabel] [--force] <medium> <name>")
	}
	cfg, err := loadConfig(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	return eng.LabelVolume(fs.Arg(0), fs.Arg(1), *relabel, *force, time.Now().UTC(), logfStdout)
}

// CmdCatalog implements `nbcatalog`: maintain the local slot-index cache.
func CmdCatalog(args []string) error {
	if len(args) == 0 || args[0] != "rebuild" {
		return fmt.Errorf("usage: nbcatalog rebuild [-c config] [-C catalog]")
	}
	fs := flag.NewFlagSet("nbcatalog rebuild", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	fs.Parse(args[1:])

	cfg, err := loadConfigRO(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	n, err := eng.RebuildCatalog()
	if err != nil {
		return err
	}
	fmt.Printf("catalog cache rebuilt from media: %d slot(s) indexed\n", n)
	return nil
}

// CmdRestore implements `nbrestore`: rebuild a DLE (or all DLEs) from a slot.
func CmdRestore(args []string) error {
	fs := flag.NewFlagSet("nbrestore", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	dleName := fs.String("dle", "", "DLE name to restore (default: all DLEs in the slot)")
	dest := fs.String("dest", "", "destination directory (required)")
	fs.Parse(args)

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: nbrestore [-dle NAME] -dest DIR <slot-id>")
	}
	if *dest == "" {
		return fmt.Errorf("-dest is required")
	}
	cfg, err := loadConfigRO(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	slotID := fs.Arg(0)
	s, err := eng.Catalog().ReadSlot(slotID)
	if err != nil {
		return err
	}

	var dles []string
	if *dleName != "" {
		dles = []string{*dleName}
	} else {
		dles = eng.DLEsInSlot(s)
	}

	for _, name := range dles {
		out := *dest
		if len(dles) > 1 {
			out = fmt.Sprintf("%s/%s", *dest, name)
		}
		fmt.Printf("restoring DLE %s as of %s -> %s\n", name, slotID, out)
		if err := eng.Restore(slotID, name, out, logfStdout); err != nil {
			return err
		}
	}
	fmt.Println("restore complete")
	return nil
}
