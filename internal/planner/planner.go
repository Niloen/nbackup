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
// The one balancing lever is promotion: pulling a future full forward onto today
// to level the daily full load toward the cycle average (one full of every DLE,
// spread over the cycle). Small fulls are pulled in batches, filling today up to
// that average only when their deadlines genuinely crowd the days left; a full too
// large to ever fit under the average is instead destaggered one per run near its
// deadline. Both are bounded by the per-run room; the guards and the pacing are
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
// Estimates and params are sampled once and held constant across the window: this
// forecasts the *level schedule* from today's sizes, not capacity drift as runs
// accumulate. The per-day EstBytes therefore tracks the chosen levels, not a
// reclamation timeline. The bump decision likewise weighs today's level sizes, so
// a forecast past a simulated bump approximates the deeper level's size with the
// current one's — a schedule sketch, not an exact size projection.
func Simulate(dles []DLE, hist *catalog.History, est map[string]Estimate, forced map[string]bool, p Params, start time.Time, days int) []*Plan {
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
		plan := Build(dles, h, est, dayForced, p, date)
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
// toward the cycle average — avg, one full of every DLE spread over the cycle
// (Amanda's balanced size). It builds a deadline calendar — each not-yet-due DLE
// sits on the day (an offset from today) its full is due — and adds up today's
// already-fixed load (the mandatory fulls). avg splits the candidates into two
// regimes, because they overload a cycle in two different ways:
//
// Small fulls (<= avg) are batched: today is filled up to avg with the
// soonest-due small fulls, but only while their deadlines genuinely crowd the
// runway — some horizon of d days holds more than d*avg of small fulls, so
// leaving them all to their deadlines must overload a day. The horizon gate is
// byte-based and cumulative, so a swarm of tiny DLEs sharing a deadline is left
// alone while the days left can absorb it, then relieved several per run — never
// dribbled out one per day from weeks away. Promotion stops at avg: today never
// becomes the new peak, and a heavy day is shed ~avg per run over several runs
// rather than halved onto one.
//
// Oversize fulls (> avg) can never fit under the average — wherever one lands,
// its day carries at least its size — so leveling means keeping them apart, not
// batching them: when more oversize fulls share the next d days than there are
// days (count > d), one is pulled onto today, at most one per run — quiet early,
// one-per-day near the deadline. Each such move must not overshoot the day it
// relieves (today's resulting load may not exceed that day's load after the full
// leaves it), so a DLE dominating its own deadline day is never merely relocated,
// and a tiny co-deadline DLE cannot unlock the big move.
//
// Every move, in both regimes, must also fit the per-run room, so promotion
// spends only genuinely free capacity. And promotion only ever reacts to
// crowding: a light day under an uncrowded calendar stays light, because pulling
// a full forward always costs freshness (its next full comes sooner).
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
	var todayLoad, totalFull int64
	for _, c := range cands {
		totalFull += c.estFull
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
	// The balanced level. Zero means the estimates carry no size signal (or there
	// are no DLEs); with nothing to level against, promotion has no basis.
	avg := totalFull / int64(cycle)
	if avg <= 0 {
		return
	}

	total := runBytes(cands)
	fitsRoom := func(c *cand) bool { return room < 0 || total-c.estIncr+c.estFull <= room }
	take := func(c *cand, off int, reason string) {
		c.full, c.promoted, c.reason = true, true, reason
		todayLoad += c.estFull
		total += c.estFull - c.estIncr
		load[off] -= c.estFull
		cluster := byOffset[off]
		for i, x := range cluster {
			if x == c {
				byOffset[off] = append(cluster[:i], cluster[i+1:]...)
				break
			}
		}
		if len(byOffset[off]) == 0 {
			delete(byOffset, off)
			delete(load, off)
		}
	}

	// promoteSmall makes one small-full batch pick: find the widest crowded
	// horizon — the largest d whose fulls due within d days sum past d*avg — then
	// pull today (up to avg) the soonest-due small full that fits. One pick per
	// call; the horizon is recomputed after each, so the gate closes the moment
	// the crowding is relieved. In the crowding sum an oversize full counts as
	// avg, not its size: it consumes one run's budget on whatever day it lands
	// (small fulls sharing its deadline must clear off), but its excess is
	// irreducible — counting it fully would promote smalls that cannot actually
	// relieve anything (a tiny DLE sharing a huge DLE's deadline stays put).
	promoteSmall := func() bool {
		if todayLoad >= avg {
			return false
		}
		offsets := sortedOffsets(byOffset)
		horizon, horizonCum := 0, int64(0)
		var cum int64
		for _, off := range offsets {
			for _, c := range byOffset[off] {
				if c.estFull <= avg {
					cum += c.estFull
				} else {
					cum += avg
				}
			}
			if cum > int64(off)*avg {
				horizon, horizonCum = off, cum
			}
		}
		for _, off := range offsets {
			if off > horizon {
				return false
			}
			var pick *cand
			for _, c := range byOffset[off] {
				if c.estFull > avg || todayLoad+c.estFull > avg || !fitsRoom(c) {
					continue
				}
				if pick == nil || c.estFull > pick.estFull {
					pick = c
				}
			}
			if pick != nil {
				take(pick, off, fmt.Sprintf(
					"promoted full (due in %dd; ~%s of fulls crowd the next %dd, over the ~%s/run balanced level)",
					off, sizeutil.FormatBytes(horizonCum), horizon, sizeutil.FormatBytes(avg)))
				return true
			}
		}
		return false
	}

	// promoteOversize makes the run's one oversize pick: when more oversize fulls
	// share the next d days than there are days, they cannot each get a day of
	// their own — pull the soonest-due one that passes the guards onto today.
	promoteOversize := func() bool {
		offsets := sortedOffsets(byOffset)
		n := 0
		for _, off := range offsets {
			for _, c := range byOffset[off] {
				if c.estFull > avg {
					n++
				}
			}
			if n <= off {
				continue
			}
			for _, o := range offsets {
				if o > off {
					break
				}
				var pick *cand
				for _, c := range byOffset[o] {
					if c.estFull <= avg || !fitsRoom(c) {
						continue
					}
					// No overshoot: today's resulting load may not exceed the relieved day's
					// load *after* this full leaves it, else the move only relocates the peak.
					if todayLoad+c.estFull > load[o]-c.estFull {
						continue
					}
					if pick == nil || c.estFull > pick.estFull {
						pick = c
					}
				}
				if pick != nil {
					take(pick, o, fmt.Sprintf(
						"promoted full (due in %dd; %d fulls too large to batch crowd the next %dd — destaggering one per run)",
						o, n, off))
					return true
				}
			}
			return false
		}
		return false
	}

	// Batch small fulls first (each pick shrinks the calendar, so this
	// terminates); once nothing small is both crowded and fitting, allow at most
	// one oversize destagger per run. The order cannot starve the batch: an
	// oversize pick alone pushes today past avg, which closes the batch gate.
	for promoteSmall() {
	}
	promoteOversize()
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
