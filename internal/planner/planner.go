// Package planner decides, for each DLE, which backup level to run, balancing
// full backups across days and reporting budget status. It implements the PRD's
// objective ordering: recoverability and cycle safety first, then budget, then
// balancing daily volume.
package planner

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"time"

	"github.com/Niloen/nbackup/internal/archive"
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
	Source   config.Source
	Name     string
	Level    int    // 0 = full, 1 = incremental
	Reason   string // human-readable explanation
	BaseSlot string // base L0 slot for incrementals
}

// Build produces a plan for the given date without scanning sources.
func Build(cfg *config.Config, st *state.State, today time.Time) *Plan {
	interval := cfg.FullIntervalDays()
	p := &Plan{Date: today, Interval: interval}
	for _, src := range cfg.Sources {
		name := src.Name()
		d := st.DLE(name)
		level, reason := decide(name, d, today, interval)
		item := Item{Source: src, Name: name, Level: level, Reason: reason}
		if level >= 1 {
			item.BaseSlot = d.LastFullSlot
		}
		p.Items = append(p.Items, item)
	}
	return p
}

// decide chooses a backup level for one DLE.
//
// Rules, in order:
//   - No full ever -> full (a full is required before any incremental).
//   - Full younger than the interval -> incremental.
//   - Full at/over the interval -> due for a full, but staggered: it runs on
//     this DLE's assigned day-of-cycle so fulls spread across days. An overdue
//     full (>= 2x interval) is forced regardless of staggering.
func decide(name string, d *state.DLEState, today time.Time, interval int) (int, string) {
	days := d.DaysSinceFull(today)
	if days < 0 {
		return 0, "first backup of this DLE (no full exists yet)"
	}
	if days < interval {
		return 1, fmt.Sprintf("incremental (last full %dd ago, interval %dd)", days, interval)
	}
	phase := int(hashName(name) % uint32(interval))
	if epochDay(today)%interval == phase {
		return 0, fmt.Sprintf("scheduled full (staggered day, last full %dd ago)", days)
	}
	if days >= 2*interval {
		return 0, fmt.Sprintf("forced full (overdue: %dd >= %dd)", days, 2*interval)
	}
	return 1, fmt.Sprintf("incremental (full due in %dd on staggered day)", daysUntilPhase(today, interval, phase))
}

func hashName(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func epochDay(t time.Time) int {
	return int(t.UTC().Truncate(24*time.Hour).Unix() / 86400)
}

func daysUntilPhase(today time.Time, interval, phase int) int {
	cur := epochDay(today) % interval
	diff := (phase - cur + interval) % interval
	if diff == 0 {
		return interval
	}
	return diff
}

// Estimate walks a DLE's source to estimate the uncompressed bytes the planned
// item would archive. For incrementals it counts only changed/new files
// relative to the base snapshot. It returns 0 with a nil error if the source is
// unreadable (e.g. a remote host not mounted locally).
func Estimate(item Item, base archive.Snapshot) (int64, error) {
	var total int64
	incremental := item.Level >= 1 && base != nil
	err := filepath.Walk(item.Source.Path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if incremental {
			rel, rerr := filepath.Rel(item.Source.Path, p)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			prev, ok := base[rel]
			if ok && info.Size() == prev.Size && !info.ModTime().After(prev.ModTime) {
				return nil
			}
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
