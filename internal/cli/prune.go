package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/report"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// newPruneCmd implements `nb prune`: reclaim runs past the cycle/capacity limits.
func newPruneCmd(a *app) *cobra.Command {
	var dryRun bool
	var dateStr string
	cmd := &cobra.Command{
		Use:     "prune [medium]",
		Short:   "Delete runs past each medium's cycle/capacity limits",
		Long:    "Reclaim runs that fall outside their medium's own cycle and capacity limits. With no medium named, prunes every configured medium in turn (the hands-off form for cron, mirroring `nb sync` running every rule); name a medium to prune just that one. Either way retention is per-medium — pruning one store never touches a copy on another, and tape is a structural no-op (it recycles whole volumes by relabel, not runs), so a fleet-wide prune only reclaims disk/cloud. Deletes by default; pass --dry-run (-n) to preview. On a per-file medium (disk, cloud) it also sweeps crash leftovers — footer-less or torn files an interrupted run left behind, which no archive references — detected from the medium's own commit footers and bounded by minimum_age (so it never fights WORM/Object-Lock). Detection reads only the files the catalog does not already account for, so a large cloud store is diffed cheaply rather than re-read object by object. If the protected recovery set alone exceeds capacity, prune reclaims what it can, prints a WARNING, and still exits 0 (recoverability outranks capacity — grow capacity or lengthen the cycle); watch for it via `nb report`/notify rather than the exit code.",
		Example: "  nb prune\n  nb prune disk\n  nb prune disk --dry-run\n  nb prune offsite",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadForWrite()
			if err != nil {
				return err
			}
			// Resolve which media to prune. A named medium is validated up front (before
			// the run-reporting path): a typo'd medium is an argument error, not a failed
			// run — it must not land in the run log or fire notify.on_failure. No argument
			// fans out over every configured medium, each pruned against its own retention.
			var media []string
			if len(args) == 1 {
				if _, ok := cfg.Media[args[0]]; !ok {
					return fmt.Errorf("unknown medium %q (configured: %s)", args[0], strings.Join(mediaNames(cfg), ", "))
				}
				media = []string{args[0]}
			} else {
				media = landingFirst(cfg, mediaNames(cfg))
				if len(media) == 0 {
					return fmt.Errorf("no media configured to prune")
				}
			}
			// Dry-run prune only reads; a real run deletes runs, so lock it.
			eng, release, err := a.engineFor(cfg, !dryRun)
			if err != nil {
				return err
			}
			defer release()
			// Retention measures age from each run's commit instant, so the
			// reference 'now' must be a real wall-clock time, not a date truncated
			// to midnight — otherwise a sub-day minimum_age can never elapse within
			// the run day. An explicit --date stays a coarse, reproducible override.
			now := time.Now().UTC()
			if dateStr != "" {
				if now, err = ParseDate(dateStr); err != nil {
					return err
				}
			}

			runPrune := func() (report.Run, error) {
				rec := report.Run{Command: report.CommandPrune}
				for _, name := range media {
					eligible, swept, freed, err := eng.Prune(name, now, !dryRun, a.logf())
					if err != nil {
						return rec, err
					}
					printPruneResult(eng, name, eligible, swept, freed, !dryRun)
					warnIfOverCapacity(eng, name, now)
					rec.ArchivesPruned += eligible
					rec.BytesMoved += freed
				}
				return rec, nil
			}
			if dryRun {
				_, err := runPrune()
				return err
			}
			return a.runReported(cfg, report.Run{Command: report.CommandPrune, ExitClass: "prune-error"}, runPrune)
		},
	}
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview without deleting")
	cmd.Flags().StringVar(&dateStr, "date", "", "reference 'now' date YYYY-MM-DD (default: the current time)")
	return cmd
}

// printPruneResult renders one medium's prune outcome, for both the dry-run preview
// and the applied run. Each line is medium-prefixed so a fleet-wide `nb prune` reads
// cleanly, one medium per line.
func printPruneResult(eng *engine.Engine, name string, eligible, swept int, freed int64, apply bool) {
	if apply {
		if eligible > 0 {
			fmt.Printf("\n%s: deleted %d archive(s), freed %s\n", name, eligible, sizeutil.FormatBytes(freed))
		} else if swept == 0 {
			printNothingToReclaim(eng, name)
		}
		if swept > 0 {
			fmt.Printf("%s: swept %d crash leftover(s) (orphaned by an interrupted run, no commit footer)\n", name, swept)
		}
		return
	}
	if eligible > 0 {
		fmt.Printf("\n%s: %d archive(s) eligible. Re-run without --dry-run to delete.\n", name, eligible)
	} else if swept == 0 {
		printNothingToReclaim(eng, name)
	}
	if swept > 0 {
		fmt.Printf("%s: %d crash leftover(s) to sweep (orphaned by an interrupted run, no commit footer).\n", name, swept)
	}
}

