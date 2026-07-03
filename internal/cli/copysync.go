package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/report"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// newCopyCmd implements `nb copy`: stream a run from the landing medium to
// another configured medium (e.g. disk -> tape).
func newCopyCmd(a *app) *cobra.Command {
	var from, to string
	var dryRun, force bool
	cmd := &cobra.Command{
		Use:     "copy <run-id>",
		Short:   "Copy a run from one medium to another (e.g. disk -> tape)",
		Long:    "Stream a run from one configured medium to another. The destination is selected with --to; the source defaults to the landing medium and is overridden with --from (e.g. un-vault tape -> disk). Copies by default (like `nb sync`/`nb prune`); pass --dry-run (-n) to preview.",
		Example: "  nb copy --to tape run-2026-06-21.020000\n  nb copy --to tape --dry-run run-2026-06-21.020000\n  nb copy --from tape --to disk run-2026-06-21.020000",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadForWrite()
			if err != nil {
				return err
			}
			runID := args[0]
			if dryRun {
				return runCopyDryRun(cfg, runID, from, to, force)
			}
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			attachOperator(eng)
			// Validate/resolve up front (before the run-reporting path): an unknown run
			// or medium is an argument error, not a failed run — it must not land in the
			// run log or fire notify.on_failure (matching `nb prune`).
			plan, err := eng.PlanCopy(runID, from, to, force)
			if err != nil {
				return err
			}
			// A run already on the target is an idempotent no-op (exit 0), matching
			// `nb sync`'s "up to date" — re-running a copy in a script must not fail,
			// and a no-op is not a run worth recording.
			if plan.AlreadyOnTarget {
				where := ""
				if len(plan.TargetLabels) > 0 {
					where = fmt.Sprintf(" (volume(s) %v)", plan.TargetLabels)
				}
				fmt.Printf("run %s is already on medium %q%s; nothing to copy (use --force to copy again)\n", runID, to, where)
				return nil
			}
			// Record + notify like `nb sync`: a failing cron copy must reach the run
			// log and notify.on_failure, not just an exit code nobody reads.
			return a.runReported(cfg, report.Run{Command: report.CommandCopy, ExitClass: "copy-error"}, func() (report.Run, error) {
				if err := eng.CopyRun(runID, from, to, force, a.logf()); err != nil {
					return report.Run{}, err
				}
				fmt.Println("copy complete")
				// Mirror `nb sync`'s over-capacity warning so the single-run sibling does
				// not silently push a target past its budget.
				if over, used, capacity, cerr := eng.MediumOverCapacity(to); cerr == nil && over {
					warnMediumOverCapacity(to, used, capacity, false)
				}
				return report.Run{Command: report.CommandCopy, RunID: runID, RunsCopied: 1, BytesMoved: plan.Bytes}, nil
			})
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "destination medium name (required)")
	cmd.Flags().StringVar(&from, "from", "", "source medium name (default: the landing medium)")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview without copying")
	cmd.Flags().BoolVar(&force, "force", false, "re-copy even if the run is already recorded on the target medium")
	cmd.MarkFlagRequired("to")
	return cmd
}

// runCopyDryRun previews `nb copy` without writing, rendering the engine's CopyPlan
// (the same resolve/validate/already-present rules CopyRun applies) — matching the
// dry-run shape of sync/prune.
func runCopyDryRun(cfg *config.Config, runID, from, to string, force bool) error {
	eng, err := engine.New(cfg)
	if err != nil {
		return err
	}
	plan, err := eng.PlanCopy(runID, from, to, force)
	if err != nil {
		return err
	}
	if plan.AlreadyOnTarget {
		fmt.Printf("%s -> %s: %s already on target; nothing to copy (use --force to re-copy)\n", plan.From, plan.To, runID)
		return nil
	}
	fmt.Printf("%s -> %s: would copy %s (%d archive(s), %s). Re-run without --dry-run to copy.\n",
		plan.From, plan.To, runID, plan.Archives, sizeutil.FormatBytes(plan.Bytes))
	if over, projected, capacity, perr := eng.ProjectedOverCapacity(plan.To, plan.Bytes); perr == nil && over {
		warnMediumOverCapacity(plan.To, projected, capacity, true)
	}
	return nil
}

// warnMediumOverCapacity prints the shared copy/sync over-capacity WARNING with its
// prune/grow remedy. projected selects the preview phrasing ("would hold", for a
// dry-run or a projection) over the post-copy one ("now holds").
func warnMediumOverCapacity(medium string, used, capacity int64, projected bool) {
	verb := "now holds"
	if projected {
		verb = "would hold"
	}
	fmt.Printf("WARNING: %q %s %s, over its %s capacity — run `nb prune %s` to reclaim, or raise its capacity\n",
		medium, verb, sizeutil.FormatBytes(used), sizeutil.FormatBytes(capacity), medium)
}

