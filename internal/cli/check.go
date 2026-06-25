package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/engine"
)

// newCheckCmd implements `nb check`: NBackup's amcheck. It verifies the server side and
// every source host — by default it SSHes to each remote client to probe GNU tar, the
// source paths, any client-side tools, and the state_dir; --offline resolves and reports
// without connecting. Exits non-zero if any hard check fails, so cron can gate on it.
func newCheckCmd(a *app) *cobra.Command {
	var offline bool
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Verify the config and reach every source host (amcheck)",
		Long: "Check that a run would succeed: the server (landing medium, workdir, codec, " +
			"encryption) and each source host. A localhost DLE is checked locally; any other host " +
			"is remote and, unless --offline, probed over SSH (reachable, GNU tar, source readable, " +
			"client-side tools, state_dir). Exits non-zero if any check fails. Writes nothing.",
		Example: "  nb check\n  nb check --offline",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// check validates a real config and reaches its hosts; without a config
			// file there is nothing to check, so require one (like plan/dump) rather
			// than synthesizing a default landing and reporting it "ready".
			cfg, err := a.load()
			if err != nil {
				return err
			}
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
			rep := eng.Check(!offline)
			printCheckReport(rep)
			if rep.Failures > 0 {
				return fmt.Errorf("%d check(s) failed", rep.Failures)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "do not connect to remote hosts; only resolve and report what would be checked")
	return cmd
}

func printCheckReport(rep *engine.CheckReport) {
	fmt.Println("Server:")
	for _, l := range rep.Server {
		fmt.Printf("  %s %s\n", checkMark(l), l.Msg)
	}
	for _, h := range rep.Hosts {
		where := "local"
		if h.Remote {
			where = "remote, " + h.Target
		}
		fmt.Printf("\n%s  (%s):\n", h.Host, where)
		for _, l := range h.Lines {
			fmt.Printf("  %s %s\n", checkMark(l), l.Msg)
		}
	}
	fmt.Println()
	if rep.Failures == 0 {
		fmt.Println("all checks passed")
	} else {
		fmt.Printf("%d check(s) failed\n", rep.Failures)
	}
}

func checkMark(l engine.CheckLine) string {
	switch {
	case l.OK:
		return "✓"
	case l.Warn:
		return "!"
	default:
		return "✗"
	}
}
