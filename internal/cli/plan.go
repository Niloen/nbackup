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

			plan, err := eng.PlanWithProgress(date, estimateProgress(a.quiet))
			if err != nil {
				return err
			}
			fmt.Printf("Plan for run %s  (cycle %dd, landing %q)\n\n",
				record.DateString(date), plan.Interval, strings.Join(eng.Landings(), ", "))
			for _, w := range plan.Warnings {
				fmt.Printf("WARNING: %s\n", w)
			}
			if len(plan.Warnings) > 0 {
				fmt.Println()
			}

			estTotal, unknownEst := fprintPlanItems(os.Stdout, plan)

			current := eng.StoredBytes()
			capacity := eng.Capacity()
			fmt.Printf("\nCatalog currently stored: %s\n", sizeutil.FormatBytes(current))
			fmt.Printf("This run (estimated): %s\n", runEstimateLine(estTotal, unknownEst))
			if capacity > 0 {
				_, pct := eng.CapacityStatus(current)
				fmt.Printf("Capacity: %s (%.1f%% used)\n", sizeutil.FormatBytes(capacity), pct)
				// Capacity is a promise: the dump makes room BEFORE writing, so the
				// plan shows what tonight costs in history — the archives the
				// pre-write reclaim will free per landing — or the refusal the dump
				// would fail loud with when retention alone exceeds the budget.
				for _, f := range eng.MakeRoomForecasts(plan, date) {
					switch {
					case f.Err != nil:
						fmt.Printf("WARNING: the dump would refuse: %v\n", f.Err)
					case f.Freed > 0:
						fmt.Printf("Make room: will reclaim ~%s from %d archive(s) on %q before dumping\n",
							sizeutil.FormatBytes(f.Freed), f.Archives, f.Medium)
					}
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
//
// Resolved units of one pattern source render as a GROUP: a header naming the base, one
// indented row per matched child, and — for a partition — the "the rest" row plus an
// explicit coverage line, so the two questions a reader has ("is anything dropped?" /
// "am I double-storing the base and its children?") are answered on sight. A selection
// group says "no rest" in its header — the visible cue that only the matches are covered.
func fprintPlanItems(w io.Writer, plan *planner.Plan) (estTotal int64, unknown int) {
	tw := newTab(w)
	fmt.Fprintln(tw, "DLE\tLEVEL\tEST. SIZE\tFULL SIZE\tREASON")
	row := func(label string, item planner.Item) {
		levelStr := fmt.Sprintf("L%d (full)", item.Level)
		// For an incremental, show the full-dump size alongside the chosen size so a
		// small incremental does not hide a large full waiting at the cycle deadline.
		// For a full the two are identical, so leave the column blank to avoid noise.
		fullStr := "-"
		estStr := "~" + sizeutil.FormatBytes(item.EstBytes)
		if item.Level >= 1 {
			levelStr = fmt.Sprintf("L%d (incr)", item.Level)
			fullStr = "~" + sizeutil.FormatBytes(item.FullBytes)
			// Some archivers (postgres) cannot cheaply size an incremental and report 0 —
			// "no estimate", not "nothing to store". Render that honestly as "unknown"
			// rather than "~0 B", which would read as an empty run and mislead a capacity
			// or cost decision (the FULL SIZE column is the number to plan against).
			if item.EstBytes == 0 {
				estStr = "unknown"
				unknown++
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", label, levelStr, estStr, fullStr, item.Reason)
		estTotal += item.EstBytes
	}
	items := plan.Items
	for i := 0; i < len(items); {
		it := items[i]
		if it.DLE.Base == "" { // a plain DLE: one row, exactly as before
			row(it.DLE.ID(), it)
			i++
			continue
		}
		// A pattern group: the consecutive units resolved from one source (Resolve emits
		// them contiguously, matches first, the rest last).
		j, hasRest := i, false
		for ; j < len(items) && items[j].DLE.Host == it.DLE.Host && items[j].DLE.Base == it.DLE.Base; j++ {
			if items[j].DLE.IsRest() {
				hasRest = true
			}
		}
		groupID := it.DLE.Host + ":" + it.DLE.Base
		if hasRest {
			fmt.Fprintf(tw, "%s — partitioned\t\t\t\t\n", groupID)
		} else {
			fmt.Fprintf(tw, "%s — selection (matches only, no rest)\t\t\t\t\n", groupID)
		}
		matched := 0
		for k := i; k < j; k++ {
			m := items[k]
			branch := "├─"
			if k == j-1 {
				branch = "└─"
			}
			label := "  " + branch + " " + strings.TrimPrefix(strings.TrimPrefix(m.DLE.Source, m.DLE.Base), "/")
			if m.DLE.IsRest() {
				label = "  " + branch + " the rest"
			} else {
				matched++
			}
			row(label, m)
		}
		if hasRest {
			fmt.Fprintf(tw, "  ✓ covers 100%% of %s (%d matched + the rest)\t\t\t\t\n", groupID, matched)
		}
		i = j
	}
	tw.Flush()
	return estTotal, unknown
}

// runEstimateLine renders the "This run (estimated)" summary, noting when some
// incrementals had no size estimate so the shown total reads as the floor it is
// rather than the whole run.
func runEstimateLine(estTotal int64, unknown int) string {
	s := "~" + sizeutil.FormatBytes(estTotal)
	if unknown > 0 {
		s += fmt.Sprintf(" + %d incremental(s) with no size estimate", unknown)
	}
	return s
}

// runPlanForecast renders an extended plan: one row per simulated daily run,
// projecting the level schedule forward. Estimates are sampled once and held
// constant (see engine.Simulate), so the per-day size tracks the chosen levels.
func runPlanForecast(eng *engine.Engine, start time.Time, days int) error {
	plans, err := eng.Simulate(start, days)
	if err != nil {
		return err
	}
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
	curve, err := eng.ForecastCost(start, days)
	if err != nil {
		return err
	}
	priced := eng.CostSummary(nil).Priced

	tw := newTab(os.Stdout)
	if priced {
		fmt.Fprintln(tw, "DATE\tFULL\tINCR\tEST. SIZE\t$/MONTH\tFULLS")
	} else {
		fmt.Fprintln(tw, "DATE\tFULL\tINCR\tEST. SIZE\tFULLS")
	}
	var windowTotal int64
	var totalIncrs, totalPromoted int
	for i, p := range plans {
		var fulls, incrs int
		var est int64
		var fullNames []string
		for _, it := range p.Items {
			est += it.EstBytes
			if it.Level == 0 {
				fulls++
				name := it.DLE.ID()
				if it.Promoted {
					name += "*"
					totalPromoted++
				}
				fullNames = append(fullNames, name)
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
	if totalPromoted > 0 {
		fmt.Println("\n* = a full promoted ahead of its cycle deadline to level the daily full load.")
	}
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
