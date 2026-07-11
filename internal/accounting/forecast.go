package accounting

import (
	"sort"
	"strconv"
	"time"

	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/retention"
)

// This file is the capacity forecast: it replays the simulated schedule (one plan per
// day from scheduler.Simulate) onto every medium through the projection kernel
// (projection.go) and reads the answers off the surviving working sets — the per-medium
// fill curve, the reconstructed protected-floor history, the per-DLE footprint, and
// what the capacity buys in restore-point age. The forecast never invents aging:
// every simulated day runs the same retention.Compute + Profile.Reclaim the real
// prune uses. The one dollar in here is ForecastPoint.Monthly, a per-point overlay
// priced by the medium's cost model (cost.go owns pricing).

// forecastHistoryDays is how far the reconstructed history lines (the protected-floor
// band, the cartridges-in-use curve) reach back, matched to the forward projection
// horizon so the band spans a symmetric window. History only needs to span the
// retention window — older runs have aged out of the catalog by definition.
const forecastHistoryDays = 60

// historyStep is the day interval the history reconstructions sample at, trading
// line resolution for far fewer of the O(n²) retention passes — a floor that moves
// at the dump cadence loses nothing at this resolution.
const historyStep = 3

// ForecastPoint is one day of a medium's projected fill curve.
type ForecastPoint struct {
	Date      string  // YYYY-MM-DD
	Bytes     int64   // footprint at end of day (after the run lands and pruning reclaims)
	Monthly   float64 // recurring $/month for that footprint
	RunBytes  int64   // bytes the day's run added
	Reclaimed int64   // bytes reclaimed that day
	// Capacity is the medium's total bytes (0 = unbounded). Bytes already
	// reflects the day's reclaim, which can only free down to the retention floor — so
	// Bytes > Capacity means the protected set outgrew the medium and pruning cannot
	// make room: the capacity-pressure signal, without a second reclaim pass.
	Capacity int64
	// Protected is the retention floor: bytes pruning could NOT reclaim (within minimum_age,
	// the last recovery path, or a chain anchor). It is the MINIMUM capacity the medium
	// needs — the footprint can never be pruned below it. Its peak over the window is the
	// least capacity that keeps the retention promise.
	Protected int64
}

// OverCapacity reports whether the medium's protected footprint has outgrown its
// capacity on this day — true only for a bounded medium the forecast cannot fit.
func (p ForecastPoint) OverCapacity() bool { return p.Capacity > 0 && p.Bytes > p.Capacity }

// MediumForecast is one medium's projected fill over the window.
type MediumForecast struct {
	Medium string
	// VolumeStructured marks a discrete-volume medium (tape): it reclaims by whole-volume
	// rotation at write time, not by prune, so its capacity is measured in CARTRIDGES, not
	// bytes. Such a medium carries Volumes (a cartridge-count curve) instead of Points.
	VolumeStructured bool
	Points           []ForecastPoint // byte fill curve (projection) — disk/cloud
	History          []ForecastPoint // reconstructed protected-floor history (Bytes = floor); byte media only
	Depth            RestoreDepth    // what the capacity buys in restore-point age; byte media only
	Volumes          []VolumePoint   // cartridge-count curve — tape (history then projection)
	VolumeCeiling    int64           // available cartridges (config slots); 0 = unbounded (hand-loaded)
}

// DepthMark is the TOTAL capacity a medium needs to keep restore points back a given
// number of CYCLES — the cumulative size of that many most-recent recovery chains (a full
// plus its incrementals). Measured in cycles, not weeks, so one cycle is exactly one
// recovery chain: no calendar-window double-count where a 7-day window straddles a full
// boundary and has to keep two fulls. Increasing in Cycles.
type DepthMark struct {
	Cycles int
	Bytes  int64
}

// RestoreDepth answers "what is my capacity buying me in restore-point age": how many
// dump cycles of restore history the medium's capacity retains, and the per-cycle byte
// marks (each the TOTAL capacity to keep that many cycles) for the chart's ticks.
type RestoreDepth struct {
	CapacityCycles float64     // restore depth the capacity retains (cycles; interpolated)
	Marks          []DepthMark // TOTAL capacity to keep each cycle horizon (for axis ticks)
}

// VolumePoint is one day of a tape pool's cartridge-usage curve: how many cartridges hold
// a retention-protected run that day (are "in use"), reconstructed from the catalog for
// past days and simulated forward for future ones.
type VolumePoint struct {
	Date  string
	InUse int64
}

// VolumeOver returns the first projected day the tape pool needs more cartridges than it
// has (InUse > ceiling), or "" if it stays within its slots — the "run out of tapes"
// signal. A zero ceiling (hand-loaded drive) never trips: its shelf is unbounded.
func (m MediumForecast) VolumeOver() (date string, need int64) {
	if m.VolumeCeiling <= 0 {
		return "", 0
	}
	for _, p := range m.Volumes {
		if p.InUse > m.VolumeCeiling {
			return p.Date, p.InUse
		}
	}
	return "", 0
}

