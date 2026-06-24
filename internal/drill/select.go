package drill

import (
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/format"
	"github.com/Niloen/nbackup/internal/restore"
)

// Target is a selected DLE to drill: the point-in-time slot whose restore chain the
// drill will exercise, the chain itself (so the engine need not recompute it), and
// the risk signals that ranked it.
type Target struct {
	DLE      string
	SlotID   string         // newest slot at/before AsOf holding the DLE — the chain's tip
	AsOf     string         // point-in-time drilled (YYYY-MM-DD)
	ChainLen int            // archives in the restore chain (longer chain = more to go wrong)
	FullAge  int            // age in days of the relied-upon full (older = riskier)
	Steps    []restore.Step // the restore chain, full → incrementals, in run order
}

// candidate is a DLE's pre-ranking selection signals.
type candidate struct {
	t         Target
	lastDrill time.Time // zero = never drilled (most urgent)
}

// Select picks the risk-biased subset of DLEs to drill as of asOf: it rotates DLEs
// so each is covered within window (never-drilled and longest-overdue first), and
// prioritizes the longest incremental chains and the oldest fulls still relied upon
// (the most fragile recovery paths). It drills a point-in-time — the newest slot at
// or before asOf for each DLE — not only the latest slot. At most `sample` targets
// are returned (sample <= 0 = every due DLE); a DLE already drilled OK within the
// window is not reselected. The slots must be in run order (oldest first).
func Select(dles []string, slots []*format.Slot, asOf string, ledger *Ledger, window time.Duration, sample int, now time.Time) []Target {
	var due []candidate
	for _, dle := range dles {
		targetSlot := newestSlotForDLE(slots, dle, asOf)
		if targetSlot == "" {
			continue // no recovery point for this DLE as of the date
		}
		steps, err := restore.Chain(slots, dle, targetSlot)
		if err != nil || len(steps) == 0 {
			continue // no full at/before the date — nothing to compose
		}
		if ledger.Drilled(dle, window, now) {
			continue // covered within the window; rotate to the ones that aren't
		}
		rec, _ := ledger.Get(dle)
		due = append(due, candidate{
			t: Target{
				DLE:      dle,
				SlotID:   targetSlot,
				AsOf:     asOf,
				ChainLen: len(steps),
				FullAge:  fullAgeDays(slots, steps[0].SlotID, asOf, now),
				Steps:    steps,
			},
			lastDrill: rec.LastDrill,
		})
	}

	// Rank: never-drilled first, then longest-overdue, then the most fragile chains
	// (longest chain, then oldest full), then by name for stability.
	sort.SliceStable(due, func(i, j int) bool {
		a, b := due[i], due[j]
		an, bn := a.lastDrill.IsZero(), b.lastDrill.IsZero()
		if an != bn {
			return an // never-drilled sorts before ever-drilled
		}
		if !an && !a.lastDrill.Equal(b.lastDrill) {
			return a.lastDrill.Before(b.lastDrill) // older drill first
		}
		if a.t.ChainLen != b.t.ChainLen {
			return a.t.ChainLen > b.t.ChainLen
		}
		if a.t.FullAge != b.t.FullAge {
			return a.t.FullAge > b.t.FullAge
		}
		return a.t.DLE < b.t.DLE
	})

	if sample > 0 && len(due) > sample {
		due = due[:sample]
	}
	out := make([]Target, len(due))
	for i, c := range due {
		out[i] = c.t
	}
	return out
}

// newestSlotForDLE returns the id of the newest slot at or before asOf that holds an
// archive for the DLE, or "" if none. Slots are in run order (oldest first), so the
// last match wins.
func newestSlotForDLE(slots []*format.Slot, dle, asOf string) string {
	id := ""
	for _, s := range slots {
		if s.Date > asOf {
			continue // strictly after the point-in-time
		}
		for _, a := range s.Archives {
			if a.DLE == dle {
				id = s.ID
				break
			}
		}
	}
	return id
}

// fullAgeDays is the age in days of the full the chain relies on, measured to asOf
// (the point being drilled). It falls back to now when asOf does not parse.
func fullAgeDays(slots []*format.Slot, fullSlotID, asOf string, now time.Time) int {
	var fullDate time.Time
	for _, s := range slots {
		if s.ID == fullSlotID {
			fullDate, _ = format.ParseDateField(s.Date)
			break
		}
	}
	if fullDate.IsZero() {
		return 0
	}
	ref, err := format.ParseDateField(asOf)
	if err != nil {
		ref = now
	}
	d := int(ref.Sub(fullDate).Hours() / 24)
	if d < 0 {
		d = 0
	}
	return d
}
