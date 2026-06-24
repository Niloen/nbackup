// Package retention judges which backups must be kept. It does not hold a policy:
// the knobs live in config (a medium's minimum_age) and the last-recovery-path
// rule is an invariant, not a tunable. Compute applies those rules to one medium's
// slots at a moment in time and returns a Floor — the slots reclamation must never
// delete (the last recovery path for any DLE, and slots younger than the medium's
// minimum age), each with the reason it is pinned. Callers build the Floor once
// and query it, rather than threading a raw map around. It is pure and does no I/O.
//
// Retention is per-medium: callers pass the slots of a single medium, so "last
// recovery path" is judged within that medium alone. A copy on another medium
// never makes a slot reclaimable here — double storage exists for redundancy,
// and each medium retains against its own capacity and cycle. The rule's shape
// is medium-neutral; only the slot set it is applied to is medium-scoped.
package retention

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/format"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// Floor is the retention floor computed for one medium's slots: the slots
// reclamation must never delete, each with the reason it is pinned. Build it once
// with Compute, then query it — by slot (Keeps, Reason) or by "is any of these
// slots pinned" (First). The zero Floor keeps nothing.
type Floor struct {
	reasons map[string]string // slotID -> reason; absent ⇒ reclaimable
}

// Compute applies a medium's retention rules to its slots. A slot is kept if it
// is younger than minAge, or if any DLE it holds has no newer full backup among
// the given slots (so within this slot set it is that DLE's last recovery path).
// Pass one medium's slots to get that medium's floor.
//
// Note: once verification status is tracked, the successor requirement should
// tighten from "a newer full exists" to "a newer verified full exists".
func Compute(slots []*format.Slot, minAge time.Duration, now time.Time) Floor {
	reasons := map[string]string{}
	for _, s := range slots {
		date, _ := format.ParseDateField(s.Date)
		if minAge > 0 && now.Sub(date) < minAge {
			reasons[s.ID] = fmt.Sprintf("within minimum age (%s)", sizeutil.FormatDuration(minAge))
			continue
		}
		for _, a := range s.Archives {
			// Only a *full* makes this slot a DLE's last recovery path: an
			// incremental is useless without a base, so an incremental-only slot is
			// never the recovery floor. Without this guard a tip incremental whose
			// base full is older would be pinned forever (and the reason would name
			// whichever archive happened to be checked first, not the protecting one).
			if a.Level == 0 && !hasNewerFull(slots, a.DLE, s) {
				reasons[s.ID] = fmt.Sprintf("last recovery path for DLE %s", a.DLE)
				break
			}
		}
	}
	return Floor{reasons: reasons}
}

// Keeps reports whether the floor pins slot id (so reclamation must not delete
// it). It is the predicate a medium's Reclaim consults; callers that also want
// the message use Reason instead.
func (f Floor) Keeps(id string) bool {
	_, ok := f.reasons[id]
	return ok
}

// Reason returns why the floor pins slot id, and whether it pins it at all.
func (f Floor) Reason(id string) (reason string, ok bool) {
	r, ok := f.reasons[id]
	return r, ok
}

// First returns the first of slots that the floor pins, with the reason — the
// medium-wide floor projected onto one volume's slots. The caller computes the
// Floor over a whole medium (so "a newer full exists" is judged medium-wide),
// then passes the slots that have a part on the one volume being considered for
// reclamation — tape recycling or relabel. Because a spanned slot has a placement
// on every tape it touches, it is reported for each of them: reclaiming any one
// tape would destroy the slot, even the tapes that hold no seal record. Shared by
// the prune/recycle path and `nb label --relabel` so both judge a volume's
// reusability identically.
func (f Floor) First(slots []*format.Slot) (slotID, reason string, ok bool) {
	for _, sl := range slots {
		if r, p := f.reasons[sl.ID]; p {
			return sl.ID, r, true
		}
	}
	return "", "", false
}

func hasNewerFull(slots []*format.Slot, dle string, target *format.Slot) bool {
	for _, s := range slots {
		if !format.Less(target, s) {
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