// ForecastCapacity projects EVERY medium's footprint forward over the given simulated
// plans (one per day from start; see scheduler.Simulate), routing each simulated
// archive to its dumptype's landing media and the sync targets that mirror them, and
// reclaiming each medium against its own profile and retention. A medium is included
// when it is a route over the window or already holds archives. Tape media carry a
// cartridge-count curve instead of a byte curve.
func (a *Accountant) ForecastCapacity(start time.Time, plans []*planner.Plan) []MediumForecast {
	targets := map[string]bool{}
	for _, plan := range plans {
		for _, it := range plan.Items {
			for _, m := range a.copyMediaFor(it.DLE.DumpTypeName()) {
				targets[m] = true // landing routes AND sync targets — so a copy-only tier is forecast
			}
		}
	}
	names := make([]string, 0, len(a.d.Cfg.Media))
	for name := range a.d.Cfg.Media {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic order (map iteration is not)
	var out []MediumForecast
	for _, name := range names {
		if !targets[name] && len(a.d.Cat.ArchivesOn(name)) == 0 {
			continue // neither a route this window nor already holding anything
		}
		prof, err := a.ProfileFor(name)
		if err != nil {
			continue
		}
		mf := MediumForecast{Medium: name, VolumeStructured: prof.VolumeSize() > 0}
		if mf.VolumeStructured {
			mf.Volumes = a.forecastVolumes(name, prof, start, plans)
			mf.VolumeCeiling = prof.Volumes()
		} else {
			mf.Points = a.forecastMedium(name, start, plans)
			mf.History = a.protectedHistory(name, start)
			mf.Depth = a.restoreDepth(name, start, plans, prof.TotalBytes())
		}
		out = append(out, mf)
	}
	return out
}

// forecastMedium projects one byte medium's footprint forward — the projection kernel's
// simulation with each day's totals read off it and repriced by the medium's cost model.
func (a *Accountant) forecastMedium(name string, start time.Time, plans []*planner.Plan) []ForecastPoint {
	prof, err := a.ProfileFor(name)
	if err != nil {
		return nil
	}
	cost := a.CostModelFor(name)
	capacity := prof.TotalBytes()
	points := make([]ForecastPoint, 0, len(plans))
	a.simulateMedium(name, start, plans, func(date time.Time, landed []record.Archive, floor retention.Floor, reclaimed int64, working []record.Archive) {
		var runBytes, bytes, protected int64
		for _, ar := range landed {
			runBytes += ar.Compressed
		}
		for _, ar := range working {
			bytes += ar.Compressed
			if floor.KeepsArchive(ar.Run, ar.DLE) {
				protected += ar.Compressed // the retention floor can't reclaim this — the irreducible minimum
			}
		}
		points = append(points, ForecastPoint{
			Date: record.DateString(date), Bytes: bytes, Monthly: cost.MonthlyStorage(bytes),
			RunBytes: runBytes, Reclaimed: reclaimed, Capacity: capacity, Protected: protected,
		})
	})
	return points
}

// protectedHistory reconstructs a byte medium's retention floor — the bytes pruning
// could NOT reclaim — over the last forecastHistoryDays (sampled every historyStep):
// the minimum-capacity line beneath the recorded footprint. For each past day it
// recomputes the Floor over the archives that existed then (archivesAsOf) and sums
// what it keeps. It flows into the projection's Protected to draw one continuous floor.
func (a *Accountant) protectedHistory(name string, now time.Time) []ForecastPoint {
	onMedium := a.d.Cat.ArchivesOn(name)
	if len(onMedium) == 0 {
		return nil
	}
	minAge := a.minAgeFor(name)
	pts := make([]ForecastPoint, 0, forecastHistoryDays/historyStep+1)
	for d := forecastHistoryDays - 1; d >= 0; d -= historyStep {
		day := now.AddDate(0, 0, -d)
		asOf := archivesAsOf(onMedium, day)
		floor := retention.Compute(asOf, asOf, minAge, day)
		var protected int64
		for _, ar := range asOf {
			if floor.KeepsArchive(ar.Run, ar.DLE) {
				protected += ar.Compressed
			}
		}
		pts = append(pts, ForecastPoint{Date: record.DateString(day), Bytes: protected})
	}
	return pts
}

// ForecastDLEFootprint projects one DLE's retained footprint on its landing medium
// forward over the plans: the bytes its OWN surviving archives occupy each day, after
// that medium's pruning (which weighs every DLE on it). It answers "how much storage
// will this DLE need," distinct from its dataset size that the evolution charts show.
// It is the projection kernel's simulation — the same working set ForecastCapacity
// sees for the medium, sync-received copies included — reporting only the target
// DLE's slice; nil when the DLE isn't scheduled (retired) or its route can't be
// resolved.
func (a *Accountant) ForecastDLEFootprint(slug string, start time.Time, plans []*planner.Plan) []ForecastPoint {
	medium := a.landingForSlug(slug, plans)
	if medium == "" {
		return nil
	}
	points := make([]ForecastPoint, 0, len(plans))
	a.simulateMedium(medium, start, plans, func(date time.Time, _ []record.Archive, _ retention.Floor, _ int64, working []record.Archive) {
		var bytes int64
		for _, ar := range working {
			if ar.DLE == slug {
				bytes += ar.Compressed
			}
		}
		points = append(points, ForecastPoint{Date: record.DateString(date), Bytes: bytes})
	})
	return points
}

// landingForSlug finds a DLE's authoritative (primary) landing medium from the plans,
// which carry its resolved dumptype. "" when the slug isn't scheduled in the window.
func (a *Accountant) landingForSlug(slug string, plans []*planner.Plan) string {
	for _, plan := range plans {
		for _, it := range plan.Items {
			if it.Name == slug {
				if ms := a.mediaFor(it.DLE.DumpTypeName()); len(ms) > 0 {
					return ms[0]
				}
				return ""
			}
		}
	}
	return ""
}

// depthCycles are the restore-depth horizons the forecast prices, in dump CYCLES. Each
// answers "how much capacity to keep restore points back this many cycles."
var depthCycles = []int{1, 2, 4, 8}

// restoreDepth prices what a medium's capacity buys in restore-point age, measured in
// dump CYCLES: it accumulates the projected runs UNCAPPED by capacity (so it can price
// depths deeper than today's capacity retains), groups each DLE's stream into recovery
// chains (chainSizes), and prices a depth of N cycles as the cumulative size of the N
// most-recent chains per DLE. One cycle is exactly one chain, so depth of one cycle
// lines up with the protected floor. Runs are sized at the CURRENT rate (earliest
// estimate per DLE+level), because this is a current-state question, not a growth
// projection.
func (a *Accountant) restoreDepth(name string, start time.Time, plans []*planner.Plan, capacity int64) RestoreDepth {
	if capacity <= 0 || len(plans) == 0 {
		return RestoreDepth{}
	}
	cur := map[string]int64{}
	curKey := func(it planner.Item) string { return it.Name + "\x00" + strconv.Itoa(it.Level) }
	for _, plan := range plans {
		for _, it := range plan.Items {
			if k := curKey(it); cur[k] == 0 {
				cur[k] = it.EstBytes
			}
		}
	}
	currentRate := func(it planner.Item) int64 { return cur[curKey(it)] }
	var sim []record.Archive
	for i, plan := range plans {
		sim = append(sim, a.routedDay(name, start.AddDate(0, 0, i), plan, currentRate)...)
	}
	if limit, capped := a.mediumRunCap(name); capped {
		sim = capRuns(sim, limit)
	}
	if len(sim) == 0 {
		return RestoreDepth{}
	}

	// Per DLE, group the run stream into chains; a DLE's k-th newest chain is its
	// restore depth k cycles back.
	byDLE := map[string][]record.Archive{}
	for _, ar := range sim {
		byDLE[ar.DLE] = append(byDLE[ar.DLE], ar)
	}
	chainsByDLE := map[string][]int64{}
	maxCycles := 0
	for dle, arcs := range byDLE {
		chains := chainSizes(arcs)
		chainsByDLE[dle] = chains
		if len(chains) > maxCycles {
			maxCycles = len(chains)
		}
	}

	var marks []DepthMark
	for _, nc := range depthCycles {
		if nc > maxCycles {
			break // deeper than the projected window has chains for
		}
		var b int64
		for _, chains := range chainsByDLE {
			k := nc
			if k > len(chains) {
				k = len(chains)
			}
			for i := 0; i < k; i++ {
				b += chains[i]
			}
		}
		marks = append(marks, DepthMark{Cycles: nc, Bytes: b})
	}
	if len(marks) == 0 {
		return RestoreDepth{}
	}
	rd := RestoreDepth{Marks: marks}
	// Interpolate how many cycles of restore history the capacity buys.
	for i, m := range marks {
		if m.Bytes > capacity {
			break
		}
		rd.CapacityCycles = float64(m.Cycles)
		if i+1 < len(marks) {
			next := marks[i+1]
			if span := next.Bytes - m.Bytes; span > 0 {
				frac := float64(capacity-m.Bytes) / float64(span)
				rd.CapacityCycles = float64(m.Cycles) + frac*float64(next.Cycles-m.Cycles)
			}
		}
	}
	return rd
}
