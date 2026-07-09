package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/report"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// newDumpCmd implements `nb dump`: execute a run and seal a run, or — with
// --dry-run — plan the run for --date and print it without writing anything.
func newDumpCmd(a *app) *cobra.Command {
	var dateStr string
	var dryRun bool
	cmd := &cobra.Command{
		Use:     "dump",
		Short:   "Execute a run and commit its archives",
		Long:    "Execute a planner run, dumping each scheduled DLE and committing exactly one immutable run's archives. Use --quiet to suppress progress output. --date sets the run date (with or without --dry-run). With --dry-run the run for --date is planned against the current catalog and printed, exactly as a real dump would decide it, but nothing is written.",
		Example: "  nb dump\n  nb dump --date 2026-06-21\n  nb dump --dry-run --date 2026-07-15\n  nb -c prod.yaml dump -q",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadForWrite()
			if err != nil {
				return err
			}
			date, err := ParseDate(dateStr)
			if err != nil {
				return err
			}
			// The run's single time source: the instant it is stamped committed, the
			// moment retention is judged against, and the wall clock its id is minted
			// from. An explicit --date pins the instant to that date's midnight — a
			// coarse but reproducible override; the run date (used for guards and
			// planning) is only the instant's day.
			now := date
			if dateStr == "" {
				now = time.Now().UTC()
			}
			if dryRun {
				if err := errPastPlan(date); err != nil {
					return err
				}
				eng, err := engine.New(cfg)
				if err != nil {
					return err
				}
				warnings, err := eng.ValidatePlan()
				if err != nil {
					return err
				}
				return runDumpDryRun(eng, date, now, warnings)
			}
			if err := errPastDump(date); err != nil {
				return err
			}
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			attachOperator(eng)
			eng.SetEstimateProgress(estimateProgress(a.quiet))
			eng.SetRunProgress(runProgress(a.quiet))
			return a.runReported(cfg, report.Run{Command: report.CommandDump, ExitClass: "dump-failed"}, func() (report.Run, error) {
				s, err := eng.Run(cmd.Context(), now, a.logf())
				if err != nil {
					// A canceled run is operator-initiated, not a dump failure: record it under its
					// own exit class so the run log distinguishes the two.
					if errors.Is(err, conductor.ErrCanceled) {
						return report.Run{Command: report.CommandDump, ExitClass: "canceled"}, err
					}
					// A failed run may still have committed archives (a partial dump commits a
					// valid archive of what was readable). Record what landed — run id and per-DLE
					// stats — so `nb report --dump --run <id>` finds the run in the history.
					rec := report.Run{Command: report.CommandDump}
					if s != nil && len(s.Archives) > 0 {
						rec.RunID = s.ID
						rec.Archives = len(s.Archives)
						rec.BytesMoved = s.TotalBytes()
						rec.DumpStats, rec.LandingStats = dumpStats(s, cfg.WorkdirPath(), restSlugs(eng))
						rec.Warnings = landingWarnings(cfg.WorkdirPath(), s.ID)
					}
					return rec, err
				}
				// The blank line separates the commit line from the progress stream
				// above it; --quiet printed no stream, so don't lead with one.
				sep := "\n"
				if a.quiet {
					sep = ""
				}
				fmt.Printf(sep+"Committed %s: %d archive(s), %s total\n", s.ID, len(s.Archives), sizeutil.FormatBytes(s.TotalBytes()))
				stats, landings := dumpStats(s, cfg.WorkdirPath(), restSlugs(eng))
				return report.Run{
					Command:      report.CommandDump,
					RunID:        s.ID,
					Archives:     len(s.Archives),
					BytesMoved:   s.TotalBytes(),
					DumpStats:    stats,
					LandingStats: landings,
					Warnings:     landingWarnings(cfg.WorkdirPath(), s.ID),
				}, nil
			})
		},
	}
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today); a --dry-run for a date behind the latest committed run may show a full, since incremental state reflects the most recent dump")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "plan the run for --date and print it without writing anything")
	return cmd
}

