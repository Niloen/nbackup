// Package policy expresses NBackup's retention safety floor — the rules that
// gate capacity-driven reclamation. It computes the set of "protected" slots
// that reclamation must never touch: the last recovery path for any DLE, and
// slots younger than the medium's minimum age. It is pure and performs no I/O.
//
// Retention is per-medium: callers pass the slots of a single medium, so "last
// recovery path" is judged within that medium alone. A copy on another medium
// never makes a slot reclaimable here — double storage exists for redundancy,
// and each medium retains against its own capacity and cycle. The rule's shape
// is medium-neutral; only the slot set it is applied to is medium-scoped.
package policy

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/slot"
)

// Protected returns a map of slotID -> reason for slots that must never be
// reclaimed. A slot is protected if it is younger than minAge, or if any DLE it
// holds has no newer full backup among the given slots (so within this slot set
// it is that DLE's last recovery path). Pass one medium's slots to get that
// medium's retention floor.
//
// Note: once verification status is tracked, the successor requirement should
// tighten from "a newer full exists" to "a newer verified full exists".
func Protected(slots []*slot.Slot, minAge time.Duration, now time.Time) map[string]string {
	protected := map[string]string{}
	for _, s := range slots {
		date, _ := slot.ParseDateField(s.Date)
		if minAge > 0 && now.Sub(date) < minAge {
			protected[s.ID] = fmt.Sprintf("within minimum age (%s)", minAge)
			continue
		}
		for _, a := range s.Archives {
			if !hasNewerFull(slots, a.DLE, s) {
				protected[s.ID] = fmt.Sprintf("last recovery path for DLE %s", a.DLE)
				break
			}
		}
	}
	return protected
}

// ProtectedOn reports the first slot residing on a single volume that is in the
// protected set, if any, returning its id and reason. The caller computes
// `protected` over a whole medium (so "a newer full exists" is judged
// medium-wide), then passes the slots that have a part on the one volume being
// considered for reclamation — tape recycling or relabel. Because a spanned slot
// has a placement on every tape it touches, it is reported for each of them:
// reclaiming any one tape would destroy the slot, even the tapes that hold no
// seal record. Shared by the prune/recycle path and `nb label --relabel` so both
// judge a volume's reusability identically.
func ProtectedOn(protected map[string]string, onVolume []*slot.Slot) (slotID, reason string, ok bool) {
	for _, s := range onVolume {
		if r, p := protected[s.ID]; p {
			return s.ID, r, true
		}
	}
	return "", "", false
}

func hasNewerFull(slots []*slot.Slot, dle string, target *slot.Slot) bool {
	for _, s := range slots {
		if !slot.Less(target, s) {
			continue // s must come strictly after target in run order
		}
		for _, a := range s.Archives {
			if a.DLE == dle && a.Level == 0 {
				return true
			}
		}
	}
	return false
}
