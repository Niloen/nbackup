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
						rec.DumpStats = dumpStats(s, cfg.WorkdirPath())
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
				return report.Run{
					Command:    report.CommandDump,
					RunID:      s.ID,
					Archives:   len(s.Archives),
					BytesMoved: s.TotalBytes(),
					DumpStats:  dumpStats(s, cfg.WorkdirPath()),
				}, nil
			})
		},
	}
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today); a --dry-run for a date behind the latest committed run may show a full, since incremental state reflects the most recent dump")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "plan the run for --date and print it without writing anything")
	return cmd
}

// dumpStats builds the per-DLE statistics for a sealed run's record: sizes,
// level, and files come from the seal (authoritative); the dump duration comes from
// the run-status snapshot the tracker just flushed (the same file `nb status` reads),
// matched by DLE name and level. When the snapshot is missing or stale, sizes are
// still recorded and timing is left zero (rendered as a dash).
func dumpStats(s *catalog.Run, workdir string) []report.DLEStat {
	type key struct {
		name  string
		level int
	}
	durations := map[key]float64{}
	if snap, err := progress.Load(workdir); err == nil && snap.RunID == s.ID {
		for _, d := range snap.DLEs {
			if !d.StartedAt.IsZero() && !d.EndedAt.IsZero() {
				durations[key{d.Name, d.Level}] = d.EndedAt.Sub(d.StartedAt).Seconds()
			}
		}
	}
	stats := make([]report.DLEStat, 0, len(s.Archives))
	for _, a := range s.Archives {
		stats = append(stats, report.DLEStat{
			DLE:     a.DLE,
			Host:    a.Host,
			Path:    a.Path,
			Level:   a.Level,
			Orig:    a.Uncompressed,
			Out:     a.Compressed,
			Files:   a.FileCount,
			Seconds: durations[key{a.DLEID(), a.Level}], // progress is keyed by host:path
		})
	}
	return stats
}

// runDumpDryRun previews the dump on `date` without writing: it plans that run
// exactly as `nb dump --date <date>` would — against the current catalog, the same
// decision logic a real run uses — and prints it. Nothing is sealed. `now` is the
// instant the run id is minted from (see newDumpCmd); a real run started later
// would carry a later time suffix.
func runDumpDryRun(eng *engine.Engine, date, now time.Time, validationWarnings []string) error {
	plan := eng.Plan(date)

	fmt.Println("DRY RUN — no data is written.")
	fmt.Printf("This is the run on %s.\n\n", record.DateString(date))
	warnings := append(validationWarnings, plan.Warnings...)
	for _, w := range warnings {
		fmt.Printf("WARNING: %s\n", w)
	}
	if len(warnings) > 0 {
		fmt.Println()
	}

	estTotal := fprintPlanItems(os.Stdout, plan)
	fmt.Printf("\nThis run (estimated): ~%s\n", sizeutil.FormatBytes(estTotal))
	fmt.Printf("Would commit %s. Run without --dry-run to execute.\n", eng.PlannedRunID(now))
	return nil
}