// dumpStats builds the per-DLE and per-landing statistics for a sealed run's record:
// sizes, level, and files come from the seal (authoritative); every duration and the
// planner's level reason come from the run-status snapshot the tracker just flushed
// (the same file `nb status` reads), matched by DLE name and level. The dump duration
// ends at the DLE's DumpEndedAt — not its EndedAt, which a flushed DLE moves to its
// drain's end, so using it would silently fold the queue wait and the flush into
// "dump time". The flush columns and the landing stats carry the write side: bytes
// over *busy* seconds, so an idle-most-of-the-run drainer is never read as a slow
// device. When the snapshot is missing or stale, sizes are still recorded and
// timing/reason are left zero (rendered as a dash / omitted).
func dumpStats(s *catalog.Run, workdir string, rest map[string]bool) ([]report.DLEStat, []report.LandingStat) {
	type key struct {
		name  string
		level int
	}
	type planned struct {
		seconds      float64
		reason       string
		promoted     bool
		flushBytes   int64
		flushSeconds float64
	}
	plans := map[key]planned{}
	var landings []report.LandingStat
	if snap, err := progress.Load(workdir); err == nil && snap.RunID == s.ID {
		for _, d := range snap.DLEs {
			p := planned{reason: d.Reason, promoted: d.Promoted}
			end := d.DumpEndedAt
			if end.IsZero() {
				end = d.EndedAt // pre-flush terminal states (failed/canceled mid-dump)
			}
			if !d.StartedAt.IsZero() && !end.IsZero() {
				p.seconds = end.Sub(d.StartedAt).Seconds()
			}
			if d.Drains() {
				p.flushBytes, p.flushSeconds = d.DrainBytes, d.WriteSeconds
			}
			plans[key{d.Name, d.Level}] = p
		}
		end := snap.EndedAt
		if end.IsZero() {
			end = snap.UpdatedAt
		}
		wall := snap.Elapsed(end).Seconds()
		for _, name := range snap.Landings() {
			busy := snap.WriteBusy(name, end)
			written := snap.WrittenTo(name)
			if busy <= 0 && written == 0 {
				continue
			}
			landings = append(landings, report.LandingStat{
				Landing: name, Bytes: written, BusySeconds: busy, WallSeconds: wall,
			})
		}
	}
	stats := make([]report.DLEStat, 0, len(s.Archives))
	for _, a := range s.Archives {
		p := plans[key{a.DLEID(), a.Level}] // progress is keyed by host:path
		stats = append(stats, report.DLEStat{
			DLE:          a.DLE,
			Host:         a.Host,
			Path:         a.Path,
			Level:        a.Level,
			Orig:         a.Uncompressed,
			Out:          a.Compressed,
			Files:        a.FileCount,
			Seconds:      p.seconds,
			Promoted:     p.promoted,
			Reason:       p.reason,
			FlushBytes:   p.flushBytes,
			FlushSeconds: p.flushSeconds,
			Rest:         rest[a.DLE],
		})
	}
	return stats, landings
}

// restSlugs collects the partition-remainder slugs of the run's resolved set
// (recorded at plan-commit, so it reflects THIS run), letting each DLEStat carry
// its Rest mark into the historical record — the dump table's "(the rest)" label.
func restSlugs(eng *engine.Engine) map[string]bool {
	rest := map[string]bool{}
	for _, r := range eng.Catalog().LatestResolved() {
		if r.Rest {
			rest[r.DLE] = true
		}
	}
	return rest
}

// landingWarnings reads the run's landing degradations off the run-status snapshot
// the tracker just flushed (the same file `nb status` reads, matched by run id) —
// each landing the run skipped up front or tripped mid-run becomes one warning on
// the run record, so the report, the notification, and the web UI say WARNING
// instead of a clean OK while copies are missing.
func landingWarnings(workdir, runID string) []string {
	snap, err := progress.Load(workdir)
	if err != nil || snap.RunID != runID {
		return nil
	}
	warnings := make([]string, 0, len(snap.Skipped))
	for _, sk := range snap.Skipped {
		warnings = append(warnings, sk.Warning(runID))
	}
	return warnings
}

// runDumpDryRun previews the dump on `date` without writing: it plans that run
// exactly as `nb dump --date <date>` would — against the current catalog, the same
// decision logic a real run uses — and prints it. Nothing is sealed. `now` is the
// instant the run id is minted from (see newDumpCmd); a real run started later
// would carry a later time suffix.
func runDumpDryRun(eng *engine.Engine, date, now time.Time, validationWarnings []string) error {
	plan, err := eng.Plan(date)
	if err != nil {
		return err
	}

	fmt.Println("DRY RUN — no data is written.")
	fmt.Printf("This is the run on %s.\n\n", record.DateString(date))
	warnings := append(validationWarnings, plan.Warnings...)
	for _, w := range warnings {
		fmt.Printf("WARNING: %s\n", w)
	}
	if len(warnings) > 0 {
		fmt.Println()
	}

	estTotal, unknownEst := fprintPlanItems(os.Stdout, plan)
	fmt.Printf("\nThis run (estimated): %s\n", runEstimateLine(estTotal, unknownEst))
	fmt.Printf("Would commit %s. Run without --dry-run to execute.\n", eng.PlannedRunID(now))
	return nil
}
