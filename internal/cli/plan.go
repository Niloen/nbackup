package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// maxForecastDays bounds `nb plan --days`: the per-day forecast cost (Simulate's
// bounded history scans, and ForecastCost's per-day scan of the accumulating
// working set of archives) grows with both the window and the medium's archive/DLE
// count, so an unbounded --days can turn a routine capacity preview into a command
// that runs for minutes with no output. 1000 days (~2.7 years) comfortably covers
// any realistic capacity-planning horizon while keeping the worst case fast.
const maxForecastDays = 1000

// newPlanCmd implements `nb plan`: show what the next run would do, or — with
// --days — an extended forecast of running daily for that many days.
func newPlanCmd(a *app) *cobra.Command {
	var dateStr string
	var days int
	cmd := &cobra.Command{
		Use:     "plan",
		Short:   "Show what the next run would do",
		Long:    "Preview the next run: which DLEs would be dumped at which level, estimated sizes, and capacity/budget status. With --days N, forecast N consecutive daily runs instead, projecting the schedule forward day-by-day (when fulls land, how incrementals climb). Reads only; nothing is written.",
		Example: "  nb plan\n  nb plan --date 2026-06-21\n  nb plan --days 30",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if days < 1 {
				return fmt.Errorf("--days must be at least 1")
			}
			if days > maxForecastDays {
				return fmt.Errorf("--days %d exceeds the %d-day forecast ceiling (the simulation cost grows with the window and the media/DLE count; a shorter window is representative enough for capacity planning)", days, maxForecastDays)
			}
			cfg, err := a.loadForWrite()
			if err != nil {
				return err
			}
			eng, err := engine.New(cfg)
			if err != nil {
				return err
			}
			date, err := ParseDate(dateStr)
			if err != nil {
				return err
			}
			if err := errPastPlan(date); err != nil {
				return err
			}
			warnings, err := eng.ValidatePlan()
			if err != nil {
				return err
			}
			for _, w := range warnings {
				fmt.Printf("WARNING: %s\n", w)
			}
			if len(warnings) > 0 {
				fmt.Println()
			}
			if days > 1 {
				return runPlanForecast(eng, date, days)
			}

			plan := eng.PlanWithProgress(date, estimateProgress(a.quiet))
			fmt.Printf("Plan for run %s  (cycle %dd, landing %q)\n\n",
				record.DateString(date), plan.Interval, strings.Join(eng.Landings(), ", "))
			for _, w := range plan.Warnings {
				fmt.Printf("WARNING: %s\n", w)
			}
			if len(plan.Warnings) > 0 {
				fmt.Println()
			}

			estTotal := fprintPlanItems(os.Stdout, plan)

			current := eng.StoredBytes()
			capacity := eng.Capacity()
			fmt.Printf("\nCatalog currently stored: %s\n", sizeutil.FormatBytes(current))
			fmt.Printf("This run (estimated): ~%s\n", sizeutil.FormatBytes(estTotal))
			if capacity > 0 {
				over, pct := eng.CapacityStatus(current)
				fmt.Printf("Capacity: %s (%.1f%% used)\n", sizeutil.FormatBytes(capacity), pct)
				if over {
					fmt.Printf("WARNING: over capacity; run `nb prune` to reclaim oldest runs\n")
				} else if current+estTotal > capacity {
					// "% used" counts only what is already stored, so a run far larger
					// than capacity can still show a small percentage — call out that
					// this run would not fit rather than leaving the reader to compare.
					fmt.Printf("WARNING: this run (~%s) would exceed the %s capacity — grow capacity or lengthen the cycle\n",
						sizeutil.FormatBytes(estTotal), sizeutil.FormatBytes(capacity))
				}
			} else {
				fmt.Printf("Capacity: unbounded\n")
			}
			if cs := eng.CostSummary(plan); cs.Priced {
				fmt.Printf("Est. storage cost (%s): %s/month for %s stored\n",
					cs.Provider, formatUSD(cs.Monthly), sizeutil.FormatBytes(cs.Bytes))
				fmt.Printf("This run adds: ~%s/month (%s)\n",
					formatUSD(cs.Marginal), sizeutil.FormatBytes(cs.RunBytes))
			}
			if exp, ok := eng.ExpectedVolume(date); ok {
				fmt.Printf("Tape: %s\n", describeExpectation(exp, date))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dateStr, "date", "", "run date YYYY-MM-DD (default today); planning a date behind the latest committed run may show a full, since incremental state reflects the most recent dump")
	cmd.Flags().IntVar(&days, "days", 1, "forecast this many consecutive daily runs from --date")
	return cmd
}

// fprintPlanItems writes a plan's per-DLE level/size/reason table to w and returns
// the total estimated bytes. Shared by `nb plan` and the `nb dump --dry-run`
// preview so a single run renders identically in both.
func fprintPlanItems(w io.Writer, plan *planner.Plan) int64 {
	tw := newTab(w)
	fmt.Fprintln(tw, "DLE\tLEVEL\tEST. SIZE\tFULL SIZE\tREASON")
	var estTotal int64
	for _, item := range plan.Items {
		levelStr := fmt.Sprintf("L%d (full)", item.Level)
		// For an incremental, show the full-dump size alongside the chosen size so a
		// small incremental does not hide a large full waiting at the cycle deadline.
		// For a full the two are identical, so leave the column blank to avoid noise.
		fullStr := "-"
		if item.Level >= 1 {
			levelStr = fmt.Sprintf("L%d (incr)", item.Level)
			fullStr = "~" + sizeutil.FormatBytes(item.FullBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t~%s\t%s\t%s\n", item.DLE.ID(), levelStr, sizeutil.FormatBytes(item.EstBytes), fullStr, item.Reason)
		estTotal += item.EstBytes
	}
	tw.Flush()
	return estTotal
}

// runPlanForecast renders an extended plan: one row per simulated daily run,
// projecting the level schedule forward. Estimates are sampled once and held
// constant (see engine.Simulate), so the per-day size tracks the chosen levels.
func runPlanForecast(eng *engine.Engine, start time.Time, days int) error {
	plans := eng.Simulate(start, days)
	fmt.Printf("Forecast: %d daily runs from %s  (cycle %dd, landing %q)\n\n",
		days, record.DateString(start), plans[0].Interval, strings.Join(eng.Landings(), ", "))

	// Structural warnings (e.g. a recovery set that won't fit capacity) are
	// constant across the window; surface each one once, above the schedule.
	seen := map[string]bool{}
	for _, p := range plans {
		for _, w := range p.Warnings {
			if !seen[w] {
				seen[w] = true
				fmt.Printf("WARNING: %s\n", w)
			}
		}
	}
	if len(seen) > 0 {
		fmt.Println()
	}

	// The cost curve overlays the schedule: the projected $/month footprint at the
	// end of each day as runs land and pruning reclaims. Only shown for a priced
	// (cloud) landing medium; a local disk has no recurring bill.
	curve := eng.ForecastCost(start, days)
	priced := eng.CostSummary(nil).Priced

	tw := newTab(os.Stdout)
	if priced {
		fmt.Fprintln(tw, "DATE\tFULL\tINCR\tEST. SIZE\t$/MONTH\tFULLS")
	} else {
		fmt.Fprintln(tw, "DATE\tFULL\tINCR\tEST. SIZE\tFULLS")
	}
	var windowTotal int64
	var totalIncrs int
	for i, p := range plans {
		var fulls, incrs int
		var est int64
		var fullNames []string
		for _, it := range p.Items {
			est += it.EstBytes
			if it.Level == 0 {
				fulls++
				fullNames = append(fullNames, it.DLE.ID())
			} else {
				incrs++
			}
		}
		windowTotal += est
		totalIncrs += incrs
		names := strings.Join(fullNames, ", ")
		if names == "" {
			names = "-"
		}
		if priced {
			fmt.Fprintf(tw, "%s\t%d\t%d\t~%s\t%s\t%s\n",
				record.DateString(p.Date), fulls, incrs, sizeutil.FormatBytes(est),
				formatUSD(curve[i].Monthly), names)
		} else {
			fmt.Fprintf(tw, "%s\t%d\t%d\t~%s\t%s\n",
				record.DateString(p.Date), fulls, incrs, sizeutil.FormatBytes(est), names)
		}
	}
	tw.Flush()
	if totalIncrs > 0 {
		// The simulation replays the level schedule but never mutates the filesystem,
		// so no bytes change between simulated runs and every forecast incremental sizes
		// to 0 B. Say so, lest a reader read "~0 B" as a broken estimate; a real
		// incremental is a fraction of its full (shown in the FULL/INCR counts).
		fmt.Println("\nNote: a forecast incremental over a simulated full shows ~0 B because the simulation makes no filesystem changes between runs; a real incremental is a fraction of its full.")
	}
	fmt.Printf("\nWindow total (estimated): ~%s over %d run(s)\n", sizeutil.FormatBytes(windowTotal), days)
	if priced && len(curve) > 0 {
		last := curve[len(curve)-1]
		fmt.Printf("Projected storage cost at end of window: %s/month (%s stored)\n",
			formatUSD(last.Monthly), sizeutil.FormatBytes(last.Bytes))
	}
	return nil
}

// describeExpectation renders the tape the next run will write to for `nb plan`,
// with the volume's age relative to the run date.
func describeExpectation(exp engine.VolumeExpectation, date time.Time) string {
	if exp.Appendable {
		if exp.FreshVolume {
			return fmt.Sprintf("expects a fresh tape (pool %q is empty)", exp.Medium)
		}
		if exp.VolumeBytes > 0 {
			return fmt.Sprintf("appends to %q (%s of %s used, %s free on this reel)",
				exp.Label, sizeutil.FormatBytes(exp.UsedBytes), sizeutil.FormatBytes(exp.VolumeBytes),
				sizeutil.FormatBytes(exp.VolumeBytes-exp.UsedBytes))
		}
		return fmt.Sprintf("appends to %q", exp.Label)
	}
	if exp.FreshVolume {
		return fmt.Sprintf("expects a fresh tape (no reusable volume in pool %q)", exp.Medium)
	}
	age := int(date.Sub(exp.WrittenAt).Hours() / 24)
	detail := fmt.Sprintf("recycles %d aged run(s)", exp.Recycles)
	if exp.Recycles == 0 {
		detail = "empty, ready to write"
	}
	return fmt.Sprintf("expects %q (labeled %s, %dd ago; %s) — or a fresh tape",
		exp.Label, record.DateString(exp.WrittenAt), age, detail)
}
