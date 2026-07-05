package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/progress"
)

// newStatusCmd implements `nb status`: show the progress of the current (or most
// recent) run by reading the run-status file `nb dump` writes. It needs no
// engine, only the catalog workdir, so it is cheap to poll.
func newStatusCmd(a *app) *cobra.Command {
	var watch time.Duration
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "Show the progress of the current or most recent run",
		Long:    "Read the run-status file `nb dump` maintains and render a progress report: each DLE's state and percent of estimate, totals, throughput, and ETA. With --watch it refreshes on an interval. Reflects an in-flight run, or the last finished one.",
		Example: "  nb status\n  nb status --watch 2s",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// status only ever reflects the current or most recent run (the live
			// status file), not an arbitrary one — point a run id at the command that
			// details any run instead of erroring as an unrecognized subcommand.
			if len(args) == 1 {
				return fmt.Errorf("status only shows the current or most recent run; use `nb run %s` for a specific run's detail", args[0])
			}
			// Like plan/dump/check, a missing config is a hard error: a synthesized
			// default catalog would report "no run in progress" (exit 0) from a
			// directory nothing ever dumps to — reading as "backups idle" to a
			// monitor. --catalog still points status at an existing catalog directly.
			cfg, err := a.loadRequired()
			if err != nil {
				return err
			}
			dir := cfg.WorkdirPath()
			if watch <= 0 {
				return renderStatus(dir)
			}
			for {
				fmt.Print("\033[H\033[2J") // home cursor + clear screen
				if err := renderStatus(dir); err != nil {
					return err
				}
				snap, err := progress.Load(dir)
				if err == nil && snap.Phase.Terminal() {
					return nil // run finished; stop watching
				}
				// Watching is a read-only poll, so Ctrl-C just quits the viewer: exit cleanly
				// (nil, no "canceled" notice — there's nothing in flight to cancel) rather than
				// sleeping out the interval first.
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(watch):
				}
			}
		},
	}
	cmd.Flags().DurationVar(&watch, "watch", 0, "refresh every interval (e.g. 2s) until the run finishes")
	return cmd
}

// renderStatus loads and prints one run-status snapshot, or a friendly note when
// no run has written one yet.
func renderStatus(dir string) error {
	snap, err := progress.Load(dir)
	if progress.IsNotExist(err) {
		fmt.Println("no run in progress (no status recorded yet)")
		return nil
	}
	if err != nil {
		return err
	}
	progress.Render(os.Stdout, snap, time.Now())
	return nil
}
