// Package planner decides, for each DLE, which backup level to run. It uses an
// Amanda-style multilevel scheme (levels 0-9): a full (level 0) starts each
// cycle, and each subsequent run increments the level so it captures only what
// changed since the previous level. Fulls are balanced across the cycle so each
// run's volume stays near the medium's preferred run size, avoiding spikes. It
// is pure: it works over the catalog history and plain byte targets, with no
// knowledge of media types.
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

// Params are the planner's tuning inputs, derived from the landing medium.
type Params struct {
	// PreferredRunBytes is the target full volume per run. When >0 and full
	// sizes are known, it derives the full interval and balances fulls by size.
	PreferredRunBytes int64
	// FullIntervalDays is the fallback interval when PreferredRunBytes is 0 or
	// no full-size history exists yet.
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

// schedule returns the effective full interval and each DLE's assigned day of
// the cycle [0,interval). When a preferred run size and full-size history are
// available, the interval is derived (total full bytes / preferred) and DLEs are
// bin-packed across the cycle days by size so each day's full volume is balanced.
// Otherwise it falls back to a fixed interval with hash-based staggering.
func schedule(dles []config.DLE, hist *catalog.History, p Params) (int, map[string]int) {
	fallback := p.FullIntervalDays
	if fallback < 1 {
		fallback = 7
	}

	var totalFull int64
	for _, d := range dles {
		totalFull += hist.DLE(d.Name()).LastFullBytes
	}

	if p.PreferredRunBytes <= 0 || totalFull == 0 {
		fullDay := map[string]int{}
		for _, d := range dles {
			fullDay[d.Name()] = int(hashName(d.Name()) % uint32(fallback))
		}
		return fallback, fullDay
	}

	interval := int((totalFull + p.PreferredRunBytes - 1) / p.PreferredRunBytes)
	if interval < 1 {
		interval = 1
	}

	// Greedy bin-pack DLEs (largest full first) into `interval` day-bins.
	type sized struct {
		name  string
		bytes int64
	}
	items := make([]sized, 0, len(dles))
	for _, d := range dles {
		items = append(items, sized{d.Name(), hist.DLE(d.Name()).LastFullBytes})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].bytes > items[j].bytes })

	bins := make([]int64, interval)
	fullDay := map[string]int{}
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
