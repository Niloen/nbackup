package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/report"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// drillRunRecord maps a DrillReport (plus each DLE's prior ledger outcome) into the
// uniform run record: failure/skip/overdue counts, the never-drilled list, and a
// per-DLE health row so a digest can flag a DLE that degraded since its last drill.
func drillRunRecord(dr *engine.DrillReport, priorOK map[string]bool) report.Run {
	rec := report.Run{
		Command:      report.CommandDrill,
		Tier:         dr.Tier.String(),
		Failures:     dr.Failures,
		Skipped:      dr.Skipped,
		Overdue:      dr.Overdue,
		NeverDrilled: dr.NeverDrilled,
	}
	for _, t := range dr.Targets {
		was, seen := priorOK[t.DLE]
		drilled := t.Class != drill.ClassSkipped
		h := report.DrillHealth{
			DLE:     t.DLE,
			OK:      t.OK,
			Class:   t.Class.String(),
			WasOK:   seen && was,
			Drilled: drilled,
		}
		if drilled {
			h.Bytes = t.Bytes
		}
		rec.DrillHealth = append(rec.DrillHealth, h)
		// "Bytes moved" for a drill is the egress it actually read off the medium —
		// the drilled chain payload of each target it exercised (a skipped target read
		// nothing). This is what the report/webui history shows, so a drill row reads
		// its true size instead of 0 B.
		rec.BytesMoved += h.Bytes
	}
	return rec
}

// newDrillCmd implements `nb drill`: the recovery-drill orchestration layered on
// `nb verify`. It selects a risk-biased subset of DLEs, exercises each end-to-end at
// the chosen tier against a source copy, records a recoverability ledger, probes the
// medium for WORM/immutability, and prints a 3-2-1-1-0 posture audit. Runs by
// default (like `nb prune`/`nb sync`); `--dry-run` (`-n`) previews without cost.
func newDrillCmd(a *app) *cobra.Command {
	var (
		dryRun, unattended, worm bool
		asOf, window, from, tier string
		sample                   int
	)
	cmd := &cobra.Command{
		Use:   "drill",
		Short: "Rehearse recovery: prove backups are restorable, not just intact",
		Long: "Run a recovery drill — the recoverability proof checksum verification cannot give " +
			"(a lost key, a compression/tar drift, a broken incremental chain, an unreadable offsite copy). " +
			"It selects a risk-biased subset of DLEs (rotating so each is drilled within a window, " +
			"prioritizing the longest chains and oldest fulls), exercises each at a tier — checksum, " +
			"structural (decrypt+decompress+`tar -t`), a real point-in-time chain restore to scratch, " +
			"or the documented stock-tools one-liner — records an inspectable ledger, probes the medium " +
			"for WORM/immutability, and prints a 3-2-1-1-0 posture audit. Runs by default; pass " +
			"--dry-run (-n) to preview without cost. Use --from to drill the offsite copy, and --unattended for cron (it " +
			"never prompts and skips any target that would need a tape swap). Exits non-zero on any " +
			"classified drill failure.\n\n" +
			"Tiers (cheapest → strongest), set with --tier (default structural):\n" +
			"  sample      re-hash ONE part per archive against its per-part seal — bounded egress; successive drills rotate through the parts\n" +
			"  checksum    re-hash stored bytes against the commit footer — integrity only, no decode\n" +
			"  structural  decrypt+decompress+`tar -t` — proves a valid restorable stream, writes nothing\n" +
			"  chain       real point-in-time restore (full+incrementals) to scratch — the strong proof\n" +
			"  stock       restore via the documented gpg/zstd/tar one-liner — proves recovery needs no NBackup",
		Example: "  nb drill\n  nb drill --dry-run\n  nb drill --from offsite --tier structural\n  nb dump && nb sync && nb drill --unattended",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadForWrite()
			if err != nil {
				return err
			}

			// Resolve options: a CLI flag the user set overrides the config `drill:`
			// block (uniformly via Flags().Changed, so an explicit empty/zero flag
			// still wins). The config side owns its own parsing/defaults.
			win := cfg.DrillWindow()
			if cmd.Flags().Changed("window") {
				d, perr := sizeutil.ParseDuration(window)
				if perr != nil {
					return fmt.Errorf("--window: %w", perr)
				}
				win = d
			}
			if !cmd.Flags().Changed("sample") {
				sample = cfg.DrillSample()
			}
			if !cmd.Flags().Changed("from") {
				from = cfg.Drill.From
			}
			if !cmd.Flags().Changed("tier") {
				tier = cfg.DrillTierName()
			}
			t, err := drill.ParseTier(tier)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("worm") {
				worm = cfg.Drill.Worm
			}
			// Unattended: explicit flag, else the config, else auto when stdin is not a
			// terminal (a cron/pipe context) — so a piped run never blocks on a prompt.
			if !cmd.Flags().Changed("unattended") {
				unattended = cfg.Drill.Unattended || !stdinIsTerminal()
			}

			date, err := ParseDate(asOf)
			if err != nil {
				return err
			}

			opts := engine.DrillOptions{
				AsOf:       record.DateString(date),
				Window:     win,
				Sample:     sample,
				Medium:     from,
				Tier:       t,
				Worm:       worm,
				Unattended: unattended,
				Apply:      !dryRun,
				Now:        time.Now().UTC(),
			}

			// Dry-run only reads; a real run writes the ledger + WORM probe, so lock it.
			eng, release, err := a.engineFor(cfg, !dryRun)
			if err != nil {
				return err
			}
			defer release()
			if !dryRun && !unattended {
				eng.SetOperator(stdinOperator{}) // attended: may prompt for a tape swap
			}

			if dryRun {
				dr, err := eng.Drill(opts, a.logf())
				if err != nil {
					return err
				}
				printDrillReport(dr)
				return nil
			}

			// Capture each DLE's prior ledger outcome so the run record can flag a DLE
			// that *degraded* (passed before, fails now) versus a first-time failure.
			priorOK := map[string]bool{}
			if prior, lerr := drill.Load(cfg.WorkdirPath()); lerr == nil {
				for _, rec := range prior.Records {
					priorOK[rec.DLE] = rec.OK
				}
			}
			return a.runReported(cfg, report.Run{Command: report.CommandDrill, ExitClass: "drill-failures"}, func() (report.Run, error) {
				dr, err := eng.Drill(opts, a.logf())
				if err != nil {
					return report.Run{}, err
				}
				printDrillReport(dr)
				rec := drillRunRecord(dr, priorOK)
				if dr.Failures > 0 {
					return rec, fmt.Errorf("%d drill failure(s) — recovery is at risk", dr.Failures)
				}
				return rec, nil
			})
		},
	}
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview the drill without running it")
	cmd.Flags().BoolVar(&unattended, "unattended", false, "cron mode: never prompt; skip targets needing a tape swap")
	cmd.Flags().BoolVar(&worm, "worm", false, "probe the medium for WORM/immutability (skipped in --dry-run)")
	cmd.Flags().StringVar(&asOf, "as-of", "", "drill a point-in-time YYYY-MM-DD (default today)")
	cmd.Flags().StringVar(&window, "window", "", "each DLE should be drilled within this window (e.g. 30d); pass 0 to drill now regardless of when each was last drilled")
	cmd.Flags().IntVar(&sample, "sample", 1, "max DLEs to drill this run")
	cmd.Flags().StringVar(&from, "from", "", "source medium to drill (default: the landing medium)")
	cmd.Flags().StringVar(&tier, "tier", "", "drill depth (default structural); see Tiers above for each level")
	return cmd
}

