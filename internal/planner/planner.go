// Package planner decides, for each DLE, which backup level to run. It uses an
// Amanda-style multilevel scheme (levels 0-9) with a dynamic, estimate-driven
// schedule: each run estimates every DLE, then balances by degrading (demoting
// over-budget/over-target fulls to incrementals) and optionally promoting
// (pulling future fulls forward to fill light runs). The PRD priority order is
// encoded directly: mandatory fulls (recoverability, cycle deadline) are
// immovable, the per-run capacity ceiling overrides the balance target, and
// promotion is off by default so balancing never spends extra storage.
//
// Capacity is enforced at two scopes. Per run, degrade keeps a single run's peak
// under the room left before pruning would evict a protected slot. Per cycle,
// Build checks that a complete recovery set (one full of every DLE) fits the
// medium at all; that is structural — degrading only defers a due full to its
// deadline within the same cycle, never reducing the cycle's full demand — so it
// is surfaced as a warning rather than silently scheduled around.
package planner

import (
	"fmt"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// MaxLevel is the highest incremental level assigned (Amanda uses levels 0-9).
const MaxLevel = 9

// Estimate is the predicted size of a DLE's full and next-incremental dumps.
type Estimate struct {
	Full int64
	Incr int64
}

// Params are the planner's tuning inputs, derived from config and the medium.
type Params struct {
	// FullIntervalDays is the cycle length: the target days between fulls.
	FullIntervalDays int
	// CapacityBytes is the medium's total retainable capacity, used for the
	// structural cycle check (can a complete recovery set — one full of every
	// DLE — be retained at all). Zero or negative means unbounded.
	CapacityBytes int64
	// CapacityRoomBytes is the hard per-run ceiling: the most a single run may
	// write, the tighter of the pool's free room (capacity minus the protected
	// set — bytes pruning cannot reclaim) and the landing volume's remaining room
	// (a run fills the reel it appends to before spilling to the next). It bounds a
	// single run's peak; it does not, and cannot, bound the cycle total — degrading
	// a due full only defers it to its deadline within the same cycle, so the
	// cycle's full demand is governed by CapacityBytes above, not by this per-run
	// ceiling. Negative means unbounded.
	CapacityRoomBytes int64
	// Promote enables pulling future fulls forward to fill light runs.
	Promote bool
	// PromoteCeilingBytes bounds promotion so it never spends storage past this
	// headroom. Negative means unbounded.
	PromoteCeilingBytes int64
}

// Plan is the result of a planning run.
type Plan struct {
	Date     time.Time
	Interval int
	Target   int64 // balanced full-bytes target per run
	Items    []Item
	Warnings []string // structural problems no scheduling can fix (e.g. cycle won't fit capacity)
}

// Item is the planned backup of a single DLE.
type Item struct {
	DLE       config.DLE
	Name      string
	Level     int    // 0 = full, 1..9 = incremental
	BaseLevel int    // level whose snapshot an incremental builds on (-1 for a full)
	EstBytes  int64  // estimated size of the chosen dump
	Reason    string // human-readable explanation
	BaseSlot  string // slot whose state an incremental builds on
}

type cand struct {
	dle       config.DLE
	name      string
	st        *catalog.DLEState
	days      int // days since last full, -1 if never
	full      bool
	mandatory bool
	due       bool
	estFull   int64
	estIncr   int64
	reason    string
}

// Build produces a plan for the given date from per-DLE estimates.
func Build(dles []config.DLE, hist *catalog.History, est map[string]Estimate, p Params, today time.Time) *Plan {
	interval := p.FullIntervalDays
	if interval < 1 {
		interval = 7
	}

	cands := make([]*cand, 0, len(dles))
	var totalFull int64
	for _, d := range dles {
		name := d.Name()
		st := hist.DLE(name)
		e := est[name]
		totalFull += e.Full
		c := &cand{dle: d, name: name, st: st, days: st.DaysSinceFull(today), estFull: e.Full, estIncr: e.Incr}
		switch {
		case c.days < 0:
			c.full, c.mandatory = true, true
			c.reason = "first backup of this DLE (mandatory full)"
		case c.days >= 2*interval:
			c.full, c.mandatory = true, true
			c.reason = fmt.Sprintf("forced full (deadline reached: %dd >= %dd)", c.days, 2*interval)
		case c.days >= interval:
			c.full, c.due = true, true
			c.reason = fmt.Sprintf("full due (%dd since last full)", c.days)
		default:
			c.reason = fmt.Sprintf("incremental (%dd since last full)", c.days)
		}
		cands = append(cands, c)
	}

	target := totalFull / int64(interval)
	degrade(cands, target, p.CapacityRoomBytes)
	if p.Promote {
		promote(cands, target, p.PromoteCeilingBytes)
	}

	plan := &Plan{Date: today, Interval: interval, Target: target}
	// Structural cycle check (priority #1, recoverability): over a cycle every
	// DLE is fulled once, and with minimum_age >= cycle those fulls coexist on
	// the medium. If a single complete recovery set cannot fit capacity, no
	// scheduling can keep the medium recoverable — surface it rather than
	// silently pruning the oldest recovery points away. This is the cycle-scope
	// counterpart to the per-run CapacityRoomBytes ceiling enforced in degrade.
	if p.CapacityBytes > 0 && totalFull > p.CapacityBytes {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"capacity too small to retain a complete recovery set: one full of every DLE needs ~%s but capacity is %s; the oldest recovery points will be pruned and full recoverability cannot be guaranteed",
			sizeutil.FormatBytes(totalFull), sizeutil.FormatBytes(p.CapacityBytes)))
	}
	for _, c := range cands {
		it := Item{DLE: c.dle, Name: c.name, BaseLevel: -1, Reason: c.reason}
		if c.full {
			it.Level, it.EstBytes = 0, c.estFull
		} else {
			lvl := c.st.IncrementalsSinceFull() + 1
			if lvl > MaxLevel {
				lvl = MaxLevel
			}
			it.Level, it.BaseLevel, it.EstBytes, it.BaseSlot = lvl, lvl-1, c.estIncr, c.st.LastSlot()
		}
		plan.Items = append(plan.Items, it)
	}
	return plan
}

