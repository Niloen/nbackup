// Package planner decides, for each DLE, which backup level to run. It uses an
// Amanda-style multilevel scheme (levels 0-9): a full (level 0) starts each
// cycle, and each subsequent run increments the level so it captures only what
// changed since the previous level, keeping daily volume small. Fulls are
// staggered across DLEs to avoid spikes. Priority order follows the PRD:
// recoverability and cycle safety first, then budget, then balancing volume.
package planner

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/state"
)

// Plan is the result of a planning run.
type Plan struct {
	Date     time.Time
	Interval int
	Items    []Item
}

// Item is the planned backup of a single DLE.
type Item struct {
	Source    config.Source
	Name      string
	Level     int    // 0 = full, 1..9 = incremental
	BaseLevel int    // level whose snapshot this builds on (-1 for a full)
	Reason    string // human-readable explanation
	BaseSlot  string // slot whose state an incremental builds on
}

// Build produces a plan for the given date without scanning sources.
func Build(cfg *config.Config, st *state.State, today time.Time) *Plan {
	interval := cfg.FullIntervalDays()
	p := &Plan{Date: today, Interval: interval}
	for _, src := range cfg.Sources {
		name := src.Name()
		d := st.DLE(name)
		level, reason := decide(name, d, today, interval)
		item := Item{Source: src, Name: name, Level: level, BaseLevel: -1, Reason: reason}
		if level >= 1 {
			item.BaseLevel = level - 1
			if n := len(d.Runs); n > 0 {
				item.BaseSlot = d.Runs[n-1].Slot
			}
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
//     capped at the maximum level), capturing changes since that level.
func decide(name string, d *state.DLEState, today time.Time, interval int) (int, string) {
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
	if level > config.MaxLevel {
		level = config.MaxLevel
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
