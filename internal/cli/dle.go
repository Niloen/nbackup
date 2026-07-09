package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/dletree"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// newDleCmd implements `nb dle`: inspect the catalog grouped by DLE (backup source)
// rather than by run. The same archives the run view groups by run, browsed instead
// by what was backed up — one row per DLE, then its archive timeline across runs.
func newDleCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "dle [dle]",
		Short:   "List DLEs (backup sources), or detail one",
		Long:    "Inspect the catalog grouped by DLE (a host:path backup source). With no argument it lists each DLE and its backup history; pass a DLE to show its archive timeline across runs. (Reclaim with `nb prune`, which is per-DLE on disk/cloud.)",
		Example: "  nb dle\n  nb dle localhost:/home",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runDleShow(a, args[0])
			}
			return runDleList(a)
		},
	}
}

func runDleList(a *app) error {
	cfg, err := a.loadOrDefaultCatalog()
	if err != nil {
		return err
	}
	eng, err := engine.New(cfg)
	if err != nil {
		return err
	}
	sums := eng.DLESummaries()
	if len(sums) == 0 {
		if len(cfg.Sources) == 0 {
			fmt.Println(noConfigHint("no DLEs in catalog", a.catalog))
		} else {
			fmt.Println("no DLEs in catalog")
		}
		return nil
	}

	// Rows arranged by path (dletree, the same tree `nb plan` and the report draw):
	// a partitioned source's DLEs render under one host:base header with short
	// relative labels and a size subtotal, so the list stays readable when one
	// source resolves into dozens of long-named children.
	tw := newTab(os.Stdout)
	fmt.Fprintln(tw, "DLE\tRUNS\tLAST FULL\tLAST\tSIZE\tCOPIES")
	row := func(label string, g catalog.DLESummary) {
		lastFull := g.LastFull
		if lastFull == "" {
			lastFull = "never"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\tL%d\t%s\t%s\n", label, g.Runs, lastFull, g.LastLevel,
			sizeutil.FormatBytes(g.Bytes), strings.Join(g.Media, ", "))
	}
	items := make([]dletree.Item, len(sums))
	for i, s := range sums {
		items[i], _ = dletree.Split(s.Display)
		items[i].Rest = s.Rest
	}
	for _, g := range dletree.Build(items) {
		if g.Children == nil {
			s := sums[g.Index]
			display := s.Display
			if s.Rest {
				// A remainder whose siblings left the catalog: position no longer
				// says what it is, so the suffix still has to.
				display += "  (the rest of a partition)"
			}
			row(display, s)
			continue
		}
		var bytes int64
		for _, c := range g.Children {
			bytes += sums[c.Index].Bytes
		}
		fmt.Fprintf(tw, "%s · %d DLEs · %s\t\t\t\t\t\n", g.ID(), len(g.Children), sizeutil.FormatBytes(bytes))
		for i, c := range g.Children {
			row("  "+dletree.Branch(i, len(g.Children))+" "+g.Label(c), sums[c.Index])
		}
	}
	tw.Flush()
	return nil
}

func runDleShow(a *app, arg string) error {
	cfg, err := a.loadOrDefaultCatalog()
	if err != nil {
		return err
	}
	eng, err := engine.New(cfg)
	if err != nil {
		return err
	}
	runs := eng.Catalog().Runs()
	slug, display, ok := resolveDLE(runs, arg)
	if !ok {
		return fmt.Errorf("no DLE %q in catalog (list them with `nb dle`)", arg)
	}
	for _, r := range eng.Catalog().LatestResolved() {
		if r.DLE == slug && r.Rest {
			display += "  (the rest of a partition)"
		}
	}
	fmt.Printf("DLE %s\n\n", display)
	tw := newTab(os.Stdout)
	fmt.Fprintln(tw, "RUN\tDATE\tLEVEL\tSIZE\tBASE\tCOPIES")
	for _, s := range runs {
		for _, ar := range s.Archives {
			if ar.DLE != slug {
				continue
			}
			base := ar.BaseRun
			if base == "" {
				base = "-"
			}
			// Name each copy by the volumes THIS archive occupies (PlacedArchive.Labels),
			// not the run copy's full label set — a spanned run's copy touches volumes
			// this DLE's archive never landed on.
			var media []string
			for _, p := range eng.Catalog().Placements(s.ID) {
				if pa, ok := p.Placed(slug, ar.Level); ok {
					if labels := pa.Labels(); len(labels) > 0 {
						media = append(media, p.Medium+":"+strings.Join(labels, "+"))
					} else {
						media = append(media, p.Medium)
					}
				}
			}
			sort.Strings(media)
			fmt.Fprintf(tw, "%s\t%s\tL%d\t%s\t%s\t%s\n", s.ID, s.Date(), ar.Level,
				sizeutil.FormatBytes(ar.Compressed), base, strings.Join(media, ", "))
		}
	}
	tw.Flush()
	return nil
}

// resolveDLE matches a user-typed DLE identifier against the catalog's archives,
// accepting either the internal slug or the host:path display id, and returns the
// slug plus a display string. Archives carry their own host/path, so the match needs
// no config — a DLE that was dumped but later removed from config still resolves.
func resolveDLE(runs []*catalog.Run, arg string) (slug, display string, ok bool) {
	for _, s := range runs {
		for _, ar := range s.Archives {
			if ar.DLE == arg || ar.DLEID() == arg {
				return ar.DLE, ar.DLEID(), true
			}
		}
	}
	return "", "", false
}
