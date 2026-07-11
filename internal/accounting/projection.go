package accounting

import (
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/retention"
)

// This file is the projection kernel: the one place the simulated schedule is
// replayed onto a medium. Every forecast — the per-medium byte curve, the per-DLE
// footprint, the restore-depth pricing, the tape reel packing — builds its days from
// routedDay (which archives land here, at what size) and, for the byte media, drives
// simulateMedium (land, cap, reclaim, survive). The consumers differ only in what
// they aggregate; routing, sizing, and the sync run cap can never drift apart.

// sizer prices one plan item for a projection. forecastMedium sizes by the plan's
// (growth-aware) estimate; restoreDepth overrides with the current rate, because
// "how far back can I restore" is a current-state question.
type sizer func(it planner.Item) int64

// planEstimate is the default sizer: the plan's own growth-aware estimate.
func planEstimate(it planner.Item) int64 { return it.EstBytes }

// routedDay synthesizes the archives of one simulated day that land on medium —
// every plan item whose dumptype routes here or syncs a copy here (copyMediaFor) —
// stamped with the day's clock-minted run id and sized by size.
func (a *Accountant) routedDay(medium string, date time.Time, plan *planner.Plan, size sizer) []record.Archive {
	runID := record.IDFromTime(date)
	var out []record.Archive
	for _, it := range plan.Items {
		if !contains(a.copyMediaFor(it.DLE.DumpTypeName()), medium) {
			continue // this medium neither routes nor receives a sync copy of this DLE
		}
		out = append(out, record.Archive{Run: runID, DLE: it.Name, Level: it.Level, Compressed: size(it), CreatedAt: date})
	}
	return out
}

// simulateMedium replays the plans onto one byte medium day by day — seed the working
// set from the catalog, land each day's routed archives (replacing same-run archives so
// a re-simulation is idempotent), apply the sync run cap, compute the retention floor,
// reclaim against the medium's capacity — and hands visit each day's outcome: the day's
// landed archives, the floor, the bytes reclaimed, and the surviving working set.
// Callers aggregate what they care about (totals, one DLE's slice); the simulation
// itself lives only here.
func (a *Accountant) simulateMedium(name string, start time.Time, plans []*planner.Plan,
	visit func(date time.Time, landed []record.Archive, floor retention.Floor, reclaimed int64, working []record.Archive)) {
	prof, err := a.ProfileFor(name)
	if err != nil {
		return
	}
	minAge := a.minAgeFor(name)
	capacity := prof.TotalBytes()
	limit, capped := a.mediumRunCap(name)
	working := append([]record.Archive(nil), a.d.Cat.ArchivesOn(name)...)
	for i, plan := range plans {
		date := start.AddDate(0, 0, i)
		day := a.routedDay(name, date, plan, planEstimate)
		working = dropRun(working, record.IDFromTime(date))
		working = append(working, day...)
		if capped {
			working = capRuns(working, limit) // a sync target mirrors only its last N runs
		}
		floor := retention.Compute(working, working, minAge, date)
		var reclaimed int64
		for _, r := range prof.Reclaim(capacity, working, floor, date) {
			reclaimed += r.Bytes
			working = dropArchive(working, r.RunID, r.DLE)
		}
		visit(date, day, floor, reclaimed, working)
	}
}

// chainSizes groups one DLE's archives into recovery chains — a full (level 0) opens a
// chain, the incrementals built on it join it — and returns each chain's total bytes,
// newest chain first. The chain is the unit restore depth is measured in: keeping one
// more cycle back means keeping one more chain whole.
func chainSizes(arcs []record.Archive) []int64 {
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
	return chains
}

// archivesAsOf filters a medium's archives to those that existed as of day — the
// as-of-date replay both floor-history reconstructions (protectedHistory, tapeHistory)
// are built on.
func archivesAsOf(archives []record.Archive, day time.Time) []record.Archive {
	var asOf []record.Archive
	for _, ar := range archives {
		if !ar.CreatedAt.After(day) {
			asOf = append(asOf, ar)
		}
	}
	return asOf
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
// simulateMedium ages the projected copies exactly as it does routed archives.
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
// per-archive peer of dropRun), so the simulation mirrors per-archive reclamation.
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
