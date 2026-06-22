// Package planner decides, for each DLE, which backup level to run. It uses an
// Amanda-style multilevel scheme (levels 0-9): a full (level 0) starts each
// cycle, and each subsequent run increments the level so it captures only what
// changed since the previous level. Fulls are balanced across the cycle by size
// (bin-packed into the interval's days) so daily volume is smooth — a global,
// temporal concern, independent of which medium the slots land on.
package planner

import (
	"fmt"
	"hash/fnv"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
)

// MaxLevel is the highest incremental level assigned (Amanda uses levels 0-9).
const MaxLevel = 9

// Params are the planner's tuning inputs.
type Params struct {
	// FullIntervalDays is the cycle length: the target days between fulls per
	// DLE. Fulls are spread across this many days to balance daily volume.
	FullIntervalDays int
}

// Plan is the result of a planning run.
type Plan struct {
	Date     time.Time
	Interval int
	Items    []Item
}

// Item is the planned backup of a single DLE.
type Item struct {
	DLE       config.DLE
	Name      string
	Level     int    // 0 = full, 1..9 = incremental
	BaseLevel int    // level whose snapshot this builds on (-1 for a full)
	Reason    string // human-readable explanation
	BaseSlot  string // slot whose state an incremental builds on
}

// Build produces a plan for the given date without scanning sources.
func Build(dles []config.DLE, hist *catalog.History, p Params, today time.Time) *Plan {
	interval, fullDay := schedule(dles, hist, p)
	plan := &Plan{Date: today, Interval: interval}
	for _, d := range dles {
		name := d.Name()
		st := hist.DLE(name)
		level, reason := decide(st, today, interval, fullDay[name])
		item := Item{DLE: d, Name: name, Level: level, BaseLevel: -1, Reason: reason}
		if level >= 1 {
			item.BaseLevel = level - 1
			item.BaseSlot = st.LastSlot()
		}
		plan.Items = append(plan.Items, item)
	}
	return plan
}

// schedule returns the full interval (the global cycle length) and each DLE's
// assigned day of the cycle [0,interval). DLEs are bin-packed across the cycle's
// days by last-full size (largest first into the lightest day) so each day's
// full volume is balanced. Before any full-size history exists it falls back to
// hash-based staggering.
func schedule(dles []config.DLE, hist *catalog.History, p Params) (int, map[string]int) {
	interval := p.FullIntervalDays
	if interval < 1 {
		interval = 7
	}

	type sized struct {
		name  string
		bytes int64
	}
	items := make([]sized, 0, len(dles))
	var totalFull int64
	for _, d := range dles {
		b := hist.DLE(d.Name()).LastFullBytes
		totalFull += b
		items = append(items, sized{d.Name(), b})
	}

	fullDay := map[string]int{}
	if totalFull == 0 {
		// No size history yet: stagger by hash.
		for _, it := range items {
			fullDay[it.name] = int(hashName(it.name) % uint32(interval))
		}
		return interval, fullDay
	}

	// Greedy bin-pack DLEs (largest full first) into `interval` day-bins.
	sort.Slice(items, func(i, j int) bool { return items[i].bytes > items[j].bytes })
	bins := make([]int64, interval)
	for _, it := range items {
		lightest := 0
		for b := 1; b < interval; b++ {
			if bins[b] < bins[lightest] {
				lightest = b
			}
		}
		fullDay[it.name] = lightest
		bins[lightest] += it.bytes
	}
	return interval, fullDay
}

// decide chooses a backup level for one DLE.
//
//   - No full ever -> full (required before any incremental).
//   - Full due (>= interval) on this DLE's assigned day -> full; an overdue full
//     (>= 2x interval) is forced regardless.
//   - Otherwise -> the next incremental level (one higher than the last run,
//     capped at MaxLevel), capturing changes since that level.
func decide(d *catalog.DLEState, today time.Time, interval, fullDay int) (int, string) {
	days := d.DaysSinceFull(today)
	if days < 0 {
		return 0, "first backup of this DLE (no full exists yet)"
	}
	if days >= interval {
		if epochDay(today)%interval == fullDay {
			return 0, fmt.Sprintf("scheduled full (balanced day, last full %dd ago)", days)
		}
		if days >= 2*interval {
			return 0, fmt.Sprintf("forced full (overdue: %dd >= %dd)", days, 2*interval)
		}
	}
	level := d.IncrementalsSinceFull() + 1
	if level > MaxLevel {
		level = MaxLevel
	}
	return level, fmt.Sprintf("incremental L%d (changes since L%d, last full %dd ago)", level, level-1, days)
}

func hashName(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func epochDay(t time.Time) int {
	return int(t.UTC().Truncate(24*time.Hour).Unix() / 86400)
}
