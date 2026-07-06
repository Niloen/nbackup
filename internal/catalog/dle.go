package catalog

import (
	"sort"
	"time"
)

// DLESummary is the per-DLE rollup behind `nb dle`: one backup source aggregated
// across every run in the catalog — the DLE-major view of the same archives the
// Run grouping presents run-major.
type DLESummary struct {
	DLE          string    // internal slug
	Display      string    // host:path id (record.Archive.DLEID)
	Runs         int       // archives across runs (one per run the DLE was dumped in)
	LastLevel    int       // level of its most recent archive
	LastFull     string    // date of its most recent full; "" if never fulled
	LastBackupAt time.Time // commit time of its most recent archive at any level; zero if never
	Bytes        int64     // total compressed bytes across its archives
	Media        []string  // media holding a copy of any of its archives, sorted
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
			if ar.CreatedAt.After(g.sum.LastBackupAt) {
				g.sum.LastBackupAt = ar.CreatedAt
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

// StaleDLE is one configured DLE whose newest archive (any level) predates the
// staleness window, or that has no archive in the catalog at all.
type StaleDLE struct {
	DLE        string    // internal slug
	Display    string    // host:path id
	LastBackup time.Time // commit time of its newest archive; zero if never backed up
}

// StaleDLEs reports, among the given configured DLE slugs, those whose newest
// archive at any level is older than window (or absent entirely). The catalog
// itself only knows what it has seen, so the caller supplies the configured slugs
// — the same shape as drill.Ledger.Coverage, whose "never" half this mirrors. A
// DLE is judged by its newest archive at ANY level (an incremental counts as a
// backup), unlike LastFull/LastLevel above, which track the full-cycle position.
func (c *Catalog) StaleDLEs(dles []string, window time.Duration, now time.Time) []StaleDLE {
	last := map[string]DLESummary{}
	for _, s := range c.DLESummaries() {
		last[s.DLE] = s
	}
	var out []StaleDLE
	for _, slug := range dles {
		s, ok := last[slug]
		if !ok {
			out = append(out, StaleDLE{DLE: slug})
			continue
		}
		if now.Sub(s.LastBackupAt) >= window {
			out = append(out, StaleDLE{DLE: slug, Display: s.Display, LastBackup: s.LastBackupAt})
		}
	}
	return out
}
