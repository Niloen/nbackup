package engine

import (
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/restore"
	"github.com/Niloen/nbackup/internal/retention"
)

// This file is the dollar overlay on the engine's byte accounting. The planner and
// catalog speak bytes; here they are priced by the landing medium's media.Cost — a
// pure, offline calculation, no billing API. It mirrors the capacity overlay
// (StoredBytes / CapacityStatus) one layer up in dollars, so `nb plan` can show a
// monthly bill and the read paths can warn about egress before they pull from a cloud
// store. Pricing is flat (storage + egress + request) by design — see media.Cost.

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
func (e *Engine) CostSummary(plan *planner.Plan) CostSummary {
	cs := CostSummary{Priced: e.landingCost.Priced(), Provider: e.landingCost.Provider}
	cs.Bytes = e.StoredBytes()
	cs.Monthly = e.landingCost.MonthlyStorage(cs.Bytes)
	if plan != nil {
		for _, it := range plan.Items {
			cs.RunBytes += it.EstBytes
		}
		cs.Marginal = e.landingCost.MonthlyStorage(cs.RunBytes)
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

// ForecastCost projects the landing medium's monthly storage cost forward day by day,
// reusing the planner's run simulation. It maintains a footprint of slots — appending
// each simulated run and evicting via the medium's own reclamation strategy and
// retention floor (the same primitives `nb prune` uses) — and reprices the survivors
// each day, so the curve reflects fulls/incrementals landing and pruning reclaiming.
// Pure and offline.
func (e *Engine) ForecastCost(start time.Time, days int) []ForecastPoint {
	plans := e.Simulate(start, days)
	working := append([]*record.Slot(nil), e.cat.SlotsOn(e.mediumName)...)
	points := make([]ForecastPoint, 0, len(plans))
	for i, plan := range plans {
		date := start.AddDate(0, 0, i)
		ds := record.DateString(date)

		// Synthesize the day's run as a sealed slot (sized from the plan's estimates),
		// replacing any existing slot of the same id so a re-simulation is idempotent.
		sl := record.NewSlot("slot-"+ds, ds, 1, "forecast", date)
		var runBytes int64
		for _, it := range plan.Items {
			sl.AddArchive(record.Archive{DLE: it.Name, Level: it.Level, Compressed: it.EstBytes})
			runBytes += it.EstBytes
		}
		working = dropSlot(working, sl.ID)
		if len(sl.Archives) > 0 {
			_ = sl.Seal(date)
			working = append(working, sl)
		}

		// Reclaim against this medium's capacity, honoring the retention floor.
		floor := retention.Compute(working, e.minAge, date)
		var reclaimed int64
		for _, r := range e.profile.Reclaim(working, floor, date) {
			reclaimed += r.Bytes
			working = dropSlot(working, r.SlotID)
		}

		var bytes int64
		for _, s := range working {
			bytes += s.TotalBytes
		}
		points = append(points, ForecastPoint{
			Date: ds, Bytes: bytes, Monthly: e.landingCost.MonthlyStorage(bytes),
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
func (e *Engine) RestoreCost(dles []string, asOf string) ReadEstimate {
	target, err := recovery.AsOf(e.cat.Slots(), asOf)
	if err != nil {
		return ReadEstimate{Priced: e.landingCost.Priced(), Provider: e.landingCost.Provider}
	}
	var refs []archiveRef
	for _, dle := range dles {
		steps, err := restore.Chain(e.cat.Slots(), dle, target)
		if err != nil {
			continue
		}
		for _, s := range steps {
			refs = append(refs, archiveRef{s.SlotID, s.DLE, s.Level})
		}
	}
	return e.estimateRead(refs, "")
}

// SelectionCost prices a file-level recovery: the egress of the archives its selected
// members are extracted from.
func (e *Engine) SelectionCost(steps []recovery.ExtractStep) ReadEstimate {
	refs := make([]archiveRef, 0, len(steps))
	for _, st := range steps {
		refs = append(refs, archiveRef{st.SlotID, st.DLE, st.Level})
	}
	return e.estimateRead(refs, "")
}

// archiveRef names one archive to price (slot + DLE + level).
type archiveRef struct {
	slotID string
	dle    string
	level  int
}

// estimateRead prices reading the referenced archives off a medium. With forceMedium
// set (a medium-scoped drill) it prices on that medium; otherwise it discovers the
// medium a restore would read from (the copy openArchiveFrom prefers, landing first),
// so the estimate matches what a restore will actually pay. Payload bytes are
// medium-independent (the same ciphertext lands on every copy), so they come from the
// catalog regardless of which copy is read.
func (e *Engine) estimateRead(refs []archiveRef, forceMedium string) ReadEstimate {
	var est ReadEstimate
	for _, r := range refs {
		medium, a, ok := e.locateArchive(r, forceMedium)
		if !ok {
			continue
		}
		if est.Medium == "" {
			est.Medium = medium
		}
		est.Bytes += a.Compressed
		est.Parts += partCount(a)
	}
	if est.Medium == "" {
		est.Medium = forceMedium
	}
	c := e.costModelFor(est.Medium)
	est.Provider = c.Provider
	est.Priced = c.Priced()
	est.Cost = c.ReadCost(est.Bytes, est.Parts)
	return est
}

// locateArchive resolves an archive reference to the medium it will be read from and
// its catalog record. With forceMedium set it reads that medium's copy; otherwise it
// picks the copy a restore prefers (landing first).
func (e *Engine) locateArchive(r archiveRef, forceMedium string) (medium string, a record.Archive, ok bool) {
	s, err := e.cat.ReadSlot(r.slotID)
	if err != nil {
		return "", record.Archive{}, false
	}
	ar, found := findArchive(s, r.dle, r.level)
	if !found {
		return "", record.Archive{}, false
	}
	if forceMedium != "" {
		return forceMedium, ar, true
	}
	for _, p := range e.placementsFor(r.slotID) {
		if _, has := p.Parts(r.dle, r.level); has {
			return p.Medium, ar, true
		}
	}
	return "", record.Archive{}, false
}

// costModelFor returns a medium's pricing: the landing medium's cached model, or one
// built on demand for any other. Cost configs are validated at New, so a build error
// here falls back to the landing model rather than failing a read.
func (e *Engine) costModelFor(name string) media.Cost {
	if name == "" || name == e.mediumName {
		return e.landingCost
	}
	if d, ok := e.cfg.Media[name]; ok {
		if c, err := media.OpenCost(d.Type, media.Options(d.CostOptions())); err == nil {
			return c
		}
	}
	return e.landingCost
}

// partCount is an archive's file count for request pricing: its part count when it
// spanned volumes, else one.
func partCount(a record.Archive) int64 {
	if a.Parts > 1 {
		return int64(a.Parts)
	}
	return 1
}

func dropSlot(slots []*record.Slot, id string) []*record.Slot {
	out := slots[:0:0]
	for _, s := range slots {
		if s.ID != id {
			out = append(out, s)
		}
	}
	return out
}
