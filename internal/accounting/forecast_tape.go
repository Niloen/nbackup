package accounting

import (
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/retention"
)

// tapeHistoryDays is how far the cartridge-usage curve reconstructs backward. It only
// needs to span the retention window — older cartridges have recycled and dropped out of
// the catalog (their runs aged out by definition) — so a season covers a monthly-full
// rotation comfortably.
const tapeHistoryDays = 60

// forecastVolumes builds a tape pool's cartridge-usage curve: RECORDED history
// reconstructed from the catalog (how many cartridges held a retention-protected run on
// each past day), then a forward PACKING + RECYCLING simulation. The two meet at `start`
// ("now") as one continuous series — the volume-structured peer of forecastMedium's byte
// curve. A cartridge is "in use" on a day exactly when the retention Floor keeps at least
// one of its runs; when the Floor clears them all it is recyclable (the write path's own
// rule — see librarian.protectedRun / ExpectedVolumeFor), so it drops out of the count.
func (a *Accountant) forecastVolumes(name string, prof media.Profile, start time.Time, plans []*planner.Plan) []VolumePoint {
	usable := prof.VolumeSize()
	if v := prof.Volumes(); v > 0 {
		usable = prof.TotalBytes() / v // TotalBytes = volumes × usable, so this nets the framing overhead
	}
	if usable <= 0 {
		return nil
	}
	minAge := a.d.Cfg.MinAgeFor(a.d.Cfg.Media[name])
	pts := a.tapeHistory(name, minAge, start)
	return append(pts, a.tapeProjection(name, minAge, usable, start, plans)...)
}

// tapeHistory reconstructs cartridges-in-use for each of the last tapeHistoryDays: for
// each past day it recomputes the retention Floor as of that day (over the archives that
// existed then) and counts the pool's current cartridges holding a run the Floor keeps.
// Cartridges that have since recycled are gone from the catalog, but their runs had aged
// out — so the count is faithful across the retention window, which is the part that
// determines how many tapes the rotation needs.
func (a *Accountant) tapeHistory(name string, minAge time.Duration, now time.Time) []VolumePoint {
	onMedium := a.d.Cat.ArchivesOn(name)
	if len(onMedium) == 0 {
		return nil
	}
	runDate := map[string]time.Time{} // when each run first landed on this medium
	for _, ar := range onMedium {
		if t, ok := runDate[ar.Run]; !ok || ar.CreatedAt.Before(t) {
			runDate[ar.Run] = ar.CreatedAt
		}
	}
	type vol struct {
		runs      []string
		writtenAt time.Time
	}
	var vols []vol
	for _, vr := range a.d.Cat.Volumes() {
		if vr.Label.Pool != name {
			continue
		}
		vols = append(vols, vol{runs: a.d.Cat.RunIDsOnLabel(vr.Label.Name), writtenAt: vr.Label.WrittenAt})
	}
	if len(vols) == 0 {
		return nil
	}
	pts := make([]VolumePoint, 0, tapeHistoryDays)
	for d := tapeHistoryDays - 1; d >= 0; d-- {
		day := now.AddDate(0, 0, -d)
		var asOf []record.Archive
		for _, ar := range onMedium {
			if !ar.CreatedAt.After(day) {
				asOf = append(asOf, ar)
			}
		}
		floor := retention.Compute(asOf, asOf, minAge, day)
		var inUse int64
		for _, v := range vols {
			if v.writtenAt.After(day) {
				continue // cartridge not yet written on `day`
			}
			for _, r := range v.runs {
				if t, ok := runDate[r]; ok && !t.After(day) && floor.Keeps(r) {
					inUse++
					break
				}
			}
		}
		pts = append(pts, VolumePoint{Date: record.DateString(day), InUse: inUse})
	}
	return pts
}

// tapeProjection simulates the pool forward: it seeds reels from the current cartridges
// (each with its remaining room and the runs it holds), then packs each simulated day's
// routed archives onto the open reel — spilling to a fresh reel when one fills (runs span
// volumes), or starting a fresh reel per run on a non-appendable medium — and counts the
// reels the retention Floor still keeps a run on. As runs age past minimum_age and are
// superseded the Floor releases their reels, so the count is the concurrent cartridges the
// rotation needs, whether or not a freed reel is physically relabeled.
func (a *Accountant) tapeProjection(name string, minAge time.Duration, usable int64, start time.Time, plans []*planner.Plan) []VolumePoint {
	appendable := a.MediumAppendable(name)
	limit, capped := a.mediumRunCap(name)
	type reel struct {
		remaining int64
		runs      map[string]bool
	}
	var reels []reel
	for _, vr := range a.d.Cat.Volumes() {
		if vr.Label.Pool != name {
			continue
		}
		rs := map[string]bool{}
		for _, r := range a.d.Cat.RunIDsOnLabel(vr.Label.Name) {
			rs[r] = true
		}
		rem := usable - vr.Used
		if rem < 0 {
			rem = 0
		}
		reels = append(reels, reel{remaining: rem, runs: rs})
	}
	sim := append([]record.Archive(nil), a.d.Cat.ArchivesOn(name)...)

	// pack places a run's bytes onto reels: a fresh reel for a non-appendable run, else the
	// open (last) reel, spilling to new reels as each fills. A zero-byte incremental still
	// pins whatever reel it lands on (it is a run there for retention).
	pack := func(runID string, bytes int64, freshRun bool) {
		if len(reels) == 0 || (freshRun && !appendable) {
			reels = append(reels, reel{remaining: usable, runs: map[string]bool{}})
		}
		reels[len(reels)-1].runs[runID] = true
		for bytes > 0 {
			if reels[len(reels)-1].remaining <= 0 {
				reels = append(reels, reel{remaining: usable, runs: map[string]bool{}})
			}
			r := &reels[len(reels)-1]
			put := bytes
			if put > r.remaining {
				put = r.remaining
			}
			r.remaining -= put
			bytes -= put
			r.runs[runID] = true
		}
	}

	pts := make([]VolumePoint, 0, len(plans))
	for i, plan := range plans {
		date := start.AddDate(0, 0, i)
		runID := record.IDFromTime(date)
		fresh := true
		for _, it := range plan.Items {
			if !contains(a.copyMediaFor(it.DLE.DumpTypeName()), name) {
				continue // this medium neither routes nor receives a sync copy of this DLE
			}
			pack(runID, it.EstBytes, fresh)
			fresh = false
			sim = append(sim, record.Archive{Run: runID, DLE: it.Name, Level: it.Level, Compressed: it.EstBytes, CreatedAt: date})
		}
		kept := sim
		if capped { // a last-N sync tape counts only its most recent runs as in use
			kept = capRuns(sim, limit)
		}
		floor := retention.Compute(kept, kept, minAge, date)
		var inUse int64
		for _, rl := range reels {
			for r := range rl.runs {
				if floor.Keeps(r) {
					inUse++
					break
				}
			}
		}
		pts = append(pts, VolumePoint{Date: record.DateString(date), InUse: inUse})
	}
	return pts
}
