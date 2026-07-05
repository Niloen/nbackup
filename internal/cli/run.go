package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// newRunCmd implements `nb run`: list runs, or — with a run id — detail one.
// Inspection follows the bare-noun convention (like `nb medium`, `nb dle`): no
// arg lists, a positional arg details that one. (Reclaim runs with `nb prune`.)
func newRunCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "run [run-id]",
		Short:   "List runs, or detail one",
		Long:    "Inspect the run catalog. With no argument it lists runs; pass a run id to show that run's archives and copies. (Reclaim runs with `nb prune`.)",
		Example: "  nb run\n  nb run run-2026-06-21.020000",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runRunShow(a, args[0])
			}
			return runRunList(a)
		},
	}
}

func runRunList(a *app) error {
	cfg, err := a.loadOrDefaultCatalog()
	if err != nil {
		return err
	}
	eng, err := engine.New(cfg)
	if err != nil {
		return err
	}
	runs := eng.Catalog().Runs()
	if len(runs) == 0 {
		// A bare config (no sources) means no backup config was found — read-only
		// commands fall back to the default local catalog (see noConfigHint).
		if len(cfg.Sources) == 0 {
			fmt.Println(noConfigHint("no runs in catalog", a.catalog))
		} else {
			fmt.Println("no runs in catalog (if runs exist on media but not here, run `nb rebuild`)")
		}
		return nil
	}
	tw := newTab(os.Stdout)
	fmt.Fprintln(tw, "RUN\tSTATUS\tARCHIVES\tSIZE\tCOMMITTED\tCOPIES")
	for _, s := range runs {
		committed := "-"
		if t := s.LastArchiveAt(); !t.IsZero() {
			committed = sizeutil.FormatStamp(t)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", s.ID, runStatus(s), len(s.Archives),
			sizeutil.FormatBytes(s.TotalBytes()), committed, copiesSummary(eng.Catalog().Placements(s.ID)))
	}
	tw.Flush()
	return nil
}

// runStatus renders a run's status cell: every cataloged run is committed (the archive
// is the commit unit), with a partial marker when any archive omitted unreadable files.
func runStatus(s *catalog.Run) string {
	if s.Partial() {
		return "committed (partial)"
	}
	return "committed"
}

// copiesSummary renders a run's placements as a compact comma list, naming the
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

func runRunShow(a *app, runID string) error {
	cfg, err := a.loadOrDefaultCatalog()
	if err != nil {
		return err
	}
	eng, err := engine.New(cfg)
	if err != nil {
		return err
	}
	s, err := eng.Catalog().ReadRun(runID)
	if err != nil {
		// `nb run list` (and friends) parse the word as a run id; a non-run-id argument
		// that isn't found is almost always a user reaching for a subcommand that does not
		// exist — point them at the bare list rather than at `nb rebuild`.
		if !strings.HasPrefix(runID, "run-") {
			return fmt.Errorf("%w (to list all runs, run `nb run` with no argument)", err)
		}
		return err
	}
	fmt.Printf("Run %s  (%s)\n", s.ID, runStatus(s))
	fmt.Printf("  date:    %s\n", s.Date())
	fmt.Printf("  committed: %s\n", s.LastArchiveAt().Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("  total:   %s\n\n", sizeutil.FormatBytes(s.TotalBytes()))
	tw := newTab(os.Stdout)
	fmt.Fprintln(tw, "DLE\tLEVEL\tFILES\tSIZE\tCOMPRESS\tENCRYPT")
	for _, ar := range s.Archives {
		enc := ar.Encrypt
		if enc == "" {
			enc = "none"
		}
		// A PARTIAL archive committed a valid backup of what was readable but omitted
		// unreadable source files — flag it so the gap is visible after the fact.
		partial := ""
		if ar.Partial() {
			partial = fmt.Sprintf("\tPARTIAL (%d file(s) unreadable, omitted)", ar.Unreadable)
		}
		fmt.Fprintf(tw, "%s\tL%d\t%d\t%s\t%s\t%s%s\n", ar.DLEID(), ar.Level, ar.FileCount, sizeutil.FormatBytes(ar.Compressed), ar.Compress, enc, partial)
	}
	tw.Flush()

	placements := eng.Catalog().Placements(s.ID)
	fmt.Printf("\nCOPIES (%d)\n", len(placements))
	// One row per segment (each data part, then the commit footer) rather than one
	// row per copy: a copy spanned across many volumes would otherwise pack every
	// volume name into one overflowing cell, and listing the data parts but not the
	// commit volume read like an off-by-one. A row per segment names where each piece
	// landed — volume + file number — so a spanned archive is legible and the commit
	// (written last, possibly on a later volume) is shown explicitly.
	ptw := newTab(os.Stdout)
	fmt.Fprintln(ptw, "  MEDIUM\tDLE\tLEVEL\tSEGMENT\tVOLUME\tFILE")
	spanned := false
	for _, p := range placements {
		for _, ar := range p.Archives {
			dle := eng.DisplayDLE(ar.DLE)
			level := fmt.Sprintf("L%d", ar.Level)
			n := len(ar.Parts)
			for i, pt := range ar.Parts {
				seg := "data"
				if n > 1 {
					seg = fmt.Sprintf("part %d/%d", i+1, n)
					spanned = true
				}
				fmt.Fprintf(ptw, "  %s\t%s\t%s\t%s\t%s\t%d\n", p.Medium, dle, level, seg, volumeOrDash(pt.Label), pt.Pos)
			}
			fmt.Fprintf(ptw, "  %s\t%s\t%s\t%s\t%s\t%d\n", p.Medium, dle, level, "commit", volumeOrDash(ar.Commit.Label), ar.Commit.Pos)
		}
	}
	ptw.Flush()
	fmt.Println("  FILE is the segment's sequential file number on VOLUME (VOLUME \"-\" = a label-less medium, e.g. disk/s3).")
	if spanned {
		fmt.Println("  A spanned archive lists each data part on its volume; the commit footer is written last and may land on a later volume.")
	}
	return nil
}

// volumeOrDash renders a volume label, or "-" for a label-less medium (disk/s3),
// where files are addressed within the single medium rather than by volume label.
func volumeOrDash(label string) string {
	if label == "" {
		return "-"
	}
	return label
}
