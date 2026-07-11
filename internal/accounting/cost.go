package accounting

import (
	"sort"
	"strconv"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/retention"
)

// This file is the dollar overlay on the byte accounting. The planner and
// catalog speak bytes; here they are priced by the landing medium's media.Cost — a
// pure, offline calculation, no billing API. It mirrors the capacity arithmetic one
// layer up in dollars, so `nb plan` can show a monthly bill and the read paths can
// warn about egress before they pull from a cloud store. Pricing is flat (storage +
// egress + request) by design — see media.Cost.

// CostSummary is the landing medium's cost picture for a single planned run.
type CostSummary struct {
	Priced   bool    // false for a local medium (no recurring cloud bill) — caller hides the block
	Provider string  // the rate table in use (e.g. "aws-s3")
	Bytes    int64   // current footprint on the landing medium
	Monthly  float64 // recurring $/month for that footprint
	RunBytes int64   // the next run's estimated added bytes
	Marginal float64 // added $/month the next run brings
}

// CostSummary prices the current footprint and the next run on the landing medium.
// plan may be nil (footprint only).
func (a *Accountant) CostSummary(plan *planner.Plan) CostSummary {
	cs := CostSummary{Priced: a.d.LandingCost.Priced(), Provider: a.d.LandingCost.Provider}
	cs.Bytes = a.StoredBytes()
	cs.Monthly = a.d.LandingCost.MonthlyStorage(cs.Bytes)
	if plan != nil {
		for _, it := range plan.Items {
			cs.RunBytes += it.EstBytes
		}
		cs.Marginal = a.d.LandingCost.MonthlyStorage(cs.RunBytes)
	}
	return cs
}

// ForecastPoint is one day of the projected cost curve.
type ForecastPoint struct {
	Date      string  // YYYY-MM-DD
	Bytes     int64   // footprint at end of day (after the run lands and pruning reclaims)
	Monthly   float64 // recurring $/month for that footprint
	RunBytes  int64   // bytes the day's run added
	Reclaimed int64   // bytes reclaimed that day
	// Capacity is the landing medium's total bytes (0 = unbounded). Bytes already
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

// OverCapacity reports whether the landing's protected footprint has outgrown its
// capacity on this day — true only for a bounded medium the forecast cannot fit.
func (p ForecastPoint) OverCapacity() bool { return p.Capacity > 0 && p.Bytes > p.Capacity }

// ForecastCost projects the LANDING medium's monthly storage cost and capacity
// headroom forward day by day over the given simulated plans (one per day from start;
// see scheduler.Simulate). See forecastMedium for the mechanics; this is the landing
// slice of the per-medium picture ForecastCapacity draws.
func (a *Accountant) ForecastCost(start time.Time, plans []*planner.Plan) []ForecastPoint {
	return a.forecastMedium(a.d.Landing, start, plans)
}

// forecastHistoryDays is how far the protected-floor line reconstructs backward, matched
// to the forward projection horizon so the "minimum capacity" band spans a symmetric window.
const forecastHistoryDays = 60

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

// ForecastCapacity is the per-medium generalization of ForecastCost: it projects EVERY
// size-structured landing medium's footprint forward, routing each simulated archive to
// its dumptype's landing medium and reclaiming each medium against its own profile and
// retention. A medium is included when it is a landing route over the window or already
// holds archives. Tape media are flagged (no byte curve). Sync-copy targets are not yet
// projected — their timing needs the sync schedule — so this is landing/route capacity.
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
			mf.History = a.protectedHistory(name, a.d.Cfg.MinAgeFor(a.d.Cfg.Media[name]), start)
			mf.Depth = a.restoreDepth(name, start, plans, prof.TotalBytes())
		}
		out = append(out, mf)
	}
	return out
}

