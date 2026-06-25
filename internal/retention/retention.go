// Package retention judges which backups must be kept. It does not hold a policy:
// the knobs live in config (a medium's minimum_age) and the recovery-chain rule is
// an invariant, not a tunable. Compute applies those rules to one medium's slots at
// a moment in time and returns a Floor — the slots reclamation must never delete
// (slots younger than the medium's minimum age, and every slot in a DLE's live
// recovery chain: its last full plus the later incrementals a restore replays),
// each with the reason it is pinned. Callers build the Floor once and query it,
// rather than threading a raw map around. It is pure and does no I/O.
//
// Retention is per-medium: callers pass the slots of a single medium, so "last
// recovery path" is judged within that medium alone. A copy on another medium
// never makes a slot reclaimable here — double storage exists for redundancy,
// and each medium retains against its own capacity and cycle. The rule's shape
// is medium-neutral; only the slot set it is applied to is medium-scoped.
package retention

import (
	"fmt"
	"sort"
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

// Compute applies a medium's retention rules to its slots and returns the floor —
// the slots reclamation must never delete. Three rules combine:
//
//  1. Age: a slot younger than minAge is kept, whatever its level.
//  2. Last recovery path: the last full of each DLE is kept, so at least one
//     recovery path for it never ages out.
//  3. Recovery chain: an incremental restore replays its full PLUS every later
//     incremental up to the target (see restore.Chain), so a kept slot pins the
//     whole chain its restore needs. Each DLE's latest slot pins the live chain
//     after the last full (the tip and every point in between); each young slot
//     pins the older base its restore depends on. So reclamation can never orphan
//     an incremental or break a chain it leaves restorable — an incremental is
//     kept because a chain needs it, never on its own.
//
// Pass one medium's slots to get that medium's floor.
//
// Note: once verification status is tracked, the successor requirement should
// tighten from "a newer full exists" to "a newer verified full exists".
func Compute(slots []*format.Slot, minAge time.Duration, now time.Time) Floor {
	reasons := map[string]string{}
	young := func(s *format.Slot) bool {
		if minAge <= 0 {
			return false
		}
		date, _ := format.ParseDateField(s.Date)
		return now.Sub(date) < minAge
	}
	// 1) Age floor.
	for _, s := range slots {
		if young(s) {
			reasons[s.ID] = fmt.Sprintf("within minimum age (%s)", sizeutil.FormatDuration(minAge))
		}
	}
	// 2) Last-recovery floor (kept distinct so a slot that holds a DLE's last full
	// is reported by that full, not a mere incremental it also carries).
	for _, s := range slots {
		if _, ok := reasons[s.ID]; ok {
			continue
		}
		for _, a := range s.Archives {
			if a.Level == 0 && !hasNewerFull(slots, a.DLE, s) {
				reasons[s.ID] = fmt.Sprintf("last recovery path for DLE %s", a.DLE)
				break
			}
		}
	}
	// 3) Recovery-chain floor.
	for _, dle := range dleNames(slots) {
		ds := slotsWith(slots, dle)
		anchors := map[int]bool{}
		if n := len(ds); n > 0 {
			anchors[n-1] = true // the latest slot: keeps the live chain (and its full)
		}
		for i, s := range ds {
			if young(s) {
				anchors[i] = true // a recent slot: keep the base its restore needs
			}
		}
		for ai := range anchors {
			full := -1
			for j := 0; j <= ai; j++ {
				if hasFull(ds[j], dle) {
					full = j
				}
			}
			if full < 0 {
				continue // no full at or before the anchor (cannot happen for a real chain)
			}
			for j := full; j <= ai; j++ {
				if _, ok := reasons[ds[j].ID]; !ok {
					reasons[ds[j].ID] = fmt.Sprintf("in DLE %s's recovery chain", dle)
				}
			}
		}
	}
	return Floor{reasons: reasons}
}

// dleNames returns the distinct DLEs across the slots, sorted for determinism.
func dleNames(slots []*format.Slot) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range slots {
		for _, a := range s.Archives {
			if !seen[a.DLE] {
				seen[a.DLE] = true
				out = append(out, a.DLE)
			}
		}
	}
	sort.Strings(out)
	return out
}

// slotsWith returns the slots holding an archive of dle, in run order.
func slotsWith(slots []*format.Slot, dle string) []*format.Slot {
	var out []*format.Slot
	for _, s := range slots {
		if hasArchive(s, dle) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return format.Less(out[i], out[j]) })
	return out
}

func hasArchive(s *format.Slot, dle string) bool {
	for _, a := range s.Archives {
		if a.DLE == dle {
			return true
		}
	}
	return false
}

func hasFull(s *format.Slot, dle string) bool {
	for _, a := range s.Archives {
		if a.DLE == dle && a.Level == 0 {
			return true
		}
	}
	return false
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
