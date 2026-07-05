package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/mount"
	"github.com/Niloen/nbackup/internal/recovery"
)

// newMountCmd implements `nb mount`: serve the catalog as a read-only FUSE
// filesystem — top-level directories are runs, and each run holds every DLE's
// snapshot as of that run.
func newMountCmd(a *app) *cobra.Command {
	var cacheDir string
	cmd := &cobra.Command{
		Use:   "mount <dir>",
		Short: "Mount the backups as a read-only filesystem of run snapshots",
		Long: "Mount the catalog as a read-only FUSE filesystem. The top level lists runs; inside" +
			" each run is every DLE's snapshot as of that run — its most recent full plus the" +
			" incrementals up to it, the same view `nb recover` browses.\n\n" +
			"Browsing reads only the member indexes. A file's content is recovered from the" +
			" archives on its first open and cached for the mount's lifetime, so an unopened" +
			" file lists with size 0 until read (`cat`/`cp` see the full content; whole-tree" +
			" copiers that trust listed sizes should use `nb recover --all` instead). Like" +
			" file-level recovery the view is a union: a file deleted before the run may still" +
			" appear. Reads from cloud/cold media happen without a cost prompt.\n\n" +
			"The mount holds the config lock (like an interactive recover session) until" +
			" unmounted — stop it with Ctrl-C or `fusermount -u <dir>`.",
		Example: "  nb mount /mnt/backups\n" +
			"  ls /mnt/backups\n" +
			"  cat '/mnt/backups/<run>/<dle>/etc/hosts'",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadOrDefaultCatalog()
			if err != nil {
				return err
			}
			// The mount reads media (index misses while browsing, extraction on
			// every first open), so it takes the config lock for its lifetime,
			// like an interactive recover session.
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			attachOperator(eng)

			cache := cacheDir
			if cache == "" {
				tmp, err := os.MkdirTemp("", "nb-mount-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(tmp)
				cache = tmp
			}

			logf := a.logf()
			srv, err := mount.Serve(mount.Options{
				Mountpoint: args[0],
				CacheDir:   cache,
				Logf:       logf,
			}, mount.Backend{
				Runs: eng.Catalog().Runs,
				Tree: eng.OpenRecoverRun,
				Extract: func(steps []recovery.ExtractStep, destDir string) error {
					_, _, err := eng.ExtractSelection(steps, destDir, logf, nil)
					return err
				},
			})
			if err != nil {
				return fmt.Errorf("mount %s: %w", args[0], err)
			}
			fmt.Printf("mounted at %s — unmount with Ctrl-C or `fusermount -u %s`\n", args[0], args[0])
			// Execute's signal handling cancels the command context on the
			// first Ctrl-C; unmount then, which unblocks Wait.
			go func() {
				<-cmd.Context().Done()
				_ = srv.Unmount()
			}()
			srv.Wait()
			return nil
		},
	}
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "directory for the recovered-file cache (default: a temp dir, removed on unmount)")
	return cmd
}
