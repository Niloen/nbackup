// Package policy expresses NBackup's retention rules, analogous to Amanda's
// Policy. It decides which slots may be retired while preserving recoverability
// and cycle safety, and reports budget status. It is pure: it operates on slot
// metadata and returns decisions, performing no I/O.
package policy

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/slot"
)

// Policy holds the configured retention rules.
type Policy struct {
	MinimumAge time.Duration // a slot younger than this is never retired
	Budget     int64         // target ceiling in bytes (0 = unset)
}

// Decision is a per-slot prune verdict.
type Decision struct {
	Slot   *slot.Slot
	Delete bool
	Reason string
}

// Prune evaluates every slot and returns a decision for each. A slot is
// deletable only when it is older than MinimumAge AND every DLE it holds has a
// newer full backup elsewhere — so the last valid recovery path is preserved.
func (p Policy) Prune(slots []*slot.Slot, now time.Time) []Decision {
	decisions := make([]Decision, 0, len(slots))
	for _, s := range slots {
		decisions = append(decisions, p.evaluate(s, slots, now))
	}
	return decisions
}

func (p Policy) evaluate(target *slot.Slot, all []*slot.Slot, now time.Time) Decision {
	date, _ := slot.ParseDateField(target.Date)
	if p.MinimumAge > 0 && now.Sub(date) < p.MinimumAge {
		return Decision{Slot: target, Reason: fmt.Sprintf("within minimum age (%s)", p.MinimumAge)}
	}
	for _, a := range target.Archives {
		if !hasNewerFull(all, a.DLE, target) {
			return Decision{Slot: target, Reason: fmt.Sprintf("no newer full for DLE %s (last recovery path)", a.DLE)}
		}
	}
	return Decision{Slot: target, Delete: true, Reason: "outside cycle; newer recovery path exists"}
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

// BudgetStatus reports whether current usage exceeds the budget and the percent
// used. Percent is 0 when no budget is set.
func (p Policy) BudgetStatus(currentBytes int64) (over bool, pct float64) {
	if p.Budget <= 0 {
		return false, 0
	}
	return currentBytes > p.Budget, float64(currentBytes) / float64(p.Budget) * 100
}
