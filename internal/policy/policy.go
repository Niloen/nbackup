// Package policy expresses NBackup's cross-cutting retention safety — the rules
// that hold regardless of which medium a slot lives on. It computes the set of
// "protected" slots that capacity-driven reclamation (a per-medium concern) must
// never touch: the last recovery path for any DLE, and slots younger than the
// medium's minimum age. It is pure and performs no I/O.
package policy

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/slot"
)

// Protected returns a map of slotID -> reason for slots that must never be
// reclaimed. A slot is protected if it is younger than minAge, or if any DLE it
// holds has no newer full backup elsewhere (so it is that DLE's last recovery
// path).
//
// requireVerifiedSuccessor is reserved: today the existence of a newer full is
// the successor requirement; once verification status is tracked it will further
// require that successor to be verified.
func Protected(slots []*slot.Slot, minAge time.Duration, now time.Time, requireVerifiedSuccessor bool) map[string]string {
	_ = requireVerifiedSuccessor
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
