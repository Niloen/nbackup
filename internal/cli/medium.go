package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// newLabelCmd implements `nb label`: write (or rewrite) a volume's identity
// label. This is the deliberate act that makes a tape writable; it guards
// against overwriting foreign data or a tape that still holds protected runs.
func newLabelCmd(a *app) *cobra.Command {
	var relabel, force bool
	cmd := &cobra.Command{
		Use:     "label <medium> <name>",
		Short:   "Label a volume (required for tape before first dump)",
		Long:    "Write a volume's identity label, making it writable. Refuses to overwrite foreign data, and (with --relabel) a tape that still holds protected runs — those within minimum_age or holding a DLE's last recovery path, including a run spanned across tapes. --relabel reuses an NBackup-labeled volume and --force overrides safety refusals.\n\nOn a robotic library, a new label takes a blank tape; to recycle a specific tape to a new name, `nb load <slot>` it first, then `nb label --relabel <name>` — the relabel acts on the loaded tape. A single-drive station always labels whatever reel is in the drive.",
		Example: "  nb label tape DAILY-01\n  nb load lto 2\n  nb label --relabel lto DAILY-42   # recycle the loaded tape to a new name",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// An empty name would otherwise dead-end deep in the label protocol with a
			// misleading "no blank slot available"; reject it up front for what it is.
			if strings.TrimSpace(args[1]) == "" {
				return fmt.Errorf("label name required, e.g. `nb label %s DAILY-01`", args[0])
			}
			cfg, err := a.loadForWrite()
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
			cfg, err := a.loadOrDefaultCatalog()
			if err != nil {
				return err
			}
			// A bare config (no sources) means no config file was found — read-only
			// commands fall back to a synthesized default catalog, so don't present its
			// phantom default disk medium as if storage were configured (matching `nb run`).
			if len(cfg.Sources) == 0 {
				fmt.Println(noConfigHint("no media", a.catalog))
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
			eng, err := engine.New(cfg)
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
	tw := newTab(os.Stdout)
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
		names := make([]string, 0)
		for _, mi := range eng.Media() {
			names = append(names, mi.Name)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown medium %q (configured: %s)", name, strings.Join(names, ", "))
	}
	fmt.Printf("Medium %s  (%s)\n", m.Name, m.Type)
	fmt.Printf("  volume:  %s\n", volumeStr(m))
	fmt.Printf("  used:    %s / %s%s\n", sizeutil.FormatBytes(m.Used), capacityStr(m.Capacity), overMarker(m.Used, m.Capacity))
	fmt.Printf("  retention: minimum_age %s\n", sizeutil.FormatDuration(eng.MediumMinAge(name)))
	if st, ok := eng.MediumStats(name); ok {
		printMediumStats(st)
	}
	printInventory(eng, name)
	printVolumes(eng, name)
	fmt.Println()
	runs := eng.Catalog().RunsOn(name)
	if len(runs) == 0 {
		fmt.Println("no runs on this medium")
		return nil
	}
	tw := newTab(os.Stdout)
	fmt.Fprintln(tw, "RUN\tSIZE\tARCHIVES\tCOMMITTED")
	for _, s := range runs {
		committed := "-"
		if t := s.LastArchiveAt(); !t.IsZero() {
			committed = sizeutil.FormatStamp(t)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.ID, sizeutil.FormatBytes(s.TotalBytes()), len(s.Archives), committed)
	}
	tw.Flush()
	return nil
}

// printMediumStats prints a medium's usage statistics beneath its `nb medium <name>`
// header: the full/incremental split of the currently-retained archives, then — from
// the catalog's recorded usage ledger, which captures prune/relabel declines the
// retained picture cannot — the recorded span and an average-growth projection of
// when a bounded medium fills. The full used-over-time curve behind the growth figure
// is on the webui medium page.
func printMediumStats(st engine.MediumStats) {
	if st.Archives == 0 {
		return
	}
	fmt.Printf("  archives: %d  (full %s / incr %s)\n", st.Archives,
		sizeutil.FormatBytes(st.FullBytes), sizeutil.FormatBytes(st.IncrBytes))
	g := st.Growth
	if g.Samples >= 2 && g.Last.After(g.First) {
		fmt.Printf("  history: %d samples, %s → %s\n", g.Samples,
			sizeutil.FormatStamp(g.First), sizeutil.FormatStamp(g.Last))
	}
	if g.PerDay > 0 {
		line := fmt.Sprintf("  growth:  ~%s/day", sizeutil.FormatBytes(g.PerDay))
		if !g.ProjFull.IsZero() {
			line += fmt.Sprintf("  (reaches capacity ~%s)", g.ProjFull.Format("2006-01-02"))
		}
		fmt.Println(line)
	}
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

	dw := newTab(os.Stdout)
	// Show the device node per drive so the drive-index↔node mapping is inspectable:
	// on a robotic library the `device:` list must name the nodes in the library's
	// drive order, and this is where an operator verifies it. Omitted for a file-backed
	// library (no OS node).
	hasNode := false
	for _, d := range view.Drives {
		if d.Node != "" {
			hasNode = true
		}
	}
	if hasNode {
		fmt.Fprintln(dw, "\n\tDRIVE\tNODE\tBARCODE\tLABEL\tSTATUS\tON VOLUME\tFILES")
	} else {
		fmt.Fprintln(dw, "\n\tDRIVE\tBARCODE\tLABEL\tSTATUS\tON VOLUME\tFILES")
	}
	for _, d := range view.Drives {
		node := ""
		if hasNode {
			node = "\t" + barcodeOr(d.Node)
		}
		if !d.Loaded {
			fmt.Fprintf(dw, "\t%d%s\t(empty)\t-\t-\t-\t-\n", d.Drive, node)
			continue
		}
		used := volumeUsed(eng, d.Volume.Label)
		label, status := volumeLabelStatus(d.Volume, name, appendable, volumeHasRuns(eng, d.Volume.Label), used)
		fmt.Fprintf(dw, "\t%d%s\t%s\t%s\t%s\t%s\t%d\n", d.Drive, node, barcodeOr(d.Volume.Barcode), label, status,
			sizeutil.FormatBytes(used), d.Volume.Files)
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
		sw := newTab(os.Stdout)
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

// printVolumes lists the catalog's volume registry for a labeled medium's pool —
// every label ever recorded, with its epoch, when it was (re)labeled, its stored
// fill, and the learned barcode — the "tape catalog" an operator consults to see
// what exists, on-site or off. Labels the placements reference that no scan has
// ever seen (offsite tapes known only from commit footers' part maps) are listed
// beneath, so the registry view and the rebuild worklist tell one story.
// Address-identified media have no labels and print nothing.
func printVolumes(eng *engine.Engine, name string) {
	var vols []catalog.VolumeRecord
	known := map[string]bool{}
	for _, v := range eng.Catalog().Volumes() {
		if v.Label.Pool == name {
			vols = append(vols, v)
			known[v.Label.Name] = true
		}
	}
	if len(vols) == 0 {
		return
	}
	fmt.Println("\nVolumes:")
	tw := newTab(os.Stdout)
	fmt.Fprintln(tw, "\tLABEL\tEPOCH\tLABELED\tUSED\tBARCODE")
	for _, v := range vols {
		fmt.Fprintf(tw, "\t%s\t%d\t%s\t%s\t%s\n", v.Label.Name, v.Label.Epoch,
			sizeutil.FormatStamp(v.Label.WrittenAt), sizeutil.FormatBytes(v.Used), barcodeOr(v.Barcode))
	}
	tw.Flush()
	// Labels this medium's placements reference that the registry has never seen.
	missing := map[string]bool{}
	for _, s := range eng.Catalog().RunsOn(name) {
		for _, p := range eng.Catalog().Placements(s.ID) {
			if p.Medium != name {
				continue
			}
			for _, label := range p.Labels() {
				if label != "" && !known[label] {
					missing[label] = true
				}
			}
		}
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for l := range missing {
			names = append(names, l)
		}
		sort.Strings(names)
		fmt.Printf("  referenced but never scanned (offsite?): %s — insert and run `nb rebuild`\n", strings.Join(names, ", "))
	}
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

// volumeUsed is a labeled volume's stored fill (VolumeRecord.Used, maintained by
// the catalog at record time) — the ON VOLUME column and the "full" refinement
// read it, a tape being unable to report its own fill. 0 for a blank or
// unlabeled reel.
func volumeUsed(eng *engine.Engine, label string) int64 {
	if label == "" {
		return 0
	}
	v, _ := eng.Catalog().Volume(label)
	return v.Used
}

// classifyVolume renders a volume's display label and the status decidable from
// the volume alone — foreign, blank, or wrong-pool; "" means a labeled volume of
// this pool. medium is the medium being inspected, so a reel labeled for a
// different pool is flagged rather than shown as one of this medium's own
// volumes. Pure (no catalog, no medium policy): it serves both the inventory
// listing (which refines "" — including fullness, a catalog-ledger fact a tape
// cannot report about itself — via volumeLabelStatus) and the operator swap
// prompt (reelDesc), so the two never diverge.
func classifyVolume(b media.VolumeStatus, medium string) (label, status string) {
	switch {
	case b.Foreign:
		return "(foreign)", "foreign"
	case b.Blank:
		return "(blank)", "blank"
	case b.Pool != "" && b.Pool != medium:
		// A valid NBackup label, but for another pool: the write guard would refuse it
		// (wrong tape), so the inventory must not present it as this medium's own.
		return b.Label, fmt.Sprintf("wrong-pool:%s", b.Pool)
	default:
		return b.Label, ""
	}
}

// volumeLabelStatus is the inventory-listing refinement of classifyVolume: it adds
// the catalog-derived states (reclaimable orphans, one-run-per-volume "used") and
// resolves a writable volume to "append"/"writable" per the medium's appendability.
func volumeLabelStatus(b media.VolumeStatus, medium string, appendable, hasRuns bool, used int64) (label, status string) {
	label, status = classifyVolume(b, medium)
	if status != "" {
		return label, status
	}
	if b.Files > 1 && !hasRuns {
		// The volume holds data past its label, but the catalog records no committed
		// run on it — orphan parts from an interrupted span/write. It is reusable as
		// is (no committed data to lose) via `nb label --relabel`, so present it as
		// reclaimable rather than "full"/"used", which imply it holds real backups.
		return label, "reclaimable"
	}
	if b.Capacity > 0 && used >= b.Capacity {
		// Fullness is ledger arithmetic (a tape cannot report its own fill): the
		// catalog's recorded bytes against the declared volume_size.
		return label, "full"
	}
	if !appendable {
		// One run per volume: a reel that already holds a run cannot be appended, so
		// "append" would misrepresent it. b.Files counts the file-0 label plus any run
		// files, so >1 means it holds a run.
		if b.Files > 1 {
			return label, "used"
		}
		return label, "writable"
	}
	return label, "append"
}

func volumeStr(m engine.MediumInfo) string {
	switch {
	case m.Volumes > 1:
		return fmt.Sprintf("%d volume(s)", m.Volumes)
	case m.Volume == "":
		return "-"
	case m.Epoch > 0:
		return fmt.Sprintf("%s (epoch %d)", m.Volume, m.Epoch)
	}
	return m.Volume
}

// newLoadCmd implements `nb load`: mount a volume into a changer medium's drive —
// a robotic library slot, or a reel from a single-drive station's shelf — so the
// next read or write acts on it. The physical sibling of `nb label`; what's in the
// drive is shown by `nb medium <name>`.
func newLoadCmd(a *app) *cobra.Command {
	var byLabel bool
	cmd := &cobra.Command{
		Use:     "load <medium> <slot-or-label>",
		Short:   "Load a volume into a medium's drive",
		Long:    "Load a volume into the medium's drive by its slot number (the SLOT column of `nb medium <name>`), or with --label by its volume label. Applies to changers — a robotic library, or the file-backed manual station whose slots simulate the operator's shelf; a real single drive has no slots (insert the tape by hand).",
		Example: "  nb load lto 3\n  nb load --label lto DAILY-01",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				return nil
			}
			// The natural mistake `nb load <medium>` (one arg) should explain that an
			// address-identified medium has nothing to load, rather than fall back to
			// cobra's bare "accepts 2 arg(s)".
			if len(args) == 1 {
				if cfg, err := a.loadForWrite(); err == nil {
					if d, ok := cfg.Media[args[0]]; ok && d.Type != "tape" {
						return fmt.Errorf("medium %q is addressed directly, not by loading volumes (`nb load` applies only to a tape library or single-drive station)", args[0])
					}
				}
			}
			return fmt.Errorf("load requires a medium and a slot number or --label, e.g. `nb load lto 3`")
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadForWrite()
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
	cmd.Flags().BoolVar(&byLabel, "label", false, "treat the argument as a volume label rather than a slot number")
	return cmd
}
