package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newRebuildCmd implements `nb rebuild`: rebuild the local run-index cache by
// rescanning the self-describing media. Additive by default — each pass merges
// what is readable now into the cache — so a lost catalog is recovered by feeding
// tapes one at a time and re-running until the printed worklist is empty; --full
// wipes first and keeps only what this one pass can reach.
func newRebuildCmd(a *app) *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:     "rebuild",
		Short:   "Rebuild the catalog cache by rescanning media",
		Long:    "Rescan every configured medium and merge the result into the local catalog cache (run index and volume registry), from the commit footers and labels found there. Additive: volumes not reachable right now keep their records, so after losing the catalog you can insert tapes one at a time and re-run until the printed worklist of missing tapes is empty. --full wipes the cache first and keeps only what this pass reaches.",
		Example: "  nb rebuild          # e.g. after losing the workdir: repeat per inserted tape\n  nb rebuild --full   # start over from exactly the volumes reachable now",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadOrDefaultCatalog()
			if err != nil {
				return err
			}
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			rep, err := eng.RebuildCatalog(full, a.logf())
			if err != nil {
				return err
			}
			fmt.Printf("catalog cache rebuilt from media: %d run(s) indexed\n", rep.Runs)
			// The worklist: tapes the footers name that no scan has seen, and runs
			// whose footer tape was not among the volumes fed in yet.
			if missing := eng.Catalog().MissingVolumes(); len(missing) > 0 {
				fmt.Printf("missing %d tape(s) referenced by the catalog — insert and re-run `nb rebuild`:\n", len(missing))
				for _, m := range missing {
					fmt.Printf("  %-20s parts of %s\n", m.Label, summarizeRuns(m.Runs))
				}
			}
			if len(rep.OrphanRuns) > 0 {
				fmt.Printf("%d run(s) have parts on the scanned tapes but no commit footer was found:\n", len(rep.OrphanRuns))
				for _, o := range rep.OrphanRuns {
					fmt.Printf("  %-24s parts on %s\n", o.Run, strings.Join(o.Labels, ", "))
				}
				fmt.Println("  if more tapes of the pool exist, insert them and re-run `nb rebuild`; otherwise these are a crashed run's leftovers (reclaimed at relabel)")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "wipe the cache first; keep only what this pass reaches")
	return cmd
}

// summarizeRuns renders a run list compactly: all of a few, else first + count.
func summarizeRuns(runs []string) string {
	if len(runs) <= 2 {
		return strings.Join(runs, ", ")
	}
	return fmt.Sprintf("%s and %d more run(s)", runs[0], len(runs)-1)
}
