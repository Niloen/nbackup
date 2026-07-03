package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/report"
)

// newVerifyCmd implements `nb verify`: check archive checksums of named runs, or
// every run with --all. Verifying all runs can mount every volume in the pool
// (each tape in turn), so the whole-pool scan is gated behind an explicit flag
// rather than triggered by a bare `nb verify`.
func newVerifyCmd(a *app) *cobra.Command {
	var all, deep bool
	cmd := &cobra.Command{
		Use:   "verify [run-id...]",
		Short: "Verify run integrity (checksum, or --deep structural)",
		Long: "Verify archives against their commit footers. By default it re-checks payload checksums " +
			"(integrity). With --deep it also streams each archive through the real read " +
			"pipeline — decrypt, decompress, then `tar -t` (list, not extract) — and asserts the " +
			"members match the recorded index, proving the bytes are a valid restorable stream and " +
			"exercising the key and compression end-to-end. It writes nothing either way. Pass run ids " +
			"to verify just those; with no ids it verifies every run (which may mount every volume in the pool).",
		Example: "  nb verify run-2026-06-21.020000\n  nb verify --deep run-2026-06-21.020000\n  nb verify",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return fmt.Errorf("--all cannot be combined with explicit run ids")
			}
			// Bare `nb verify` (no run ids) verifies the whole catalog — the obvious
			// reading of "verify my backups". --all stays as an explicit synonym.
			if len(args) == 0 {
				all = true
			}
			// verify is an assertion (monitors gate on its exit code), so a missing
			// config is an error — not a green "0 run(s) verified".
			cfg, err := a.loadRequired()
			if err != nil {
				return err
			}
			// Verify mounts media, so it takes the config lock like any medium-accessing
			// command — a verify mid-dump would fight the run for drives and the robot.
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			// Verifying reads media, so a spanned run on a single-drive station needs
			// reel swaps — give it the operator so it prompts (and reassembles a spanned
			// run) just like restore, rather than failing at the first volume boundary.
			attachOperator(eng)
			if all && !a.quiet {
				mode := "checksum"
				if deep {
					mode = "deep (checksum + structural)"
				}
				fmt.Printf("verifying %d run(s) in the catalog [%s]\n", len(eng.Catalog().Runs()), mode)
			}
			checks := engine.CheckChecksum
			if deep {
				checks |= engine.CheckStructural
			}
			return a.runReported(cfg, report.Run{Command: report.CommandVerify, ExitClass: "verify-failures"}, func() (report.Run, error) {
				vr, err := eng.Verify(args, engine.VerifyOptions{Checks: checks}, a.logf())
				if err != nil {
					return report.Run{}, err
				}
				rec := report.Run{Command: report.CommandVerify, Failures: vr.Failures}
				if vr.Failures > 0 {
					return rec, fmt.Errorf("%d run(s) failed verification", vr.Failures)
				}
				return rec, nil
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "verify every run in the catalog (the default when no run ids are given)")
	cmd.Flags().BoolVar(&deep, "deep", false, "also validate structure: decrypt+decompress+tar-list, members vs the recorded index")
	return cmd
}