// forecastMedium projects one medium's footprint forward: each simulated day it adds the
// day's archives that ROUTE to this medium (their dumptype's landing), reclaims against
// this medium's own capacity + retention floor (the same primitives `nb prune` uses),
// and reprices the survivors. Pure and offline — the per-medium core of ForecastCost and
// ForecastCapacity.
func (a *Accountant) forecastMedium(name string, start time.Time, plans []*planner.Plan) []ForecastPoint {
	prof, err := a.ProfileFor(name)
	if err != nil {
		return nil
	}
	cost := a.CostModelFor(name)
	minAge := a.d.Cfg.MinAgeFor(a.d.Cfg.Media[name])
	capacity := prof.TotalBytes()
	limit, capped := a.mediumRunCap(name)
	working := append([]record.Archive(nil), a.d.Cat.ArchivesOn(name)...)
	points := make([]ForecastPoint, 0, len(plans))
	for i, plan := range plans {
		date := start.AddDate(0, 0, i)

		// Synthesize the day's routed archives (sized from the plan's estimates), replacing
		// any existing archives of the same run id so a re-simulation is idempotent.
		runID := record.IDFromTime(date)
		working = dropRun(working, runID)
		var runBytes int64
		for _, it := range plan.Items {
			if !contains(a.copyMediaFor(it.DLE.DumpTypeName()), name) {
				continue // this medium neither routes nor receives a sync copy of this DLE
			}
			working = append(working, record.Archive{Run: runID, DLE: it.Name, Level: it.Level, Compressed: it.EstBytes, CreatedAt: date})
			runBytes += it.EstBytes
		}
		if capped {
			working = capRuns(working, limit) // a sync target mirrors only its last N runs
		}

		// Reclaim against this medium's capacity, honoring the retention floor.
		floor := retention.Compute(working, working, minAge, date)
		var reclaimed int64
		for _, r := range prof.Reclaim(capacity, working, floor, date) {
			reclaimed += r.Bytes
			working = dropArchive(working, r.RunID, r.DLE)
		}

		var bytes, protected int64
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
	}
	return points
}

