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
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// archiveRef identifies one archive within the floor: a slot and the DLE whose
// image it holds. A run dumps each DLE once at one level, so (slot, DLE) names an
// archive uniquely — the floor pins protection at this granularity, finer than the
// slot, so an old slot may keep one DLE's archive while another DLE's is reclaimed.
type archiveRef struct{ slot, dle string }

// Floor is the retention floor computed for one medium's slots: the archives
// reclamation must never delete, each with the reason it is pinned. Build it once
// with Compute, then query it — per archive (KeepsArchive, ReasonArchive), per slot
// (Keeps, Reason — "is any archive of the slot pinned"), or by "is any of these
// slots pinned" (First). The zero Floor keeps nothing.
//
// The floor is per-archive because reclamation on address-identified media (disk,
// cloud) is per-archive; the slot-level queries report a slot as kept when any of
// its archives is pinned, which is what the whole-volume reclaimers (tape relabel,
// ExpectedTape) and the cost forecast still reason in.
type Floor struct {
	reasons map[archiveRef]string // (slot,DLE) -> reason; absent ⇒ reclaimable
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
func Compute(slots []*record.Slot, minAge time.Duration, now time.Time) Floor {
	reasons := map[archiveRef]string{}
	pin := func(slot, dle, reason string) {
		if _, ok := reasons[archiveRef{slot, dle}]; !ok {
			reasons[archiveRef{slot, dle}] = reason
		}
	}
	young := func(s *record.Slot) bool {
		if minAge <= 0 {
			return false
		}
		date, _ := record.ParseDateField(s.Date)
		return now.Sub(date) < minAge
	}
	// 1) Age floor: a young slot pins every archive it carries.
	for _, s := range slots {
		if young(s) {
			for _, a := range s.Archives {
				pin(s.ID, a.DLE, fmt.Sprintf("within minimum age (%s)", sizeutil.FormatDuration(minAge)))
			}
		}
	}
	// 2) Last-recovery floor (kept distinct so an archive that is a DLE's last full
	// is reported by that full, not a mere incremental the slot also carries).
	for _, s := range slots {
		for _, a := range s.Archives {
			if a.Level == 0 && !hasNewerFull(slots, a.DLE, s) {
				pin(s.ID, a.DLE, fmt.Sprintf("last recovery path for DLE %s", a.DLE))
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
				pin(ds[j].ID, dle, fmt.Sprintf("in DLE %s's recovery chain", dle))
			}
		}
	}
	return Floor{reasons: reasons}
}

// dleNames returns the distinct DLEs across the slots, sorted for determinism.
func dleNames(slots []*record.Slot) []string {
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
func slotsWith(slots []*record.Slot, dle string) []*record.Slot {
	var out []*record.Slot
	for _, s := range slots {
		if hasArchive(s, dle) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return record.Less(out[i], out[j]) })
	return out
}

func hasArchive(s *record.Slot, dle string) bool {
	for _, a := range s.Archives {
		if a.DLE == dle {
			return true
		}
	}
	return false
}

func hasFull(s *record.Slot, dle string) bool {
	for _, a := range s.Archives {
		if a.DLE == dle && a.Level == 0 {
			return true
		}
	}
	return false
}

// KeepsArchive reports whether the floor pins one archive (slot+DLE), so per-archive
// reclamation must not delete it. It is the predicate a medium's Reclaim consults.
func (f Floor) KeepsArchive(slot, dle string) bool {
	_, ok := f.reasons[archiveRef{slot, dle}]
	return ok
}

// ReasonArchive returns why the floor pins one archive, and whether it pins it.
func (f Floor) ReasonArchive(slot, dle string) (reason string, ok bool) {
	r, ok := f.reasons[archiveRef{slot, dle}]
	return r, ok
}

// Keeps reports whether the floor pins any archive of slot id — the slot-level view
// the whole-volume reclaimers (tape relabel, ExpectedTape) and the cost forecast
// reason in: a slot is kept if reclaiming it would destroy any pinned archive.
func (f Floor) Keeps(id string) bool {
	_, ok := f.Reason(id)
	return ok
}

// Reason returns why the floor pins slot id, and whether it pins any archive at all.
// When several archives pin the slot it reports the strongest reason — age, then a
// DLE's last recovery path (its full), then a recovery chain — so a slot that holds a
// DLE's last full is reported by that full, not by a mere incremental it also carries
// (the precedence Compute applies per archive, projected to the slot). Ties within a
// rank break by DLE for a stable message.
func (f Floor) Reason(id string) (reason string, ok bool) {
	bestRank, bestDLE := 0, ""
	for ref, r := range f.reasons {
		if ref.slot != id {
			continue
		}
		rk := reasonRank(r)
		if !ok || rk < bestRank || (rk == bestRank && ref.dle < bestDLE) {
			bestRank, bestDLE, reason, ok = rk, ref.dle, r, true
		}
	}
	return reason, ok
}

// reasonRank orders the floor's reason kinds by strength (lower = stronger), so the
// slot-level Reason reports the same precedence Compute uses per archive.
func reasonRank(reason string) int {
	switch {
	case strings.HasPrefix(reason, "within minimum age"):
		return 0
	case strings.HasPrefix(reason, "last recovery path"):
		return 1
	default: // recovery chain
		return 2
	}
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
func (f Floor) First(slots []*record.Slot) (slotID, reason string, ok bool) {
	for _, sl := range slots {
		if r, p := f.Reason(sl.ID); p {
			return sl.ID, r, true
		}
	}
	return "", "", false
}

func hasNewerFull(slots []*record.Slot, dle string, target *record.Slot) bool {
	for _, s := range slots {
		if !record.Less(target, s) {
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
