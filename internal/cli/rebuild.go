package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newRebuildCmd implements `nb rebuild`: rebuild the local run-index cache by
// rescanning the self-describing media.
func newRebuildCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "rebuild",
		Short:   "Rebuild the catalog cache by rescanning media",
		Long:    "Rescan every configured medium and rebuild the local catalog cache (run index and volume registry) from the commit footers and labels found there.",
		Example: "  nb rebuild   # e.g. after losing the workdir on a new server",
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
			n, err := eng.RebuildCatalog(a.logf())
			if err != nil {
				return err
			}
			fmt.Printf("catalog cache rebuilt from media: %d run(s) indexed\n", n)
			return nil
		},
	}
}
