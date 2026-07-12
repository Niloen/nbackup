// Package planner decides, for each DLE, which backup level to run. It uses a
// multilevel scheme (levels 0-9) with a dynamic, estimate-driven
// schedule, but with only two user-facing inputs — the cycle and the medium's
// capacity — and no balancing knobs.
//
// Each run estimates every DLE, then assigns a level: a DLE that has never been
// fulled, or whose last full has reached the cycle deadline, gets a full; otherwise
// it gets an incremental. The cycle is a *hard* ceiling — a full never ages past it
// — so there is nothing to "demote": a full is either due or it is not.
//
// Incrementals follow a bump scheme (see chooseIncrLevel). A DLE sits at a
// level, re-dumping everything since the level below, and climbs to the next level
// only when it has held the current one for a few runs *and* climbing saves a real
// fraction of the full. So level 1 is the common case — a deeper level is earned by
// genuine savings, not reached automatically — which keeps restore chains short and
// consecutive incrementals overlapping and independent of one another.
//
// The one balancing lever is promotion: pulling a future full forward onto today to
// flatten the daily full-load calendar. Each run takes the move that most improves
// the calendar — lower the peak day first, then lower the variance of the daily load
// — so a clump of DLEs sharing a deadline is spread across distinct days rather than
// piled onto one night. A no-overshoot guard keeps it honest: a full is pulled
// forward only to relieve a genuine peak, never merely to relocate one, so a lone
// DLE is never re-fulled early and leveling a clump is a phase-shift, not extra
// fulls. Every move is bounded by the per-run room; the metric and the guards are
// documented on promote.
//
// Whether the cycle fits the medium *at all* is a separate, structural check:
// over a cycle every DLE is fulled once, and (with minimum_age >= cycle) those
// fulls coexist, so a complete recovery set must fit capacity. If it cannot, no
// scheduling can keep the medium recoverable — Build surfaces a warning rather
// than silently pruning the oldest recovery points away.
package planner

import (
	"fmt"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// MaxLevel is the highest incremental level assigned.
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
	// Incomplete is set when the archiver could only partially measure the source
	// (e.g. tar hit an unreadable file), so Full is a floor, not an exact size.
	// Build surfaces it as a warning so capacity planning isn't silently undercounted.
	Incomplete bool
}

// bumpDays is the climb hysteresis: a DLE stays at one incremental level for
// at least this many runs before it may climb, so the climb decision rests on a
// level's demonstrated steady state rather than a single run's estimate.
// (Redundancy is not this guard's job — that is copies to a second medium,
// which protect the full and incrementals alike.)
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
	// an incremental must show before it climbs to the next level. Higher means
	// levels climb more reluctantly, so level 1 stays the common case and a
	// deeper level is taken only when it is a real saving.
	BumpPercent float64
}

// Plan is the result of a planning run.
type Plan struct {
	Date     time.Time
	Interval int          // the cycle in days
	Items    []Item       // the units this run will dump
	Failed   []FailedUnit // units that failed BEFORE dumping (unresolvable source, dead estimate, unreachable host) — reported like dump failures, run exits non-zero
	Warnings []string     // structural problems no scheduling can fix (e.g. cycle won't fit capacity)
}

// Item is the planned backup of a single DLE.
type Item struct {
	DLE       DLE
	Name      string
	Level     int    // 0 = full, 1..9 = incremental
	BaseLevel int    // level whose snapshot an incremental builds on (-1 for a full)
	EstBytes  int64  // estimated size of the chosen dump
	FullBytes int64  // estimated size of a full dump (the cycle-deadline cost), shown so a small incremental does not hide a large pending full
	Reason    string // human-readable explanation
	Promoted  bool   // a full pulled forward by promotion (not due today), so reports can say why tonight was big
	BaseRun   string // run whose state an incremental builds on
}

type cand struct {
	dle       DLE
	name      string
	st        *catalog.DLEState
	days      int // days since last full, -1 if never
	full      bool
	mandatory bool
	promoted  bool
	estFull   int64
	estIncr   int64 // estimate of the chosen incremental level (set once it is decided)
	incrLevel int   // the chosen incremental level (1..MaxLevel)
	reason    string
}

