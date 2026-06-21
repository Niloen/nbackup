package cli

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

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
	noEstimate := fs.Bool("no-estimate", false, "skip scanning sources for size estimates")
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
	fmt.Printf("Plan for run %s  (full interval %dd, catalog %s)\n\n", slot.DateString(date), plan.Interval, cfg.CatalogPath())

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DLE\tLEVEL\tEST. SIZE\tREASON")
	var estTotal int64
	for _, item := range plan.Items {
		levelStr := fmt.Sprintf("L%d (full)", item.Level)
		if item.Level >= 1 {
			levelStr = fmt.Sprintf("L%d (incr)", item.Level)
		}
		estStr := "-"
		if !*noEstimate {
			if n, err := eng.Estimate(item); err == nil {
				estStr = "~" + sizeutil.FormatBytes(n) + " raw"
				estTotal += n
			} else {
				estStr = "unreadable"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", item.Name, levelStr, estStr, item.Reason)
	}
	tw.Flush()

	current, err := eng.Catalog().TotalBytes()
	if err != nil {
		return err
	}
	budget, _ := cfg.BudgetBytes()
	fmt.Printf("\nCatalog currently stored: %s\n", sizeutil.FormatBytes(current))
	if !*noEstimate {
		fmt.Printf("This run (raw, pre-compression): ~%s\n", sizeutil.FormatBytes(estTotal))
	}
	if budget > 0 {
		over, pct := eng.Policy().BudgetStatus(current)
		fmt.Printf("Budget: %s (%.1f%% used)\n", sizeutil.FormatBytes(budget), pct)
		if over {
			fmt.Printf("WARNING: catalog is over budget; run `nbslot prune` to retire old slots\n")
		}
	} else {
		fmt.Printf("Budget: not set\n")
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

	eng, err := newEngine(loadConfigRO(*cfgPath, *catalogFlag))
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

	eng, err := newEngine(loadConfigRO(*cfgPath, *catalogFlag))
	if err != nil {
		return err
	}
	slots, err := eng.Catalog().Slots()
	if err != nil {
		return err
	}
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
	eng, err := newEngine(loadConfigRO(*cfgPath, *catalogFlag))
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
	fmt.Fprintln(tw, "DLE\tLEVEL\tFILES\tSIZE\tFILE")
	for _, a := range s.Archives {
		fmt.Fprintf(tw, "%s\tL%d\t%d\t%s\t%s\n", a.DLE, a.Level, a.FileCount, sizeutil.FormatBytes(a.Compressed), a.File)
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
	eng, err := newEngine(loadConfigRO(*cfgPath, *catalogFlag))
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
