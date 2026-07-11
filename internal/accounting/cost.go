package accounting

import (
	"sort"
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

// MediumForecast is one medium's projected fill over the window.
type MediumForecast struct {
	Medium string
	// VolumeStructured marks a discrete-volume medium (tape): it reclaims by whole-volume
	// rotation at write time, not by prune, so a byte fill curve is not meaningful and
	// Points is left nil. The surface shows "N volumes" thinking instead (as pools do).
	VolumeStructured bool
	Points           []ForecastPoint
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
			for _, m := range a.mediaFor(it.DLE.DumpTypeName()) {
				targets[m] = true
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
		if !mf.VolumeStructured {
			mf.Points = a.forecastMedium(name, start, plans)
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
			if !contains(a.mediaFor(it.DLE.DumpTypeName()), name) {
				continue // this DLE's authoritative copy lands elsewhere
			}
			working = append(working, record.Archive{Run: runID, DLE: it.Name, Level: it.Level, Compressed: it.EstBytes, CreatedAt: date})
			runBytes += it.EstBytes
		}

		// Reclaim against this medium's capacity, honoring the retention floor.
		floor := retention.Compute(working, working, minAge, date)
		var reclaimed int64
		for _, r := range prof.Reclaim(capacity, working, floor, date) {
			reclaimed += r.Bytes
			working = dropArchive(working, r.RunID, r.DLE)
		}

		var bytes int64
		for _, ar := range working {
			bytes += ar.Compressed
		}
		points = append(points, ForecastPoint{
			Date: record.DateString(date), Bytes: bytes, Monthly: cost.MonthlyStorage(bytes),
			RunBytes: runBytes, Reclaimed: reclaimed, Capacity: capacity,
		})
	}
	return points
}

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

// mediaFor is the landing route for a dumptype (the media its authoritative copies are
// written to), empty when the route cannot be resolved.
func (a *Accountant) mediaFor(dumptype string) []string {
	m, err := a.d.Cfg.LandingsForDumptype(dumptype)
	if err != nil {
		return nil
	}
	return m
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
