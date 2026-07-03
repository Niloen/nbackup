package catalog

import "sort"

// DLESummary is the per-DLE rollup behind `nb dle`: one backup source aggregated
// across every run in the catalog — the DLE-major view of the same archives the
// Run grouping presents run-major.
type DLESummary struct {
	DLE       string   // internal slug
	Display   string   // host:path id (record.Archive.DLEID)
	Runs      int      // archives across runs (one per run the DLE was dumped in)
	LastLevel int      // level of its most recent archive
	LastFull  string   // date of its most recent full; "" if never fulled
	Bytes     int64    // total compressed bytes across its archives
	Media     []string // media holding a copy of any of its archives, sorted
}

// DLESummaries aggregates the catalog per DLE across all runs, sorted by display
// id. Runs come in run order, so LastLevel/LastFull reflect each DLE's most
// recent run.
func (c *Catalog) DLESummaries() []DLESummary {
	type agg struct {
		sum   DLESummary
		media map[string]bool
	}
	aggs := map[string]*agg{}
	var order []string
	for _, s := range c.Runs() {
		ps := c.Placements(s.ID)
		for _, ar := range s.Archives {
			g := aggs[ar.DLE]
			if g == nil {
				g = &agg{sum: DLESummary{DLE: ar.DLE, Display: ar.DLEID()}, media: map[string]bool{}}
				aggs[ar.DLE] = g
				order = append(order, ar.DLE)
			}
			g.sum.Runs++
			g.sum.Bytes += ar.Compressed
			g.sum.LastLevel = ar.Level
			if ar.Level == 0 {
				g.sum.LastFull = s.Date()
			}
			for _, p := range ps {
				for _, pa := range p.Archives {
					if pa.DLE == ar.DLE {
						g.media[p.Medium] = true
					}
				}
			}
		}
	}
	sort.Slice(order, func(i, j int) bool { return aggs[order[i]].sum.Display < aggs[order[j]].sum.Display })
	out := make([]DLESummary, 0, len(order))
	for _, slug := range order {
		g := aggs[slug]
		for m := range g.media {
			g.sum.Media = append(g.sum.Media, m)
		}
		sort.Strings(g.sum.Media)
		out = append(out, g.sum)
	}
	return out
}
