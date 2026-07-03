package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
		Long:    "Write a volume's identity label, making it writable. Refuses to overwrite foreign data, and (with --relabel) a tape that still holds protected runs — those within minimum_age or holding a DLE's last recovery path, including a run spanned across tapes. --relabel reuses an NBackup-labeled volume and --force overrides safety refusals.\n\nOn a robotic library, a new label takes a blank bay; to recycle a specific tape to a new name, `nb load <bay>` it first, then `nb label --relabel <name>` — the relabel acts on the loaded bay. A single-drive station always labels whatever reel is in the drive.",
		Example: "  nb label tape DAILY-01\n  nb load lto bay-02\n  nb label --relabel lto DAILY-42   # recycle the loaded tape to a new name",
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
				fmt.Println(noConfigHint("no media"))
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
	printInventory(eng, name)
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

// volumeHasRuns reports whether the catalog records any committed run on the named
// label — false for a blank tape or one holding only orphan parts from an aborted
// span/write. Used to mark such a tape reclaimable in the inventory.
func volumeHasRuns(eng *engine.Engine, label string) bool {
	if label == "" {
		return false
	}
	return len(eng.Catalog().RunsOnLabel(label)) > 0
}

// classifyVolume renders a volume's display label and the status decidable from
// the volume alone — foreign, blank, wrong-pool, or full; "" means a labeled
// volume of this pool with room. medium is the medium being inspected, so a reel
// labeled for a different pool is flagged rather than shown as one of this
// medium's own volumes. Pure (no catalog, no medium policy): it serves both the
// inventory listing (which refines "" and "full" via volumeLabelStatus) and the
// operator swap prompt (reelDesc), so the two never diverge.
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
	case b.Capacity > 0 && b.Used >= b.Capacity:
		return b.Label, "full"
	default:
		return b.Label, ""
	}
}

// volumeLabelStatus is the inventory-listing refinement of classifyVolume: it adds
// the catalog-derived states (reclaimable orphans, one-run-per-volume "used") and
// resolves a writable volume to "append"/"writable" per the medium's appendability.
func volumeLabelStatus(b media.VolumeStatus, medium string, appendable, hasRuns bool) (label, status string) {
	label, status = classifyVolume(b, medium)
	switch status {
	case "", "full":
		// Refined below; even a full volume can instead be reclaimable.
	default:
		return label, status
	}
	if b.Files > 1 && !hasRuns {
		// The volume holds data past its label, but the catalog records no committed
		// run on it — orphan parts from an interrupted span/write. It is reusable as
		// is (no committed data to lose) via `nb label --relabel`, so present it as
		// reclaimable rather than "full"/"used", which imply it holds real backups.
		return label, "reclaimable"
	}
	if status == "full" {
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
				if cfg, err := a.loadForWrite(); err == nil {
					if d, ok := cfg.Media[args[0]]; ok && d.Type != "tape" {
						return fmt.Errorf("medium %q is addressed directly, not by loading volumes (`nb load` applies only to a tape library or single-drive station)", args[0])
					}
				}
			}
			return fmt.Errorf("load requires a medium and a bay/reel/label, e.g. `nb load lto bay-03`")
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
	cmd.Flags().BoolVar(&byLabel, "label", false, "treat the argument as a volume label rather than a bay/reel id")
	return cmd
}
