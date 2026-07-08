package accounting

import (
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/retention"
)

// VolumeExpectation describes the volume the next run on a labeled medium will
// write to. It is derived from the catalog's volume registry and the retention
// policy, never from a physical scan: for a one-run-per-tape (non-appendable) medium it names the
// oldest reusable volume the run would recycle, or a fresh tape when none is
// reusable; for an appendable medium it names the current volume the run extends.
type VolumeExpectation struct {
	Medium      string    // the labeled medium this expectation is for
	Appendable  bool      // true: extend a volume; false: one run per tape (recycle/fresh)
	Label       string    // the expected volume's label; "" when a fresh tape is expected
	WrittenAt   time.Time // when that volume was last labeled (zero for a fresh tape)
	Recycles    int       // runs on it the next run would overwrite (non-appendable reuse)
	FreshVolume bool      // no reusable volume exists — a fresh/blank tape is expected
	VolumeBytes int64     // the reel's physical capacity (volume_size); 0 = unknown/unsized
	UsedBytes   int64     // bytes already on the expected reel (0 for a fresh/recycled reel)
}

// ExpectedVolume reports the tape the next run on the landing medium will write to,
// or ok=false for address-identified media (disk, s3) that carry no label and so
// have no tape to expect. It is the landing shortcut over ExpectedVolumeFor.
func (a *Accountant) ExpectedVolume(now time.Time) (VolumeExpectation, bool) {
	if !a.d.LandingLabeled() {
		return VolumeExpectation{}, false
	}
	return a.ExpectedVolumeFor(a.d.Landing, now), true
}

// ExpectedVolumeFor computes the expected volume for a labeled medium from the
// catalog's volume registry ordered oldest-written-first. A
// non-appendable run reuses the oldest volume whose every run is unprotected (the
// retention safety floor: past minimum age, with a newer recovery path); an
// appendable run extends the most recently written volume in the pool. The reel's
// capacity and current fill (VolumeBytes/UsedBytes) bound the run physically: an
// appendable run extends the latest reel (room = size - used), a fresh or recycled
// reel offers a whole reel (used stays 0).
func (a *Accountant) ExpectedVolumeFor(medium string, now time.Time) VolumeExpectation {
	def := a.d.Cfg.Media[medium]
	exp := VolumeExpectation{Medium: medium, Appendable: def.IsAppendable()}
	if prof, err := a.ProfileFor(medium); err == nil {
		exp.VolumeBytes = prof.VolumeSize()
	}

	// volumesInPool returns the same pool sorted by name; this expectation wants
	// oldest-written-first, so copy and re-sort rather than duplicate the compress.
	pool := append([]catalog.VolumeRecord(nil), a.volumesInPool(medium)...)
	sort.Slice(pool, func(i, j int) bool { return pool[i].Label.WrittenAt.Before(pool[j].Label.WrittenAt) })

	if exp.Appendable {
		if n := len(pool); n > 0 {
			exp.Label, exp.WrittenAt = pool[n-1].Label.Name, pool[n-1].Label.WrittenAt
			// The reel's stored fill — the same figure the librarian's Remaining()
			// starts from, so the plan and the write agree.
			exp.UsedBytes = pool[n-1].Used
		} else {
			exp.FreshVolume = true
		}
		return exp
	}

	minAge := a.d.Cfg.MinAgeFor(def)
	// Retention is per-medium: a volume is reusable only when this medium no
	// longer needs its runs, so protection is computed over this medium's own
	// runs. Scoping to all media's runs would recycle a tape merely
	// because a newer full landed on disk — discarding the offsite copy and the
	// redundancy double storage exists to provide.
	floor := retention.Compute(a.d.Cat.ArchivesOn(medium), a.d.Cat.Archives(), minAge, now)
	for _, v := range pool {
		held := a.d.Cat.RunIDsOnLabel(v.Label.Name)
		if _, _, ok := floor.First(held); ok {
			continue // some run on this tape is still kept — not reusable
		}
		exp.Label, exp.WrittenAt, exp.Recycles = v.Label.Name, v.Label.WrittenAt, len(held)
		return exp
	}
	exp.FreshVolume = true // nothing reusable — the run needs a fresh tape
	return exp
}

// CapacityRoom is the hard per-run write ceiling fed to the planner: the most a
// single run may write. It is the tighter of two independent bounds — the pool's
// free room (retention: capacity minus the protected set, the bytes pruning
// cannot reclaim) and the landing volume's remaining room (physical: a run fills
// the reel it appends to before spilling to the next). Either is unbounded (-1)
// on media that lack it — object stores have no reel, a bare drive has no bounded
// pool — and the result is unbounded only when both are.
func (a *Accountant) CapacityRoom(now time.Time) int64 {
	return minRoom(a.PoolRoom(now), a.volumeRoom(now))
}

// volumeRoom is the physical bound: the bytes left on the reel the run lands on
// before it spills to the next. An appendable run extends the latest reel, so its
// room is volume_size minus what is already on it; a fresh or recycled reel
// offers a whole volume_size. Negative = unbounded (the medium has no reel size).
func (a *Accountant) volumeRoom(now time.Time) int64 {
	exp, ok := a.ExpectedVolume(now)
	if !ok || exp.VolumeBytes <= 0 {
		return -1
	}
	if room := exp.VolumeBytes - exp.UsedBytes; room > 0 {
		return room
	}
	return 0
}

// minRoom returns the tighter of two per-run ceilings, treating negative as
// unbounded (no bound from that source); the result is unbounded only when both
// inputs are.
func minRoom(a, b int64) int64 {
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}