// newSyncCmd implements `nb sync`: the batch form of `nb copy`. It mirrors every
// landing run a target is missing onto that target, oldest
// first. With --to it syncs one ad-hoc target; without --to it runs the rules in
// the config's `sync:` block. Copies by default (like `nb prune`).
func newSyncCmd(a *app) *cobra.Command {
	var from, to, sinceStr string
	var last int
	var dryRun, force bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Mirror one medium's runs onto another (e.g. disk -> tape/s3)",
		Long: "Copy every run the target medium is missing from a source medium, oldest " +
			"first. The batch, idempotent form of `nb copy`: an interrupted or repeated sync " +
			"resumes, copying only what is not yet on the target. The source defaults to the " +
			"landing medium and is overridden with --from. With --to it syncs one target; " +
			"without --to it runs the `sync:` rules from the config. Copies by default; pass " +
			"--dry-run (-n) to preview.",
		Example: "  nb sync\n  nb sync --to lto\n  nb sync --to glacier --last 4\n  nb sync --from lto --to disk --dry-run",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadForWrite()
			if err != nil {
				return err
			}
			since, err := ParseDate(sinceStr)
			if err != nil {
				return fmt.Errorf("invalid --since date %q: --since must be in YYYY-MM-DD format", sinceStr)
			}
			if sinceStr == "" {
				since = time.Time{} // ParseDate defaults to today; sync wants "no bound"
			}
			if last < 0 {
				return fmt.Errorf("--last must be 0 (all) or a positive count, got %d", last)
			}

			// Resolve the targets: an explicit --to (ad-hoc), or every configured rule.
			type target struct {
				from, name string
				sel        engine.SyncSelection
			}
			var targets []target
			if to != "" {
				targets = append(targets, target{from, to, engine.SyncSelection{Last: last, Since: since}})
			} else {
				for _, r := range cfg.Sync {
					targets = append(targets, target{r.From, r.To, engine.SyncSelection{Last: r.Last}})
				}
				if len(targets) == 0 {
					return fmt.Errorf("no sync target: pass --to <medium> or add a `sync:` block to the config")
				}
			}

			// Dry-run only reads; a real run writes media + catalog, so lock it.
			eng, release, err := a.engineFor(cfg, !dryRun)
			if err != nil {
				return err
			}
			defer release()
			if !dryRun {
				attachOperator(eng)
			}

			runSync := func() (report.Run, error) {
				rec := report.Run{Command: report.CommandSync}
				for _, t := range targets {
					sr, err := eng.SyncTo(t.from, t.name, t.sel, !dryRun, force, a.logf())
					if sr != nil {
						printSyncReport(sr, !dryRun)
						rec.RunsCopied += sr.Copied()
						rec.BytesMoved += sr.CopiedBytes()
					}
					if err != nil {
						return rec, err
					}
				}
				return rec, nil
			}
			if dryRun {
				_, err := runSync()
				return err
			}
			return a.runReported(cfg, report.Run{Command: report.CommandSync, ExitClass: "sync-error"}, runSync)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "target medium (omit to run the config's sync: rules)")
	cmd.Flags().StringVar(&from, "from", "", "source medium (default: the landing medium)")
	cmd.Flags().IntVar(&last, "last", 0, "copy only the N most recent runs (0 = all); combined with --since, the newest N of those on/after the date")
	cmd.Flags().StringVar(&sinceStr, "since", "", "copy only runs dated on/after this date YYYY-MM-DD (intersects with --last)")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview without copying")
	cmd.Flags().BoolVar(&force, "force", false, "re-copy runs already recorded on the target")
	return cmd
}

// printSyncReport renders one target's backlog, matching the prune dry-run style.
func printSyncReport(r *engine.SyncReport, apply bool) {
	if len(r.Items) == 0 {
		fmt.Printf("%s -> %s: up to date\n", r.From, r.To)
		return
	}
	if apply {
		// Report the bytes that actually landed (copied runs), not the whole backlog —
		// a sync that stops partway (e.g. the target filled) must not claim it moved
		// bytes for runs it never copied.
		fmt.Printf("%s -> %s: copied %d run(s), %s\n", r.From, r.To, r.Copied(), sizeutil.FormatBytes(r.CopiedBytes()))
	} else {
		fmt.Printf("%s -> %s: %d run(s) to copy, %s (dry-run; re-run without --dry-run to copy):\n",
			r.From, r.To, len(r.Items), sizeutil.FormatBytes(r.Bytes()))
		for _, it := range r.Items {
			fmt.Printf("  %-24s %2d archive(s)  %s\n", it.RunID, it.Archives, sizeutil.FormatBytes(it.Bytes))
		}
	}
	// Sync copies regardless, but a target it pushes past capacity is worth flagging:
	// otherwise the overshoot only surfaces later, at the next `nb plan`/`nb prune`.
	if r.OverCapacity() {
		warnMediumOverCapacity(r.To, r.ProjectedBytes, r.TargetCapacity, !apply)
	}
}
