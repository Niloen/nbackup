// Package planner decides, for each DLE, which backup level to run. It uses an
// Amanda-style multilevel scheme (levels 0-9) with a dynamic, estimate-driven
// schedule, but with only two user-facing inputs — the cycle and the medium's
// capacity — and no balancing knobs.
//
// Each run estimates every DLE, then assigns a level: a DLE that has never been
// fulled, or whose last full has reached the cycle deadline, gets a full; otherwise
// it gets an incremental. The cycle is a *hard* ceiling — a full never ages past it
// — so there is nothing to "demote": a full is either due or it is not.
//
// Incrementals follow Amanda's bump scheme (see chooseIncrLevel). A DLE sits at a
// level, re-dumping everything since the level below, and climbs to the next level
// only when it has held the current one for a few runs *and* climbing saves a real
// fraction of the full. So level 1 is the common case — a deeper level is earned by
// genuine savings, not reached automatically — which keeps restore chains short and
// consecutive incrementals overlapping for redundancy.
//
// The one balancing lever is promotion: pulling a future full forward onto today
// to level the daily full load across the cycle, so a pile-up of deadlines on one
// day is spread over the lighter runs before it. It works from a deadline calendar
// (each not-yet-due DLE sits on the day its full is due) and repeatedly relieves
// the heaviest future day by pulling one of its fulls onto today, as long as (a)
// today is lighter than that peak, (b) the move does not overshoot the day it
// relieves — today's resulting load may not exceed that day's load *after the moved
// full leaves it* — which is why a DLE that dominates its own deadline day is never
// promoted: relocating it would make today the new, equally heavy peak, so it waits
// for its own deadline rather than being re-fulled early to chase an average (and a
// tiny DLE merely sharing that deadline can no longer inflate the peak enough to
// unlock the move) — and (c) it fits the per-run room left before pruning would
// evict a protected slot. With no free capacity, promotion
// does nothing; with capacity to spare, it spends it to keep backups fresh and
// balanced — which is exactly what budgeting that capacity is for.
//
// Whether the cycle fits the medium *at all* is a separate, structural check:
// over a cycle every DLE is fulled once, and (with minimum_age >= cycle) those
// fulls coexist, so a complete recovery set must fit capacity. If it cannot, no
// scheduling can keep the medium recoverable — Build surfaces a warning rather
// than silently pruning the oldest recovery points away.
package planner

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// MaxLevel is the highest incremental level assigned (Amanda uses levels 0-9).
const MaxLevel = 9

// Estimate is the predicted size of a DLE's dumps at the levels the planner may
// choose between: a full, the level the DLE currently sits at, and the next level
// up. Incr and IncrNext let the planner weigh whether climbing a level saves
// enough to be worth it (see chooseIncrLevel). IncrNext is 0 when the next level
// is not yet dumpable (no base snapshot) — i.e. the DLE has not sat at the current
// level long enough to have produced one.
type Estimate struct {
	Full     int64 // level 0
	Incr     int64 // the current sitting level L
	IncrNext int64 // level L+1 (0 if not yet estimable)
}

// bumpDays is Amanda's redundancy guard: a DLE stays at one incremental level for
// at least this many runs before it may climb, so consecutive incrementals overlap
// and losing one does not break the restore chain.
const bumpDays = 2

// Params are the planner's tuning inputs, derived from config and the medium.
type Params struct {
	// CycleDays is the dump cycle: the hard maximum age of any full, and the
	// window across which fulls are balanced.
	CycleDays int
	// CapacityBytes is the medium's total retainable capacity, used for the
	// structural cycle check (can a complete recovery set — one full of every
	// DLE — be retained at all). Zero or negative means unbounded.
	CapacityBytes int64
	// RoomBytes is the per-run ceiling that bounds promotion: the most a single
	// run may write, the tighter of the pool's free room (capacity minus the
	// protected set — bytes pruning cannot reclaim) and the landing volume's
	// remaining room. Promotion never pushes a run past it, so promotion spends
	// only genuinely free space. Negative means unbounded.
	RoomBytes int64
	// BumpPercent is the minimum saving — as a percentage of the full-dump size —
	// an incremental must show before it climbs to the next level (Amanda's
	// bumppercent). Higher means levels climb more reluctantly, so level 1 stays
	// the common case and a deeper level is taken only when it is a real saving.
	BumpPercent float64
}

