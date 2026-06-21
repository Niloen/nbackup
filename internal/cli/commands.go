package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/Niloen/nbackup/internal/archive"
	"github.com/Niloen/nbackup/internal/backup"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/restore"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/slot"
	"github.com/Niloen/nbackup/internal/state"
)

// CmdPlan implements `nbplan`: show what the next run would do.
func CmdPlan(args []string) error {
	fs := flag.NewFlagSet("nbplan", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	dateStr := fs.String("date", "", "run date YYYY-MM-DD (default today)")
	noEstimate := fs.Bool("no-estimate", false, "skip scanning sources for size estimates")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	catalog := ResolveCatalog(*catalogFlag, cfg)
	st, err := state.Load(catalog)
	if err != nil {
		return err
	}
	date, err := ParseDate(*dateStr)
	if err != nil {
		return err
	}

	plan := planner.Build(cfg, st, date)
	fmt.Printf("Plan for run %s  (full interval %dd, catalog %s)\n\n", slot.DateString(date), plan.Interval, catalog)

	if !*noEstimate {
		if err := archive.CheckTar(cfg.TarPath()); err != nil {
			fmt.Printf("(size estimates disabled: %v)\n\n", err)
			*noEstimate = true
		}
	}

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
			eo := archive.CreateOptions{
				Tar:        cfg.TarPath(),
				SourcePath: item.Source.Path,
				Level:      item.Level,
			}
			if item.Level >= 1 {
				eo.BaseSnapshot = state.SnapshotPath(catalog, item.Name, item.BaseLevel)
			}
			if n, err := archive.Estimate(eo); err == nil {
				estStr = "~" + sizeutil.FormatBytes(n) + " raw"
				estTotal += n
			} else {
				estStr = "unreadable"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", item.Name, levelStr, estStr, item.Reason)
	}
	tw.Flush()

	// Budget reporting.
	current, err := CatalogBytes(catalog)
	if err != nil {
		return err
	}
	budget, err := cfg.BudgetBytes()
	if err != nil {
		return err
	}
	fmt.Printf("\nCatalog currently stored: %s\n", sizeutil.FormatBytes(current))
	if !*noEstimate {
		fmt.Printf("This run (raw, pre-compression): ~%s\n", sizeutil.FormatBytes(estTotal))
	}
	if budget > 0 {
		pct := float64(current) / float64(budget) * 100
		fmt.Printf("Budget: %s (%.1f%% used)\n", sizeutil.FormatBytes(budget), pct)
		if current > budget {
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

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	catalog := ResolveCatalog(*catalogFlag, cfg)
	if err := os.MkdirAll(catalog, 0o755); err != nil {
		return err
	}
	st, err := state.Load(catalog)
	if err != nil {
		return err
	}
	date, err := ParseDate(*dateStr)
	if err != nil {
		return err
	}

	plan := planner.Build(cfg, st, date)
	logf := func(f string, a ...any) { fmt.Printf(f+"\n", a...) }
	if *quiet {
		logf = nil
	}
	s, err := backup.Run(cfg, st, plan, backup.Options{Catalog: catalog, Logf: logf})
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

	catalog := resolveCatalogMaybeConfig(*catalogFlag, *cfgPath)
	targets := fs.Args()

	var slotIDs []string
	if len(targets) > 0 {
		slotIDs = targets
	} else {
		slots, err := slot.List(catalog)
		if err != nil {
			return err
		}
		for _, s := range slots {
			slotIDs = append(slotIDs, s.ID)
		}
	}
	if len(slotIDs) == 0 {
		fmt.Println("no slots to verify")
		return nil
	}

	failures := 0
	for _, id := range slotIDs {
		dir := SlotDir(catalog, id)
		sums, err := slot.ReadChecksums(dir)
		if err != nil {
			fmt.Printf("%s: ERROR reading checksums: %v\n", id, err)
			failures++
			continue
		}
		ok := true
		for rel, want := range sums {
			got, err := archive.HashFile(filepath.Join(dir, rel))
			if err != nil {
				fmt.Printf("%s: %s MISSING (%v)\n", id, rel, err)
				ok = false
				continue
			}
			if got != want {
				fmt.Printf("%s: %s CHECKSUM MISMATCH\n", id, rel)
				ok = false
			}
		}
		if ok {
			fmt.Printf("%s: OK (%d archive(s))\n", id, len(sums))
		} else {
			failures++
		}
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

	catalog := resolveCatalogMaybeConfig(*catalogFlag, *cfgPath)
	slots, err := slot.List(catalog)
	if err != nil {
		return err
	}
	if len(slots) == 0 {
		fmt.Printf("no slots in catalog %s\n", catalog)
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

	catalog := resolveCatalogMaybeConfig(*catalogFlag, *cfgPath)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: nbslot show <slot-id>")
	}
	id := fs.Arg(0)
	s, err := slot.Read(SlotDir(catalog, id))
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

// cmdPrune retires slots that are outside the cycle while preserving
// recoverability: it never deletes a slot newer than the configured minimum age,
// and never deletes a full that later incrementals still depend on.
func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("nbslot prune", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	apply := fs.Bool("apply", false, "actually delete (default is dry-run)")
	dateStr := fs.String("date", "", "reference 'now' date YYYY-MM-DD (default today)")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	catalog := ResolveCatalog(*catalogFlag, cfg)
	now, err := ParseDate(*dateStr)
	if err != nil {
		return err
	}
	minAge, err := cfg.MinimumAge()
	if err != nil {
		return err
	}

	slots, err := slot.List(catalog)
	if err != nil {
		return err
	}

	// A slot is a deletion candidate only if every DLE it contains still has a
	// complete, newer recovery path (a later full chain). We approximate this:
	// a slot may be retired if it is older than minAge AND a newer full exists
	// for every DLE it holds.
	deletable := func(target *slot.Slot) (bool, string) {
		date, _ := slot.ParseDateField(target.Date)
		if minAge > 0 && now.Sub(date) < minAge {
			return false, fmt.Sprintf("within minimum age (%s)", cfg.Cycle.MinimumAge)
		}
		for _, a := range target.Archives {
			if !hasNewerFull(slots, a.DLE, target) {
				return false, fmt.Sprintf("no newer full for DLE %s (last recovery path)", a.DLE)
			}
		}
		return true, ""
	}

	pruned := 0
	for _, s := range slots {
		ok, reason := deletable(s)
		if !ok {
			fmt.Printf("keep   %s  (%s)\n", s.ID, reason)
			continue
		}
		if *apply {
			if err := os.RemoveAll(SlotDir(catalog, s.ID)); err != nil {
				return fmt.Errorf("delete %s: %w", s.ID, err)
			}
			fmt.Printf("DELETE %s  (%s freed)\n", s.ID, sizeutil.FormatBytes(s.TotalBytes))
		} else {
			fmt.Printf("would delete %s  (%s)\n", s.ID, sizeutil.FormatBytes(s.TotalBytes))
		}
		pruned++
	}
	if !*apply && pruned > 0 {
		fmt.Printf("\n%d slot(s) eligible. Re-run with --apply to delete.\n", pruned)
	}
	return nil
}

func hasNewerFull(slots []*slot.Slot, dle string, target *slot.Slot) bool {
	for _, s := range slots {
		if !slot.Less(target, s) {
			continue // s must come strictly after target in run order
		}
		for _, a := range s.Archives {
			if a.DLE == dle && a.Level == 0 {
				return true
			}
		}
	}
	return false
}

// CmdRestore implements `nbrestore`: rebuild a DLE (or all DLEs) from a slot.
func CmdRestore(args []string) error {
	fs := flag.NewFlagSet("nbrestore", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	dle := fs.String("dle", "", "DLE name to restore (default: all DLEs in the slot)")
	dest := fs.String("dest", "", "destination directory (required)")
	fs.Parse(args)

	catalog := resolveCatalogMaybeConfig(*catalogFlag, *cfgPath)
	tarBin := resolveTar(*cfgPath)
	if err := archive.CheckTar(tarBin); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: nbrestore [-dle NAME] -dest DIR <slot-id>")
	}
	if *dest == "" {
		return fmt.Errorf("-dest is required")
	}
	slotID := fs.Arg(0)
	s, err := slot.Read(SlotDir(catalog, slotID))
	if err != nil {
		return err
	}

	var dles []string
	if *dle != "" {
		dles = []string{*dle}
	} else {
		seen := map[string]bool{}
		for _, a := range s.Archives {
			if !seen[a.DLE] {
				seen[a.DLE] = true
				dles = append(dles, a.DLE)
			}
		}
	}

	logf := func(f string, a ...any) { fmt.Printf(f+"\n", a...) }
	for _, name := range dles {
		out := *dest
		if len(dles) > 1 {
			out = filepath.Join(*dest, name)
		}
		if err := os.MkdirAll(out, 0o755); err != nil {
			return err
		}
		fmt.Printf("restoring DLE %s as of %s -> %s\n", name, slotID, out)
		if err := restore.Run(tarBin, catalog, name, slotID, out, logf); err != nil {
			return err
		}
	}
	fmt.Println("restore complete")
	return nil
}

// resolveCatalogMaybeConfig resolves the catalog, tolerating a missing config
// file (read-only commands should still work without one when -C is given).
func resolveCatalogMaybeConfig(catalogFlag, cfgPath string) string {
	if catalogFlag != "" {
		return catalogFlag
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return DefaultCatalog
	}
	return ResolveCatalog("", cfg)
}

// resolveTar returns the GNU tar binary from config, defaulting when the config
// is absent.
func resolveTar(cfgPath string) string {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return archive.DefaultTar
	}
	return cfg.TarPath()
}