// Simulate projects the planner forward over `days` consecutive daily runs from
// `start`, advancing a cloned history after each simulated run so each day's plan
// reflects the fulls and incrementals the runs before it would have produced (the
// next full lands at the cycle deadline, incrementals climb in between). It writes
// nothing — the caller's history is untouched.
//
// Estimates and params are sampled once and held constant across the window: this
// forecasts the *level schedule* from today's sizes, not capacity drift as slots
// accumulate. The per-day EstBytes therefore tracks the chosen levels, not a
// reclamation timeline.
func Simulate(dles []config.DLE, hist *catalog.History, est map[string]Estimate, p Params, start time.Time, days int) []*Plan {
	if days < 1 {
		days = 1
	}
	h := hist.Clone()
	plans := make([]*Plan, 0, days)
	for i := 0; i < days; i++ {
		date := start.AddDate(0, 0, i)
		plan := Build(dles, h, est, p, date)
		plans = append(plans, plan)
		// Advance the cloned history as if this day's run had been sealed, so the
		// next day's DaysSinceFull / IncrementalsSinceFull see it.
		day := date.Format("2006-01-02")
		slotID := "slot-" + day
		for _, it := range plan.Items {
			h.RecordRun(it.Name, slotID, day, it.Level)
		}
	}
	return plans
}

func runBytes(cands []*cand) int64 {
	var t int64
	for _, c := range cands {
		if c.full {
			t += c.estFull
		} else {
			t += c.estIncr
		}
	}
	return t
}

func fullBytes(cands []*cand) int64 {
	var t int64
	for _, c := range cands {
		if c.full {
			t += c.estFull
		}
	}
	return t
}

// degrade demotes the least-urgent non-mandatory due-fulls to incrementals while
// the run exceeds the per-run capacity room (hard, priority #3) or the balance
// target (soft, priority #4). Mandatory fulls are never touched, so a single big
// DLE on its day may still exceed the ceiling — that is accepted.
//
// Soft-target demotions never empty a run of its fulls. A single full that already
// exceeds the balance target is irreducible — fulls are atomic, so demoting the
// last one cannot bring the run closer to target; it only defers that full to its
// hard deadline. Two DLEs whose last fulls coincide would then defer in lock-step
// and pile onto the same deadline day, the opposite of balancing. Keeping the
// most-urgent full in place drains the cycle one full per run and naturally
// staggers such DLEs apart. The hard capacity ceiling still overrides and may
// demote down to zero fulls — capacity beats balance.
//
// This only smooths a single run's peak. It cannot reduce the cycle's full
// demand: a demoted due-full still climbs to its deadline and is forced full
// within the same cycle. Whether the cycle as a whole fits the medium is a
// structural property checked separately against CapacityBytes (see Build).
func degrade(cands []*cand, target, room int64) {
	var candidates []*cand
	for _, c := range cands {
		if c.full && !c.mandatory && c.due {
			candidates = append(candidates, c)
		}
	}
	// Least urgent first: smallest days since full, then largest size.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].days != candidates[j].days {
			return candidates[i].days < candidates[j].days
		}
		return candidates[i].estFull > candidates[j].estFull
	})
	overHard := func() bool { return room >= 0 && runBytes(cands) > room }
	overSoft := func() bool { return target > 0 && fullBytes(cands) > target }
	fullsLeft := func() int {
		n := 0
		for _, c := range cands {
			if c.full {
				n++
			}
		}
		return n
	}
	for _, c := range candidates {
		hard := overHard()
		if !hard && !overSoft() {
			break
		}
		// Under soft-only pressure, stop before emptying the run of fulls: the last
		// remaining full is irreducible and demoting it would only defer to a deadline
		// pile-up. The hard ceiling has no such floor.
		if !hard && fullsLeft() <= 1 {
			break
		}
		c.full, c.due = false, false
		c.reason = "degraded to incremental (over capacity/balance target)"
	}
}

// promote pulls soonest-due mid-cycle DLEs forward to full early, filling a light
// run toward the target. It is bounded by once-per-interval (only mid-cycle DLEs,
// never re-fulling a current one) and by the capacity headroom, so it never
// spends storage past the ceiling.
func promote(cands []*cand, target, ceiling int64) {
	if target <= 0 {
		return
	}
	var future []*cand
	for _, c := range cands {
		if !c.full && c.days >= 0 {
			future = append(future, c)
		}
	}
	// Soonest due first: largest days since full.
	sort.Slice(future, func(i, j int) bool { return future[i].days > future[j].days })
	for _, c := range future {
		if fullBytes(cands) >= target {
			break
		}
		projected := runBytes(cands) - c.estIncr + c.estFull
		if ceiling >= 0 && projected > ceiling {
			continue
		}
		c.full = true
		c.reason = fmt.Sprintf("promoted full (was %dd into cycle)", c.days)
	}
}