// Build produces a plan for the given date from per-DLE estimates. forced names DLEs an
// operator has asked to full on the next run (`nb reset`); each is scheduled a mandatory
// level 0, overriding the cycle/bump schedule — the archiver-independent peer of Amanda's
// FORCE_FULL, decided here rather than by deleting the archiver's incremental state.
func Build(dles []DLE, hist *catalog.History, est map[string]Estimate, forced map[string]bool, p Params, today time.Time) *Plan {
	cycle := p.CycleDays
	if cycle < 1 {
		cycle = 7
	}

	cands := make([]*cand, 0, len(dles))
	var totalFull int64
	var estWarnings []string
	for _, d := range dles {
		name := d.Name()
		st := hist.DLE(name)
		e := est[name]
		totalFull += e.Full
		if e.Incomplete {
			estWarnings = append(estWarnings, fmt.Sprintf(
				"DLE %s: source is not fully readable, so its size estimate (~%s) is a floor — capacity planning may undercount (run as a user that can read every file, or exclude the unreadable paths)",
				d.ID(), sizeutil.FormatBytes(e.Full)))
		}
		c := &cand{dle: d, name: name, st: st, days: st.DaysSinceFull(today), estFull: e.Full, estIncr: e.Incr}
		switch {
		case forced[name]:
			c.full, c.mandatory = true, true
			c.reason = "forced full (nb reset)"
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
	plan.Warnings = append(plan.Warnings, estWarnings...)
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
		it := Item{DLE: c.dle, Name: c.name, BaseLevel: -1, Reason: c.reason, FullBytes: c.estFull, Promoted: c.promoted}
		if c.full {
			it.Level, it.EstBytes = 0, c.estFull
		} else {
			it.Level, it.BaseLevel, it.EstBytes = c.incrLevel, c.incrLevel-1, c.estIncr
			it.BaseRun = c.st.RunAtLevel(c.incrLevel - 1)
		}
		plan.Items = append(plan.Items, it)
	}
	return plan
}

// SittingLevel returns the incremental level a DLE currently sits at: its last
// dumped level, floored at 1 (right after a full the next incremental is L1) and
// clamped to MaxLevel. This is the one rule for "what level would this DLE dump
// next, absent a bump" — the scheduler estimates at this level so its sizes match
// the level chooseIncrLevel holds or climbs from.
func SittingLevel(st *catalog.DLEState) int {
	lvl := st.LastLevel()
	if lvl < 1 {
		lvl = 1
	}
	if lvl > MaxLevel {
		lvl = MaxLevel
	}
	return lvl
}

// chooseIncrLevel decides the incremental level for a DLE that is not getting a
// full, returning the level, its estimated size, and a human reason.
//
// A DLE *sits* at a level: it repeats that level run after run, each time
// re-dumping everything changed since the level below — so consecutive
// incrementals overlap and stay independent of one another (losing one costs at
// most its own restore point, never the runs after it). It climbs to the next
// level only when both guards pass: it has sat at the current level for at
// least bumpDays runs (hysteresis), and the next level would save at least
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
// Params are sampled once and held constant; estimates come from estAt, evaluated per
// simulated day. A caller that projects sizes forward (the offline history estimator)
// makes each day's fulls/incrementals reflect dataset growth to that day; a caller with
// no growth model returns the same map every day, and this is a pure level-schedule
// sketch — the next full by the cycle deadline, incrementals between — from fixed sizes.
// The bump decision weighs whichever day's sizes estAt returns.
func SimulateFunc(dles []DLE, hist *catalog.History, estAt func(time.Time) map[string]Estimate, forced map[string]bool, p Params, start time.Time, days int) []*Plan {
	if days < 1 {
		days = 1
	}
	h := hist.Clone()
	plans := make([]*Plan, 0, days)
	for i := 0; i < days; i++ {
		date := start.AddDate(0, 0, i)
		// A forced full is consumed on the first simulated day; later days follow the
		// ordinary schedule the day-0 full reseeds.
		dayForced := forced
		if i > 0 {
			dayForced = nil
		}
		plan := Build(dles, h, estAt(date), dayForced, p, date)
		plans = append(plans, plan)
		// Advance the cloned history as if this day's run had been sealed, so the
		// next day's DaysSinceFull / LastLevel / RunsAtCurrentLevel see it.
		day := date.Format("2006-01-02")
		runID := record.IDFromTime(date) // simulated id; mirrors the real run's shape
		for _, it := range plan.Items {
			h.RecordRun(it.Name, runID, day, it.Level)
		}
	}
	return plans
}

// Simulate is SimulateFunc with a single estimate map held constant across the window —
// the size-frozen schedule forecast (today's sizes, every day). Kept for callers (and
// tests) that have no per-day growth model.
func Simulate(dles []DLE, hist *catalog.History, est map[string]Estimate, forced map[string]bool, p Params, start time.Time, days int) []*Plan {
	return SimulateFunc(dles, hist, func(time.Time) map[string]Estimate { return est }, forced, p, start, days)
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

// promote pulls future fulls forward onto today to flatten the daily full-load
// calendar. Each not-yet-due DLE sits on the day (an offset from today) its full is
// due; today already carries its fixed load (the mandatory fulls). promote then
// repeatedly makes the single admissible move — pull one due full onto today — that
// most improves the calendar in lexicographic order: (1) lower the peak day, then
// (2) lower the variance of the daily load. Over a fixed window with a fixed total,
// minimizing variance is minimizing the sum of squared daily loads, so "flatter"
// means "smaller Σ load²". It stops when no admissible move improves either metric.
//
// A move is admissible only when it fits the per-run room (promotion spends only
// genuinely free capacity), the DLE was last fulled at least half a cycle ago (the
// recency floor — a just-fulled DLE sitting at the back of the window is not due
// soon, so pulling its full forward would spend a whole full for near-zero freshness
// and, worse, re-full it a day after its last full), and it does not overshoot:
// today's load *after* the move may not exceed the relieved day's load *after* the
// full leaves it. The no-overshoot guard is what keeps promotion honest. It forbids merely relocating a
// peak onto today — pulling a lone big DLE forward would move its whole size onto
// today for no net flattening, so a lone DLE (or one dominating its own deadline
// day) is never promoted. And it never pulls a full forward earlier than the
// crowding genuinely requires, so a DLE still fulls once per cycle: leveling a clump
// of shared-deadline DLEs is a phase-shift onto distinct days, not extra fulls.
//
// A DLE fulled today (days == 0) is excluded as a candidate: pulling its full
// "forward" onto the day it already lives on is pure waste and would recur on every
// same-day run. Staggering only buys anything across distinct days. It still counts
// toward today's load, though — a full an earlier same-day run already sealed is real
// weight on today's calendar, so the no-overshoot guard measures against it. That is
// what makes promotion idempotent across intraday reruns: once a run has staggered
// part of a shared-deadline clump onto today, a later run the same day sees that load
// and does not re-promote the rest onto the same day.
func promote(cands []*cand, cycle int, room int64) {
	if cycle < 1 {
		cycle = 7
	}
	// Deadline calendar over a cycle-length window, offsets 0..cycle-1 (0 == today).
	// load[off] is the total full bytes due that day; a not-yet-due incremental last
	// fulled `days` ago is due in cycle-days days.
	byOffset := map[int][]*cand{}
	load := make([]int64, cycle)
	for _, c := range cands {
		switch {
		case c.full:
			load[0] += c.estFull
		case c.days == 0:
			// Already fulled today by an earlier same-day run: the DLE now dumps only
			// a small incremental, but its full is real load already sitting on today's
			// calendar. Count it toward load[0] (though it is not itself a promotion
			// candidate) so the no-overshoot guard sees today's true full load. Without
			// this, a second run the same day would see today as empty of the fulls the
			// first run staggered onto it and re-promote the rest of the clump, piling a
			// whole shared-deadline clump onto one day across intraday reruns.
			load[0] += c.estFull
		case c.days > 0:
			off := cycle - c.days
			if off < 1 {
				off = 1
			}
			if off > cycle-1 {
				off = cycle - 1
			}
			byOffset[off] = append(byOffset[off], c)
			load[off] += c.estFull
		}
	}

	total := runBytes(cands)
	fitsRoom := func(c *cand) bool { return room < 0 || total-c.estIncr+c.estFull <= room }

	peak := func() int64 {
		var m int64
		for _, v := range load {
			if v > m {
				m = v
			}
		}
		return m
	}
	// sumSq is float64 because daily loads reach terabytes and their squares overflow
	// int64; the metric only needs to compare loads, not to be exact.
	sumSq := func() float64 {
		var s float64
		for _, v := range load {
			f := float64(v)
			s += f * f
		}
		return s
	}
	// better reports whether (p1,q1) beats (p2,q2): lower peak wins; on an equal
	// peak, lower sum-of-squares (variance) wins.
	better := func(p1 int64, q1 float64, p2 int64, q2 float64) bool {
		if p1 != p2 {
			return p1 < p2
		}
		return q1 < q2
	}

	take := func(c *cand, off int) {
		c.full, c.promoted = true, true
		c.reason = fmt.Sprintf("promoted full (leveled: pulled forward from due-in-%dd to flatten the daily load)", off)
		load[0] += c.estFull
		load[off] -= c.estFull
		total += c.estFull - c.estIncr
		cluster := byOffset[off]
		for i, x := range cluster {
			if x == c {
				byOffset[off] = append(cluster[:i], cluster[i+1:]...)
				break
			}
		}
		if len(byOffset[off]) == 0 {
			delete(byOffset, off)
		}
	}

	for {
		curPeak, curSq := peak(), sumSq()
		var best *cand
		var bestOff int
		var bestPeak int64
		var bestSq float64
		// Iterate offsets in sorted order (and each cluster in slice order) so the
		// scan — and thus tie-breaking, which takes the first move that ties the best
		// objective — is deterministic. Ranging the map directly would let Go's
		// randomized map-iteration order pick a different winner run to run, yielding
		// different schedules from identical input.
		for _, off := range sortedOffsets(byOffset) {
			for _, c := range byOffset[off] {
				s := c.estFull
				if !fitsRoom(c) {
					continue
				}
				// Recency floor: pulling this full onto today shortens its cycle to
				// c.days (it was last fulled c.days ago). A DLE less than half a cycle
				// past its full is not "due soon" — it is freshly done, sitting at the
				// back of the window — so promoting it spends a whole full to buy
				// near-zero freshness. Forbid it. Without this, staggering a clump refills
				// the far offset with just-fulled DLEs every day, and the greedy pass
				// re-promotes them one day after their full (a useless double promotion).
				if c.days < cycle/2 {
					continue
				}
				// No overshoot: today-after may not exceed the relieved day-after.
				if load[0]+s > load[off]-s {
					continue
				}
				newToday, newOff := load[0]+s, load[off]-s
				// Peak after the move: recompute the max with the two changed days.
				np := int64(0)
				for i, v := range load {
					switch i {
					case 0:
						v = newToday
					case off:
						v = newOff
					}
					if v > np {
						np = v
					}
				}
				// Σload² after: subtract the two old squares, add the two new ones.
				o0, oo := float64(load[0]), float64(load[off])
				n0, no := float64(newToday), float64(newOff)
				nq := curSq - o0*o0 - oo*oo + n0*n0 + no*no
				if !better(np, nq, curPeak, curSq) {
					continue // move doesn't improve the objective
				}
				if best == nil || better(np, nq, bestPeak, bestSq) {
					best, bestOff, bestPeak, bestSq = c, off, np, nq
				}
			}
		}
		if best == nil {
			return
		}
		take(best, bestOff)
	}
}

// sortedOffsets returns the calendar's deadline offsets in ascending order —
// soonest-due first, the order every promotion pass scans in.
func sortedOffsets(byOffset map[int][]*cand) []int {
	offs := make([]int, 0, len(byOffset))
	for off := range byOffset {
		offs = append(offs, off)
	}
	sort.Ints(offs)
	return offs
}