// Plan is the result of a planning run.
type Plan struct {
	Date     time.Time
	Interval int // the cycle in days
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
	estFull   int64
	estIncr   int64 // estimate of the chosen incremental level (set once it is decided)
	incrLevel int   // the chosen incremental level (1..MaxLevel)
	reason    string
}

// Build produces a plan for the given date from per-DLE estimates.
func Build(dles []config.DLE, hist *catalog.History, est map[string]Estimate, p Params, today time.Time) *Plan {
	cycle := p.CycleDays
	if cycle < 1 {
		cycle = 7
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
		case c.days >= cycle:
			c.full, c.mandatory = true, true
			c.reason = fmt.Sprintf("full due (cycle deadline reached: %dd >= %dd)", c.days, cycle)
		default:
			c.incrLevel, c.estIncr, c.reason = chooseIncrLevel(st, e, p.BumpPercent)
		}
		cands = append(cands, c)
	}

	promote(cands, cycle, p.RoomBytes)

	plan := &Plan{Date: today, Interval: cycle}
	// Structural cycle check (priority #1, recoverability): over a cycle every
	// DLE is fulled once, and with minimum_age >= cycle those fulls coexist on
	// the medium. If a single complete recovery set cannot fit capacity, no
	// scheduling can keep the medium recoverable — surface it rather than
	// silently pruning the oldest recovery points away.
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
			it.Level, it.BaseLevel, it.EstBytes = c.incrLevel, c.incrLevel-1, c.estIncr
			it.BaseSlot = c.st.SlotAtLevel(c.incrLevel - 1)
		}
		plan.Items = append(plan.Items, it)
	}
	return plan
}

// chooseIncrLevel decides the incremental level for a DLE that is not getting a
// full, returning the level, its estimated size, and a human reason.
//
// A DLE *sits* at a level: it repeats that level run after run, each time
// re-dumping everything changed since the level below — so consecutive
// incrementals overlap and stay independent of one another. It climbs to the next
// level only when both Amanda guards pass: it has sat at the current level for at
// least bumpDays runs (redundancy), and the next level would save at least
// BumpPercent of the full-dump size (a real saving). Because the saving from a
// climb shrinks as levels deepen, a percentage threshold naturally keeps level 1
// the common case and deeper levels rare. The first incremental after a full
// always starts at level 1.
func chooseIncrLevel(st *catalog.DLEState, e Estimate, bumpPercent float64) (level int, est int64, reason string) {
	last := st.LastLevel()
	if last < 1 {
		return 1, e.Incr, "incremental L1 (first since last full)"
	}
	if last < MaxLevel && e.IncrNext > 0 && st.RunsAtCurrentLevel() >= bumpDays {
		saving := e.Incr - e.IncrNext
		thresh := int64(float64(e.Full) * bumpPercent / 100)
		if saving >= thresh {
			return last + 1, e.IncrNext, fmt.Sprintf(
				"bumped to L%d (climbing saves ~%s, over the %.0f%%-of-full threshold)",
				last+1, sizeutil.FormatBytes(saving), bumpPercent)
		}
	}
	return last, e.Incr, fmt.Sprintf("incremental L%d (held; climbing would not save enough)", last)
}

// Simulate projects the planner forward over `days` consecutive daily runs from
// `start`, advancing a cloned history after each simulated run so each day's plan
// reflects the fulls and incrementals the runs before it would have produced (the
// next full lands by the cycle deadline, incrementals fill the days between, and
// promotion staggers same-day deadlines apart). It writes nothing — the caller's
// history is untouched.
//
// Estimates and params are sampled once and held constant across the window: this
// forecasts the *level schedule* from today's sizes, not capacity drift as slots
// accumulate. The per-day EstBytes therefore tracks the chosen levels, not a
// reclamation timeline. The bump decision likewise weighs today's level sizes, so
// a forecast past a simulated bump approximates the deeper level's size with the
// current one's — a schedule sketch, not an exact size projection.
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
		// next day's DaysSinceFull / LastLevel / RunsAtCurrentLevel see it.
		day := date.Format("2006-01-02")
		slotID := "slot-" + day
		for _, it := range plan.Items {
			h.RecordRun(it.Name, slotID, day, it.Level)
		}
	}
	return plans
}

