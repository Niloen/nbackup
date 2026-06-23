package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
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
	parseArgs(fs, args)

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

	current := eng.StoredBytes()
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
	parseArgs(fs, args)

	cfg, err := loadConfig(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	eng.SetOperator(stdinOperator{})
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
	slotIDs := parseArgs(fs, args)

	cfg, err := loadConfigRO(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	failures, err := eng.Verify(slotIDs, logfStdout)
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
	parseArgs(fs, args)

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

func cmdSlotShow(args []string) error {
	fs := flag.NewFlagSet("nbslot show", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	pos := parseArgs(fs, args)

	if len(pos) < 1 {
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
	s, err := eng.Catalog().ReadSlot(pos[0])
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
		for _, a := range p.Archives {
			positions = append(positions, fmt.Sprintf("%s/L%d@%d", a.DLE, a.Level, a.Pos))
		}
		fmt.Fprintf(ptw, "  %s\t%s\t%s\t%s\n", p.Medium, volume, epoch, strings.Join(positions, " "))
	}
	ptw.Flush()
	return nil
}

func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("nbslot prune", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	apply := fs.Bool("apply", false, "actually delete (default is dry-run)")
	dateStr := fs.String("date", "", "reference 'now' date YYYY-MM-DD (default today)")
	parseArgs(fs, args)

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
	force := fs.Bool("force", false, "re-copy even if the slot is already recorded on the target medium")
	pos := parseArgs(fs, args)

	if len(pos) < 1 {
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
	eng.SetOperator(stdinOperator{})
	if err := eng.CopySlot(pos[0], *to, *force, logfStdout); err != nil {
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
	pos := parseArgs(fs, args)

	if len(pos) < 2 {
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
	return eng.LabelVolume(pos[0], pos[1], *relabel, *force, time.Now().UTC(), logfStdout)
}

// CmdCatalog implements `nbcatalog`: maintain the local slot-index cache.
func CmdCatalog(args []string) error {
	if len(args) == 0 || args[0] != "rebuild" {
		return fmt.Errorf("usage: nbcatalog rebuild [-c config] [-C catalog]")
	}
	fs := flag.NewFlagSet("nbcatalog rebuild", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	parseArgs(fs, args[1:])

	cfg, err := loadConfigRO(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	n, err := eng.RebuildCatalog(logfStdout)
	if err != nil {
		return err
	}
	fmt.Printf("catalog cache rebuilt from media: %d slot(s) indexed\n", n)
	return nil
}

// CmdMedium implements `nb medium`: list configured media with capacity and the
// volume each currently holds, or detail one medium and the slots it stores.
func CmdMedium(args []string) error {
	fs := flag.NewFlagSet("nb medium", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	pos := parseArgs(fs, args)

	cfg, err := loadConfigRO(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	if len(pos) >= 1 {
		return mediumDetail(eng, pos[0])
	}
	return mediumList(eng)
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
	fmt.Printf("  used:    %s / %s\n\n", sizeutil.FormatBytes(m.Used), capacityStr(m.Capacity))
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

func capacityStr(c int64) string {
	if c <= 0 {
		return "unbounded"
	}
	return sizeutil.FormatBytes(c)
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

// CmdChanger implements `nb changer`: the manual changer — inventory the library
// or mount a volume. Reading the actual label requires loading a volume in the
// drive, so this is also how you point the drive at a specific reel before a write.
func CmdChanger(args []string) error {
	if len(args) > 0 && args[0] == "load" {
		return cmdChangerLoad(args[1:])
	}
	if len(args) > 0 && args[0] == "list" {
		return cmdChangerList(args[1:])
	}
	return fmt.Errorf("usage: nb changer list <medium> | nb changer load <medium> <bay> [--label]")
}

func cmdChangerList(args []string) error {
	fs := flag.NewFlagSet("nb changer list", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	pos := parseArgs(fs, args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: nb changer list <medium>")
	}
	cfg, err := loadConfigRO(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	loaded, bays, err := eng.Bays(pos[0])
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "\tBAY\tLABEL\tSTATUS\tUSED\tCAPACITY\tFILES")
	for _, b := range bays {
		mark := " "
		if b.Bay == loaded {
			mark = "*"
		}
		label, status := b.Label, "append"
		if b.Blank {
			label, status = "(blank)", "blank"
		} else if b.Capacity > 0 && b.Used >= b.Capacity {
			status = "full"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", mark, b.Bay, label, status,
			sizeutil.FormatBytes(b.Used), capacityStr(b.Capacity), b.Files)
	}
	tw.Flush()

	// A single-drive (manual) station also has reels in the room — offline, not in
	// any bay — that the operator can load. List them so their ids/labels are known.
	if shelf, err := eng.Shelf(pos[0]); err == nil && len(shelf) > 0 {
		fmt.Println("\nIn the room (load with `nb changer load`, or when prompted):")
		rw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(rw, "  REEL\tLABEL\tSTATUS\tUSED\tCAPACITY\tFILES")
		for _, b := range shelf {
			label, status := b.Label, "append"
			if b.Blank {
				label, status = "(blank)", "blank"
			} else if b.Capacity > 0 && b.Used >= b.Capacity {
				status = "full"
			}
			fmt.Fprintf(rw, "  %s\t%s\t%s\t%s\t%s\t%d\n", b.Bay, label, status,
				sizeutil.FormatBytes(b.Used), capacityStr(b.Capacity), b.Files)
		}
		rw.Flush()
	}
	return nil
}

func cmdChangerLoad(args []string) error {
	fs := flag.NewFlagSet("nb changer load", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	byLabel := fs.Bool("label", false, "treat the argument as a volume label rather than a bay id")
	pos := parseArgs(fs, args)
	if len(pos) < 2 {
		return fmt.Errorf("usage: nb changer load [--label] <medium> <bay-or-label>")
	}
	cfg, err := loadConfig(*cfgPath, *catalogFlag)
	if err != nil {
		return err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		return err
	}
	return eng.LoadVolume(pos[0], pos[1], *byLabel, logfStdout)
}

// CmdRestore implements `nbrestore`: rebuild a DLE (or all DLEs) from a slot.
func CmdRestore(args []string) error {
	fs := flag.NewFlagSet("nbrestore", flag.ExitOnError)
	cfgPath := fs.String("c", DefaultConfigPath, "path to config file")
	catalogFlag := fs.String("C", "", "catalog directory (overrides config)")
	dleName := fs.String("dle", "", "DLE name to restore (default: all DLEs in the slot)")
	dest := fs.String("dest", "", "destination directory (required)")
	pos := parseArgs(fs, args)

	if len(pos) < 1 {
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
	eng.SetOperator(stdinOperator{})
	slotID := pos[0]
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
