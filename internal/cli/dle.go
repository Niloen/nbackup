package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
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
			fmt.Println(noConfigHint("no DLEs in catalog"))
		} else {
			fmt.Println("no DLEs in catalog")
		}
		return nil
	}

	tw := newTab(os.Stdout)
	fmt.Fprintln(tw, "DLE\tRUNS\tLAST FULL\tLAST\tSIZE\tCOPIES")
	for _, g := range sums {
		lastFull := g.LastFull
		if lastFull == "" {
			lastFull = "never"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\tL%d\t%s\t%s\n", g.Display, g.Runs, lastFull, g.LastLevel,
			sizeutil.FormatBytes(g.Bytes), strings.Join(g.Media, ", "))
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
			var media []string
			for _, p := range eng.Catalog().Placements(s.ID) {
				for _, pa := range p.Archives {
					if pa.DLE == slug {
						media = append(media, p.Medium)
						break
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
