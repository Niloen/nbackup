package accounting

import (
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
}

// ForecastCost projects the landing medium's monthly storage cost forward day by day
// over the given simulated plans (one per day from start; see scheduler.Simulate). It
// maintains a footprint of runs — appending each simulated run and evicting via the
// medium's own reclamation strategy and retention floor (the same primitives
// `nb prune` uses) — and reprices the survivors each day, so the curve reflects
// fulls/incrementals landing and pruning reclaiming. Pure and offline.
func (a *Accountant) ForecastCost(start time.Time, plans []*planner.Plan) []ForecastPoint {
	working := append([]record.Archive(nil), a.d.Cat.ArchivesOn(a.d.Landing)...)
	points := make([]ForecastPoint, 0, len(plans))
	for i, plan := range plans {
		date := start.AddDate(0, 0, i)
		ds := record.DateString(date)

		// Synthesize the day's run as archives (sized from the plan's estimates), replacing
		// any existing archives of the same run id so a re-simulation is idempotent.
		runID := record.IDFromParts(ds, 1)
		working = dropRun(working, runID)
		var runBytes int64
		for _, it := range plan.Items {
			working = append(working, record.Archive{Run: runID, DLE: it.Name, Level: it.Level, Compressed: it.EstBytes, CreatedAt: date})
			runBytes += it.EstBytes
		}

		// Reclaim against this medium's capacity, honoring the retention floor.
		floor := retention.Compute(working, a.d.LandingMinAge, date)
		var reclaimed int64
		for _, r := range a.d.LandingProfile.Reclaim(working, floor, date) {
			reclaimed += r.Bytes
			working = dropArchive(working, r.RunID, r.DLE)
		}

		var bytes int64
		for _, ar := range working {
			bytes += ar.Compressed
		}
		points = append(points, ForecastPoint{
			Date: ds, Bytes: bytes, Monthly: a.d.LandingCost.MonthlyStorage(bytes),
			RunBytes: runBytes, Reclaimed: reclaimed,
		})
	}
	return points
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

// SelectionCost prices a file-level recovery: the egress of the archives its selected
// members are extracted from.
func (a *Accountant) SelectionCost(steps []recovery.ExtractStep) ReadEstimate {
	refs := make([]archiveio.Ref, 0, len(steps))
	for _, st := range steps {
		refs = append(refs, archiveio.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level})
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
