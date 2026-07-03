package accounting

// Capacity arithmetic. The queries here sit on three axes the callers keep distinct:
//   - scope: the landing medium only (Capacity/CapacityStatus/StoredBytes/PoolRoom)
//     vs any configured medium by name (the Medium*OverCapacity family);
//   - the byte figure: raw used (MediumOverCapacity), the protected residual a prune
//     cannot reclaim (MediumProtectedOverCapacity / PoolRoom), or a projected total
//     after a pending write (ProjectedOverCapacity);
//   - 0 capacity always means unbounded, never "full".
//
// The two "protected bytes" figures are deliberately different quantities:
// MediumProtectedOverCapacity is the residual left after a capacity-fitting prune
// (current total minus what Reclaim would free — which may leave unprotected bytes
// a prune had no need to delete), while PoolRoom counts only the floor-pinned bytes
// themselves (the hard minimum no prune can ever go below).

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/retention"
)

// Capacity returns the landing medium's total retainable bytes (0 = unbounded).
func (a *Accountant) Capacity() int64 { return a.d.LandingProfile.TotalBytes() }

// CapacityStatus reports whether current usage exceeds capacity and the percent
// used (0 when unbounded).
func (a *Accountant) CapacityStatus(current int64) (over bool, pct float64) {
	c := a.d.LandingProfile.TotalBytes()
	if c <= 0 {
		return false, 0
	}
	return current > c, float64(current) / float64(c) * 100
}

// StoredBytes is the bytes currently stored on the engine's own medium.
func (a *Accountant) StoredBytes() int64 { return a.d.Cat.MediumBytes(a.d.Landing) }

// MediumOverCapacity reports whether the named medium still holds more than its
// capacity (a 0 capacity means unbounded). used and capacity are returned for
// messaging — used after a prune to tell the operator that reclaiming every dead
// archive was not enough because the protected recovery set alone exceeds capacity.
func (a *Accountant) MediumOverCapacity(name string) (over bool, used, capacity int64, err error) {
	prof, err := a.ProfileFor(name)
	if err != nil {
		return false, 0, 0, err
	}
	capacity = prof.TotalBytes()
	used = a.d.Cat.MediumBytes(name)
	return capacity > 0 && used > capacity, used, capacity, nil
}

// MediumProtectedOverCapacity reports whether the bytes a prune *cannot* reclaim —
// the protected recovery set — still exceed the medium's capacity. It subtracts
// everything Reclaim would free from the current total, so the answer is the same
// whether or not a real prune has run: a dry-run still sees the would-delete archives
// in the catalog while a completed prune has already removed them, but
// `residual = current − reclaimable` is identical either way (after a real prune the
// reclaimable set is empty and the current total is already the residual). This is
// what `nb prune` warns on, so its preview and its real run agree.
func (a *Accountant) MediumProtectedOverCapacity(name string, now time.Time) (over bool, residual, capacity int64, err error) {
	prof, err := a.ProfileFor(name)
	if err != nil {
		return false, 0, 0, err
	}
	def, ok := a.d.Cfg.Media[name]
	if !ok {
		return false, 0, 0, fmt.Errorf("unknown medium %q", name)
	}
	capacity = prof.TotalBytes()
	archives := a.d.Cat.ArchivesOn(name)
	floor := retention.Compute(archives, a.d.Cfg.MinAgeFor(def), now)
	var reclaimable int64
	for _, r := range prof.Reclaim(archives, floor, now) {
		reclaimable += r.Bytes
	}
	residual = a.d.Cat.MediumBytes(name) - reclaimable
	return capacity > 0 && residual > capacity, residual, capacity, nil
}

// MediumProtectionIsAgeBound reports whether every archive pinning the medium over
// capacity is held by the minimum_age floor (vs a live recovery chain). When false,
// advising the operator to shorten minimum_age is useless — a DLE's last full and its
// later incrementals are pinned regardless of age — so the remedy text drops it.
func (a *Accountant) MediumProtectionIsAgeBound(name string, now time.Time) bool {
	def, ok := a.d.Cfg.Media[name]
	if !ok {
		return true
	}
	archives := a.d.Cat.ArchivesOn(name)
	floor := retention.Compute(archives, a.d.Cfg.MinAgeFor(def), now)
	for _, ar := range archives {
		kind, ok := floor.KindArchive(ar.Run, ar.DLE)
		if ok && kind != retention.KindAge {
			return false // a recovery-chain pin that shortening minimum_age can't release
		}
	}
	return true
}

// ProjectedOverCapacity reports whether the named medium would exceed its capacity
// after add more bytes land on it (a 0 capacity means unbounded) — the check
// `nb copy` runs before/after a copy so it warns about overshooting a target's
// budget the way `nb sync` already does.
func (a *Accountant) ProjectedOverCapacity(name string, add int64) (over bool, projected, capacity int64, err error) {
	prof, err := a.ProfileFor(name)
	if err != nil {
		return false, 0, 0, err
	}
	capacity = prof.TotalBytes()
	projected = a.d.Cat.MediumBytes(name) + add
	return capacity > 0 && projected > capacity, projected, capacity, nil
}

// PoolRoom is the retention bound: capacity minus the bytes pruning cannot
// reclaim (the protected set). Negative = unbounded (no pool budget).
func (a *Accountant) PoolRoom(now time.Time) int64 {
	capacity := a.d.LandingProfile.TotalBytes()
	if capacity <= 0 {
		return -1
	}
	archives := a.d.Cat.ArchivesOn(a.d.Landing)
	floor := retention.Compute(archives, a.d.LandingMinAge, now)
	var keptBytes int64
	for _, ar := range archives {
		if floor.KeepsArchive(ar.Run, ar.DLE) {
			keptBytes += ar.Compressed
		}
	}
	if room := capacity - keptBytes; room > 0 {
		return room
	}
	return 0
}
