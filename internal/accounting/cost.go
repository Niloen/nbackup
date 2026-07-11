package accounting

import (
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
)

// This file is the dollar overlay on the byte accounting. The planner and catalog
// speak bytes; here they are priced by a medium's media.Cost — a pure, offline
// calculation, no billing API. It prices the current footprint and the next run
// (CostSummary), and the read paths (restore, recover, drill) so they can warn about
// egress before they pull from a cloud store. Pricing is flat (storage + egress +
// request) by design — see media.Cost. The forward-looking byte curves live in
// forecast.go; the only dollar there is the per-point Monthly, priced by the same
// models this file resolves (CostModelFor).

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
	c := a.CostModelFor(a.d.Landing)
	cs := CostSummary{Priced: c.Priced(), Provider: c.Provider}
	cs.Bytes = a.StoredBytes()
	cs.Monthly = c.MonthlyStorage(cs.Bytes)
	if plan != nil {
		for _, it := range plan.Items {
			cs.RunBytes += it.EstBytes
		}
		cs.Marginal = c.MonthlyStorage(cs.RunBytes)
	}
	return cs
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
		c := a.CostModelFor(a.d.Landing)
		return ReadEstimate{Priced: c.Priced(), Provider: c.Provider}
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

// CostModelFor returns a medium's pricing model, built from its config. Every
// configured medium's cost options are validated at engine construction, so a build
// failure cannot arise for a configured medium; an unknown or unconfigured name
// yields the zero (unpriced) model — never another medium's rates, so a resolution
// gap can only under-report, not misprice.
func (a *Accountant) CostModelFor(name string) media.Cost {
	if d, ok := a.d.Cfg.Media[name]; ok {
		if c, err := media.OpenCost(d.Type, media.Options(d.CostOptions())); err == nil {
			return c
		}
	}
	return media.Cost{}
}

// partCount is an archive's file count for request pricing: its part count when it
// spanned volumes, else one.
func partCount(a record.Archive) int64 {
	if a.Parts > 1 {
		return int64(a.Parts)
	}
	return 1
}