// printDrillReport renders a drill report: what ran (or would run), the WORM/posture
// audit, and the SLO framing.
func printDrillReport(r *engine.DrillReport) {
	mode := "dry run"
	if r.Apply {
		mode = "apply"
	}
	fmt.Printf("Recovery drill — %s (as of %s, medium %q, tier %s%s)\n\n",
		mode, r.AsOf, r.Medium, r.Tier, unattendedTag(r.Unattended))

	if len(r.Targets) == 0 {
		fmt.Printf("No DLEs due to drill (every DLE drilled within %s).\n", sizeutil.FormatDuration(r.Window))
		fmt.Printf("To drill now regardless of coverage, pass --window 0 (with --tier/--sample to pick depth and count).\n\n")
	} else {
		verb := "Would drill"
		if r.Apply {
			verb = "Drilled"
		}
		fmt.Printf("%s %d DLE(s) (window %s, sample of the riskiest):\n", verb, len(r.Targets), sizeutil.FormatDuration(r.Window))
		tw := newTab(os.Stdout)
		if r.Apply {
			fmt.Fprintln(tw, "  DLE\tAS-OF\tRUN\tEGRESS\tRESULT")
			for _, t := range r.Targets {
				result := "OK"
				if t.Class == drill.ClassSkipped {
					result = "SKIPPED: " + t.Detail
				} else if !t.OK {
					result = fmt.Sprintf("FAIL [%s]: %s", t.Class, t.Detail)
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", t.DLEDisplay, t.AsOf, t.RunID, sizeutil.FormatBytes(t.Bytes), result)
			}
		} else {
			fmt.Fprintln(tw, "  DLE\tAS-OF\tRUN\tEGRESS")
			for _, t := range r.Targets {
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", t.DLEDisplay, t.AsOf, t.RunID, sizeutil.FormatBytes(t.Bytes))
			}
		}
		tw.Flush()
		if r.Priced {
			fmt.Printf("Forecast egress (drilled bytes read off %q): %s — ~%s (%s)\n\n",
				r.Medium, sizeutil.FormatBytes(r.ForecastBytes), formatUSD(r.ForecastCost), r.Provider)
		} else {
			fmt.Printf("Forecast egress (drilled bytes read off %q): %s\n\n", r.Medium, sizeutil.FormatBytes(r.ForecastBytes))
		}
	}

	// Coverage / SLO.
	fmt.Printf("Coverage: %d DLE(s) not yet covered within %s", r.Overdue, sizeutil.FormatDuration(r.Window))
	if n := len(r.NeverDrilled); n > 0 {
		fmt.Printf(" (%d never drilled)", n)
	}
	fmt.Println()
	if r.Apply {
		status := "MET"
		if !r.SLOMet() {
			status = "NOT MET"
		}
		fmt.Printf("SLO (0 failures this run): %s — %d failure(s), %d skipped\n", status, r.Failures, r.Skipped)
	}
	fmt.Println()

	// WORM + 3-2-1-1-0 posture audit.
	fmt.Println("Recoverability posture (3-2-1-1-0)")
	ptw := newTab(os.Stdout)
	for _, c := range r.Posture.Checks {
		fmt.Fprintf(ptw, "  [%s]\t%s\t%s\n", c.Status, c.Name, c.Detail)
	}
	ptw.Flush()

	if !r.Apply {
		fmt.Println("\nDry run — re-run without --dry-run (-n) to execute the drill.")
	}
}

func unattendedTag(unattended bool) string {
	if unattended {
		return ", unattended"
	}
	return ""
}
