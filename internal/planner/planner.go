// Package planner decides, for each DLE, which backup level to run. It uses an
// Amanda-style multilevel scheme (levels 0-9): a full (level 0) starts each
// cycle, and each subsequent run increments the level so it captures only what
// changed since the previous level, keeping daily volume small. Fulls are
// staggered across DLEs to avoid spikes. It is pure, operating over the catalog
// history.
package planner

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/dle"
)

// MaxLevel is the highest incremental level assigned (Amanda uses levels 0-9).
const MaxLevel = 9

// Plan is the result of a planning run.
type Plan struct {
	Date     time.Time
	Interval int
	Items    []Item
}

// Item is the planned backup of a single DLE.
type Item struct {
	DLE       dle.DLE
	Name      string
	Level     int    // 0 = full, 1..9 = incremental
	BaseLevel int    // level whose snapshot this builds on (-1 for a full)
	Reason    string // human-readable explanation
	BaseSlot  string // slot whose state an incremental builds on
}

// Build produces a plan for the given date without scanning sources.
func Build(dles []dle.DLE, hist *catalog.History, interval int, today time.Time) *Plan {
	p := &Plan{Date: today, Interval: interval}
	for _, d := range dles {
		name := d.Name()
		st := hist.DLE(name)
		level, reason := decide(name, st, today, interval)
		item := Item{DLE: d, Name: name, Level: level, BaseLevel: -1, Reason: reason}
		if level >= 1 {
			item.BaseLevel = level - 1
			item.BaseSlot = st.LastSlot()
		}
		p.Items = append(p.Items, item)
	}
	return p
}

// decide chooses a backup level for one DLE.
//
// Rules, in order:
//   - No full ever -> full (required before any incremental).
//   - Full due (>= interval) on this DLE's staggered day -> full; an overdue
//     full (>= 2x interval) is forced regardless of staggering.
//   - Otherwise -> the next incremental level (one higher than the last run,
//     capped at MaxLevel), capturing changes since that level.
func decide(name string, d *catalog.DLEState, today time.Time, interval int) (int, string) {
	days := d.DaysSinceFull(today)
	if days < 0 {
		return 0, "first backup of this DLE (no full exists yet)"
	}
	if days >= interval {
		phase := int(hashName(name) % uint32(interval))
		if epochDay(today)%interval == phase {
			return 0, fmt.Sprintf("scheduled full (staggered day, last full %dd ago)", days)
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