// runBytes is the total estimated bytes the run currently writes.
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

// promote pulls future fulls forward onto today to level the daily full load
// across the cycle. It builds a deadline calendar — each not-yet-due DLE sits on
// the day (an offset from today) its full is due — adds up today's already-fixed
// load (the mandatory fulls), then repeatedly relieves the heaviest future day by
// pulling one of its fulls onto today.
//
// Two guards keep it from chasing an average. Each move must not overshoot the day
// it relieves — today's resulting load may not exceed that day's load after the
// moved full leaves it — so the move lowers the global peak rather than just
// relocating it. That means a DLE dominating its own deadline day is never promoted
// (moving it leaves an equal peak on today), and a tiny DLE merely sharing that
// deadline cannot inflate the peak enough to unlock the big move. And each move must
// fit the per-run room, so promotion spends only genuinely free capacity. When the heaviest day
// cannot be relieved (its fulls are too big to drop the peak or to fit room) it is
// set aside and the next-heaviest is tried; promotion stops once today is no longer
// lighter than any remaining peak.
func promote(cands []*cand, cycle int, room int64) {
	// Deadline calendar: an incremental candidate last fulled `days` ago is due in
	// `cycle-days` days (offset >= 1). byOffset groups the candidates due on each
	// day, load is their total full bytes, and todayLoad is today's fixed load.
	// Never-fulled DLEs are mandatory fulls, not candidates. A DLE fulled today
	// (days == 0) is excluded entirely: its full already exists at today's date, so
	// pulling it "forward" would only re-full it the same day — no stagger, pure
	// waste — and it would recur on every same-day run. Staggering only buys
	// anything across distinct days, so such a DLE waits until it is at least a day
	// old before it can relieve a future peak.
	byOffset := map[int][]*cand{}
	load := map[int]int64{}
	var todayLoad int64
	for _, c := range cands {
		switch {
		case c.full:
			todayLoad += c.estFull
		case c.days > 0:
			off := cycle - c.days
			if off < 1 {
				off = 1
			}
			byOffset[off] = append(byOffset[off], c)
			load[off] += c.estFull
		}
	}

	total := runBytes(cands)
	for {
		// Find the heaviest future day. If today already carries at least as much, no
		// move can lower the peak, so promotion is done.
		peakOff, peakLoad := 0, int64(0)
		for off, l := range load {
			if l > peakLoad {
				peakOff, peakLoad = off, l
			}
		}
		if peakOff == 0 || todayLoad >= peakLoad {
			return
		}
		// Pull the largest full off the peak day that still leaves today below it (so
		// the peak strictly drops) and fits the per-run room.
		var pick *cand
		var pickIdx int
		for i, c := range byOffset[peakOff] {
			// The move must not overshoot the day it relieves: today's resulting load
			// may not exceed the peak day's load *after this full leaves it*. Comparing
			// against the day's remaining load (peakLoad-c.estFull), not its total, is
			// what keeps a DLE that dominates its own deadline day from being relocated
			// for a near-zero gain just because a tiny co-deadline DLE inflated the peak
			// above it — the case that otherwise re-fulls a big DLE almost every run.
			if todayLoad+c.estFull > peakLoad-c.estFull {
				continue
			}
			if room >= 0 && total-c.estIncr+c.estFull > room {
				continue
			}
			if pick == nil || c.estFull > pick.estFull {
				pick, pickIdx = c, i
			}
		}
		if pick == nil {
			// This peak can't be relieved usefully; set it aside and try the next.
			delete(load, peakOff)
			delete(byOffset, peakOff)
			continue
		}
		pick.full = true
		pick.reason = fmt.Sprintf("promoted full (filling a light run; %dd into a %dd cycle)", pick.days, cycle)
		todayLoad += pick.estFull
		total += pick.estFull - pick.estIncr
		load[peakOff] -= pick.estFull
		byOffset[peakOff] = append(byOffset[peakOff][:pickIdx], byOffset[peakOff][pickIdx+1:]...)
	}
}