// protectedHistory reconstructs a byte medium's retention floor — the bytes pruning
// could NOT reclaim — for each of the last forecastHistoryDays: the minimum-capacity line
// beneath the recorded footprint. Same as-of-date reconstruction as tapeHistory: for each
// past day it recomputes the Floor over the archives that existed then and sums what it
// keeps. It flows into the projection's Protected to draw one continuous floor.
func (a *Accountant) protectedHistory(name string, minAge time.Duration, now time.Time) []ForecastPoint {
	onMedium := a.d.Cat.ArchivesOn(name)
	if len(onMedium) == 0 {
		return nil
	}
	// The floor history is just a line on the chart, so sample it every historyStep days
	// rather than daily — an O(n²) retention.Compute per point is the dominant forecast
	// cost, and a floor that moves at the dump cadence loses nothing at this resolution.
	pts := make([]ForecastPoint, 0, forecastHistoryDays/historyStep+1)
	for d := forecastHistoryDays - 1; d >= 0; d -= historyStep {
		day := now.AddDate(0, 0, -d)
		var asOf []record.Archive
		for _, ar := range onMedium {
			if !ar.CreatedAt.After(day) {
				asOf = append(asOf, ar)
			}
		}
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

// historyStep is the day interval the floor-history reconstruction samples at, trading
// line resolution for far fewer of the O(n²) retention passes.
const historyStep = 3

// ForecastDLEFootprint projects one DLE's retained footprint on its landing medium
// forward over the plans: the bytes its OWN surviving archives occupy each day, after
// that medium's pruning (which weighs every DLE on it). It answers "how much storage
// will this DLE need," distinct from its dataset size that the evolution charts show. It
// shares forecastMedium's per-medium simulation but reports only the target DLE's slice;
// nil when the DLE isn't scheduled (retired) or its route can't be resolved.
func (a *Accountant) ForecastDLEFootprint(slug string, start time.Time, plans []*planner.Plan) []ForecastPoint {
	medium := a.landingForSlug(slug, plans)
	if medium == "" {
		return nil
	}
	prof, err := a.ProfileFor(medium)
	if err != nil {
		return nil
	}
	minAge := a.d.Cfg.MinAgeFor(a.d.Cfg.Media[medium])
	capacity := prof.TotalBytes()
	working := append([]record.Archive(nil), a.d.Cat.ArchivesOn(medium)...)
	points := make([]ForecastPoint, 0, len(plans))
	for i, plan := range plans {
		date := start.AddDate(0, 0, i)
		runID := record.IDFromTime(date)
		working = dropRun(working, runID)
		for _, it := range plan.Items {
			if !contains(a.mediaFor(it.DLE.DumpTypeName()), medium) {
				continue
			}
			working = append(working, record.Archive{Run: runID, DLE: it.Name, Level: it.Level, Compressed: it.EstBytes, CreatedAt: date})
		}
		floor := retention.Compute(working, working, minAge, date)
		for _, r := range prof.Reclaim(capacity, working, floor, date) {
			working = dropArchive(working, r.RunID, r.DLE)
		}
		var bytes int64
		for _, ar := range working {
			if ar.DLE == slug {
				bytes += ar.Compressed
			}
		}
		points = append(points, ForecastPoint{Date: record.DateString(date), Bytes: bytes})
	}
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

// restoreDepth prices what a medium's capacity buys in restore-point age, measured in dump
// CYCLES. It groups the projected runs into RECOVERY CHAINS (a full plus the incrementals
// that build on it — the unit you must keep whole to restore to any point in that cycle),
// then a depth of N cycles is the cumulative size of the N most-recent chains per DLE. So
// one cycle is exactly one chain: no calendar-window double-count where "1 week" straddles
// a full boundary and needs two fulls, and depth of one cycle lines up with the protected
// floor. Runs are sized at the CURRENT rate (earliest estimate per DLE+level), so this is a
// current-state question, not a growth projection.
func (a *Accountant) restoreDepth(name string, start time.Time, plans []*planner.Plan, capacity int64) RestoreDepth {
	if capacity <= 0 || len(plans) == 0 {
		return RestoreDepth{}
	}
	limit, capped := a.mediumRunCap(name)
	cur := map[string]int64{}
	curKey := func(it planner.Item) string { return it.Name + "\x00" + strconv.Itoa(it.Level) }
	for _, plan := range plans {
		for _, it := range plan.Items {
			if k := curKey(it); cur[k] == 0 {
				cur[k] = it.EstBytes
			}
		}
	}
	var sim []record.Archive
	for i, plan := range plans {
		date := start.AddDate(0, 0, i)
		runID := record.IDFromTime(date)
		for _, it := range plan.Items {
			if !contains(a.copyMediaFor(it.DLE.DumpTypeName()), name) {
				continue
			}
			sim = append(sim, record.Archive{Run: runID, DLE: it.Name, Level: it.Level, Compressed: cur[curKey(it)], CreatedAt: date})
		}
	}
	if capped {
		sim = capRuns(sim, limit)
	}
	if len(sim) == 0 {
		return RestoreDepth{}
	}

	// Per DLE, split the run stream into chains (a full opens one; incrementals join the
	// open chain), then keep chain sizes newest-first. A DLE's k-th newest chain is its
	// restore depth k cycles back.
	byDLE := map[string][]record.Archive{}
	for _, ar := range sim {
		byDLE[ar.DLE] = append(byDLE[ar.DLE], ar)
	}
	chainsByDLE := map[string][]int64{}
	maxCycles := 0
	for dle, arcs := range byDLE {
		sort.Slice(arcs, func(i, j int) bool { return arcs[i].CreatedAt.Before(arcs[j].CreatedAt) })
		var chains []int64
		for _, ar := range arcs {
			if ar.Level == 0 || len(chains) == 0 {
				chains = append(chains, 0)
			}
			chains[len(chains)-1] += ar.Compressed
		}
		for i, j := 0, len(chains)-1; i < j; i, j = i+1, j-1 { // reverse -> newest first
			chains[i], chains[j] = chains[j], chains[i]
		}
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

// mediaFor is the landing route for a dumptype (the media its authoritative copies are
// written to), empty when the route cannot be resolved.
func (a *Accountant) mediaFor(dumptype string) []string {
	m, err := a.d.Cfg.LandingsForDumptype(dumptype)
	if err != nil {
		return nil
	}
	return m
}

// copyMediaFor returns every medium that will hold a copy of a dumptype's archives: its
// landing route PLUS every sync target reachable from those media through the sync rules
// (transitive closure). An auto rule (from: "") mirrors the config landing. This is the
// routing the capacity forecast uses so a sync-target medium — an offsite/archive tier
// that never appears in a landing route — is projected too, not just the landing.
//
// Approximations (documented, conservative): auto rules are sourced from the config
// landing (a dumptype routed only to a custom medium is not auto-mirrored); a rule's
// `last:` window is applied as a per-target run cap (see mediumRunCap), not modeled per
// hop; per-run source selection (sourceFor) is not replayed. The per-medium retention in
// forecastMedium ages the projected copies exactly as it does routed archives.
func (a *Accountant) copyMediaFor(dumptype string) []string {
	set := map[string]bool{}
	for _, m := range a.mediaFor(dumptype) {
		set[m] = true
	}
	landings, _ := a.d.Cfg.LandingNames()
	for changed := true; changed; {
		changed = false
		for _, r := range a.d.Cfg.Sync {
			if r.To == "" || set[r.To] {
				continue
			}
			from := false
			if r.From == "" { // auto: mirrors the landing
				for _, l := range landings {
					if set[l] {
						from = true
						break
					}
				}
			} else {
				from = set[r.From]
			}
			if from {
				set[r.To] = true
				changed = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// mediumRunCap reports the most-recent-run limit a PURE sync target holds — the loosest
// `last:` among the rules feeding it — so an expensive tier (a `last: 30` tape) is not
// projected holding its whole retention window. A landing route, or a target fed by any
// unbounded (last: 0) rule, is uncapped: it holds everything its own retention keeps.
func (a *Accountant) mediumRunCap(name string) (limit int, capped bool) {
	dts := []string{""}
	for dt := range a.d.Cfg.DumpTypes {
		dts = append(dts, dt)
	}
	for _, dt := range dts {
		if contains(a.mediaFor(dt), name) {
			return 0, false // a landing route holds all its routed runs
		}
	}
	for _, r := range a.d.Cfg.Sync {
		if r.To != name {
			continue
		}
		if r.Last <= 0 {
			return 0, false // an unbounded rule feeds it — no run cap
		}
		capped = true
		if r.Last > limit {
			limit = r.Last
		}
	}
	return limit, capped
}

// capRuns keeps only the `limit` most-recent runs' archives (run ids sort chronologically),
// modelling a sync target that mirrors just the last N runs of its source.
func capRuns(working []record.Archive, limit int) []record.Archive {
	if limit <= 0 {
		return working
	}
	ids := map[string]bool{}
	for _, a := range working {
		ids[a.Run] = true
	}
	if len(ids) <= limit {
		return working
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(sorted))) // newest first
	keep := map[string]bool{}
	for i := 0; i < limit; i++ {
		keep[sorted[i]] = true
	}
	out := make([]record.Archive, 0, len(working)) // fresh slice: never alias the caller's backing
	for _, a := range working {
		if keep[a.Run] {
			out = append(out, a)
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ReadEstimate is the cost of reading a set of archives back off a medium — the
// egress a restore, recover, or offsite drill spends before it can hand back bytes.
type ReadEstimate struct {
	Medium   string
	Provider string
	Priced   bool
	Bytes    int64   // compressed payload read off the medium
	Parts    int64   // number of files/parts fetched (request count)
	Cost     float64 // egress + request cost
	Ranged   bool    // every archive in the estimate is read in ranges (only the selected members), not whole
}

// ReadItem is one archive's pre-computed contribution to a read estimate: the encoded
// bytes that will be pulled off the medium and in how many fetches. A ranged read makes
// Bytes a fraction of the whole archive; the read layer computes it (only it knows the
// frame plan), accounting only prices it.
type ReadItem struct {
	Ref    archiveio.Ref
	Bytes  int64
	Parts  int64
	Ranged bool
}

// PriceRead prices a set of pre-computed per-archive reads on the medium each is read
// from (the copy a restore prefers, landing first) — the ranged-aware sibling of
// EstimateRead, which prices whole archives straight from the catalog. Bytes already
// reflect any ranged read, so this only resolves the medium and applies its rate table.
// Ranged is set when every priced archive is read in ranges, so the caller can say so.
func (a *Accountant) PriceRead(items []ReadItem) ReadEstimate {
	est := ReadEstimate{Ranged: len(items) > 0}
	for _, it := range items {
		medium, _, ok := a.locateArchive(it.Ref, "")
		if !ok {
			continue
		}
		if est.Medium == "" {
			est.Medium = medium
		}
		est.Bytes += it.Bytes
		est.Parts += it.Parts
		if !it.Ranged {
			est.Ranged = false
		}
	}
	c := a.CostModelFor(est.Medium)
	est.Provider = c.Provider
	est.Priced = c.Priced()
	est.Cost = c.ReadCost(est.Bytes, est.Parts)
	return est
}

// ReadRow is one archive's resolved read pricing: the medium a restore would read it from
// and that medium's cost picture for the pre-computed bytes/fetches. It backs the
// per-archive extraction plan, where each row names its own copy (archives of one
// selection can land on different media, so a single aggregate medium would misattribute).
type ReadRow struct {
	Medium   string
	Provider string
	Priced   bool
	Cost     float64
}

// PriceReadRow prices one pre-computed read on the copy a restore would read it from —
// the per-archive sibling of PriceRead's aggregate, for the extraction plan. An
// unlocatable ref (no placement) yields a zero, unpriced row.
func (a *Accountant) PriceReadRow(it ReadItem) ReadRow {
	medium, _, ok := a.locateArchive(it.Ref, "")
	if !ok {
		return ReadRow{}
	}
	c := a.CostModelFor(medium)
	return ReadRow{Medium: medium, Provider: c.Provider, Priced: c.Priced(), Cost: c.ReadCost(it.Bytes, it.Parts)}
}

// RestoreCost prices a whole-DLE restore (or every DLE) as of a date — the egress of
// the chains the restore would replay, read off the copy a restore would pick (the
// landing medium first). DLEs with no backup as of the date contribute nothing.
func (a *Accountant) RestoreCost(dles []string, asOf string) ReadEstimate {
	target, err := recovery.AsOf(a.d.Cat.Archives(), asOf)
	if err != nil {
		return ReadEstimate{Priced: a.d.LandingCost.Priced(), Provider: a.d.LandingCost.Provider}
	}
	var refs []archiveio.Ref
	for _, dle := range dles {
		steps, err := recovery.Chain(a.d.Cat.Archives(), dle, target)
		if err != nil {
			continue
		}
		for _, s := range steps {
			refs = append(refs, archiveio.Ref{Run: s.RunID, DLE: s.DLE, Level: s.Level})
		}
	}
	return a.EstimateRead(refs, "")
}

// EstimateRead prices reading the referenced archives off a medium. With forceMedium
// set (a medium-scoped drill) it prices on that medium; otherwise it discovers the
// medium a restore would read from (the copy a restore prefers, landing first),
// so the estimate matches what a restore will actually pay. Payload bytes are
// medium-independent (the same ciphertext lands on every copy), so they come from the
// catalog regardless of which copy is read.
func (a *Accountant) EstimateRead(refs []archiveio.Ref, forceMedium string) ReadEstimate {
	var est ReadEstimate
	for _, r := range refs {
		medium, ar, ok := a.locateArchive(r, forceMedium)
		if !ok {
			continue
		}
		if est.Medium == "" {
			est.Medium = medium
		}
		est.Bytes += ar.Compressed
		est.Parts += partCount(ar)
	}
	if est.Medium == "" {
		est.Medium = forceMedium
	}
	c := a.CostModelFor(est.Medium)
	est.Provider = c.Provider
	est.Priced = c.Priced()
	est.Cost = c.ReadCost(est.Bytes, est.Parts)
	return est
}

// locateArchive resolves an archive reference to the medium it will be read from and
// its catalog record. With forceMedium set it reads that medium's copy; otherwise it
// picks the copy a restore prefers (landing first).
func (a *Accountant) locateArchive(r archiveio.Ref, forceMedium string) (medium string, ar record.Archive, ok bool) {
	s, err := a.d.Cat.ReadRun(r.Run)
	if err != nil {
		return "", record.Archive{}, false
	}
	rec, found := s.Archive(r.DLE, r.Level)
	if !found {
		return "", record.Archive{}, false
	}
	if forceMedium != "" {
		return forceMedium, rec, true
	}
	for _, p := range a.d.PlacementsFor(r.Run) {
		if _, has := p.Parts(r.DLE, r.Level); has {
			return p.Medium, rec, true
		}
	}
	return "", record.Archive{}, false
}

// CostModelFor returns a medium's pricing: the landing medium's cached model, or one
// built on demand for any other. Cost configs are validated at engine construction,
// so a build error here falls back to the landing model rather than failing a read.
func (a *Accountant) CostModelFor(name string) media.Cost {
	if name == "" || name == a.d.Landing {
		return a.d.LandingCost
	}
	if d, ok := a.d.Cfg.Media[name]; ok {
		if c, err := media.OpenCost(d.Type, media.Options(d.CostOptions())); err == nil {
			return c
		}
	}
	return a.d.LandingCost
}

// partCount is an archive's file count for request pricing: its part count when it
// spanned volumes, else one.
func partCount(a record.Archive) int64 {
	if a.Parts > 1 {
		return int64(a.Parts)
	}
	return 1
}

// dropRun removes every archive of a run id from the working set.
func dropRun(archives []record.Archive, id string) []record.Archive {
	out := archives[:0:0]
	for _, a := range archives {
		if a.Run != id {
			out = append(out, a)
		}
	}
	return out
}

// dropArchive removes one DLE's archive from a run in the working set (the
// per-archive peer of dropRun), so the cost forecast mirrors per-archive reclamation.
func dropArchive(archives []record.Archive, id, dle string) []record.Archive {
	out := archives[:0:0]
	for _, a := range archives {
		if a.Run == id && a.DLE == dle {
			continue
		}
		out = append(out, a)
	}
	return out
}