// newFlushCmd implements `nb flush`: drain a crashed holding-disk run's leftover archives to
// the landing (Amanda's amflush). A normal `nb dump` already auto-flushes leftovers first, so
// this is the explicit, attended form.
func newFlushCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "flush",
		Short:   "Drain leftover holding-disk archives to the landing",
		Long:    "Copy any archives a crashed holding-disk run left on the holding disk to the landing, then reclaim the disk. The catalog already records what is on the holding disk, so no media scan is needed. `nb dump` runs this automatically before each run; use `nb flush` to drain explicitly. A no-op without a holding disk or when nothing is staged.",
		Example: "  nb flush   # after a crashed dump, before the next scheduled run",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadForWrite()
			if err != nil {
				return err
			}
			eng, release, err := a.engineFor(cfg, true) // writes media + catalog: lock it
			if err != nil {
				return err
			}
			defer release()
			attachOperator(eng)
			return a.runReported(cfg, report.Run{Command: report.CommandFlush, ExitClass: "flush-error"}, func() (report.Run, error) {
				n, err := eng.Flush(time.Now().UTC(), a.logf())
				if err != nil {
					return report.Run{}, err
				}
				if n == 0 {
					// Nothing staged (no holding disk, or already drained) — a no-op, not a run:
					// don't pollute the run log / nb report with an empty "OK" flush.
					fmt.Println("nothing to flush")
					return report.Run{}, skip(nil)
				}
				fmt.Printf("flushed %d archive(s) to the landing\n", n)
				return report.Run{Command: report.CommandFlush, RunsCopied: n}, nil
			})
		},
	}
	return cmd
}

// newResetCmd implements `nb reset <dle>`: schedule a DLE for a full on its next run. The
// escape hatch when an incremental chain has gone bad — an interrupted dump that left a
// dead snapshot, or a base that no longer matches the source.
func newResetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "reset <dle>",
		Short:   "Schedule a DLE for a full on its next run",
		Long:    "Mark a DLE so the next `nb dump` backs it up at level 0, starting a fresh incremental chain. Use this when an incremental chain has gone bad — e.g. a dump interrupted out of space left a dead snapshot, so incrementals would re-dump everything anyway. This records a force-full directive in the catalog that the planner honors (the archiver-independent peer of Amanda's `amadmin force`); it touches no incremental state, so the existing chain stays intact until the new full actually commits. The directive is consumed once the forced full runs. The DLE is named by its host:path identity (as `nb plan` shows) or its config name.",
		Example: "  nb reset web1:/var/www",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadForWrite()
			if err != nil {
				return err
			}
			eng, release, err := a.engineFor(cfg, true) // writes the catalog directive: lock out a concurrent dump
			if err != nil {
				return err
			}
			defer release()
			id, err := eng.ForceFull(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s will be fulled on its next run\n", id)
			return nil
		},
	}
}

// mediaNames returns the config's medium names, sorted for a stable listing in
// prompts and error hints.
func mediaNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Media))
	for name := range cfg.Media {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// landingFirst reorders an alphabetical medium list so the landing medium — the
// operationally primary one — is reported first in a fleet-wide `nb prune`, with
// the rest staying alphabetical (config declaration order isn't preserved through
// YAML-to-map decoding, so this is the closest stable, meaningful ordering).
func landingFirst(cfg *config.Config, names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == cfg.Landing.Primary() {
			out = append(out, n)
		}
	}
	for _, n := range names {
		if n != cfg.Landing.Primary() {
			out = append(out, n)
		}
	}
	return out
}

// printNothingToReclaim explains a zero-reclaim prune. Tape reclaims whole volumes
// (relabel), never individual runs, so "fits capacity" would be misleading there —
// it is a no-op by design even when the library is over capacity. Say so, and point
// at the deliberate recycle path, so a user lowering tape capacity isn't told the
// runs "fit" when per-run pruning simply does not apply to tape.
func printNothingToReclaim(eng *engine.Engine, name string) {
	if info, ok := eng.Medium(name); ok && info.Type == "tape" {
		if info.Capacity > 0 && info.Used > info.Capacity {
			fmt.Printf("\n%s: over capacity (%s of %s), but tape reclaims whole volumes, not runs — recycle an aged-out tape with `nb label --relabel` (per-run pruning does not apply to tape)\n",
				name, sizeutil.FormatBytes(info.Used), sizeutil.FormatBytes(info.Capacity))
		} else {
			fmt.Printf("\n%s: nothing to reclaim — tape reclaims whole volumes, not runs (recycle an aged-out tape with `nb label --relabel`)\n", name)
		}
		return
	}
	fmt.Printf("\n%s: nothing to reclaim (all runs fit capacity or are protected)\n", name)
}

// warnIfOverCapacity closes the plan→prune loop: when a prune leaves a medium still
// over capacity it is because the protected recovery set alone exceeds capacity, so
// say so rather than reporting "freed N" and silently leaving the medium over budget.
// Tape is excluded — its whole-volume over-capacity case is covered by
// printNothingToReclaim's recycle hint.
func warnIfOverCapacity(eng *engine.Engine, medium string, now time.Time) {
	if info, ok := eng.Medium(medium); ok && info.Type == "tape" {
		return
	}
	// Use the post-reclamation residual (the protected set), not the raw catalog
	// total, so the dry-run preview and the real run report the same thing — a
	// dry-run still has the would-delete archives in the catalog.
	over, residual, capacity, err := eng.MediumProtectedOverCapacity(medium, now)
	if err != nil || !over {
		return
	}
	remedy := "raise its capacity or shorten minimum_age"
	if !eng.MediumProtectionIsAgeBound(medium, now) {
		// The binding pins are live recovery chains, which shortening minimum_age
		// cannot release — only more capacity or a longer cycle helps.
		remedy = "raise its capacity or lengthen the cycle"
	}
	fmt.Printf("WARNING: %q still holds %s, over its %s capacity — reclaiming every dead archive was not enough; the protected recovery set exceeds capacity, so %s\n",
		medium, sizeutil.FormatBytes(residual), sizeutil.FormatBytes(capacity), remedy)
}
