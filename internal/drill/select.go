package drill

import (
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
)

// Target is a selected DLE to drill: the point-in-time run whose restore chain the
// drill will exercise, the chain itself (so the engine need not recompute it), and
// the risk signals that ranked it.
type Target struct {
	DLE      string
	RunID    string          // newest run at/before AsOf holding the DLE — the chain's tip
	AsOf     string          // point-in-time drilled (YYYY-MM-DD)
	ChainLen int             // archives in the restore chain (longer chain = more to go wrong)
	FullAge  int             // age in days of the relied-upon full (older = riskier)
	Steps    []recovery.Step // the restore chain, full → incrementals, in run order
}

// candidate is a DLE's pre-ranking selection signals.
type candidate struct {
	t         Target
	lastDrill time.Time // zero = never drilled (most urgent)
}

// Select picks the risk-biased subset of DLEs to drill as of asOf: it rotates DLEs
// so each is covered within window (never-drilled and longest-overdue first), and
// prioritizes the longest incremental chains and the oldest fulls still relied upon
// (the most fragile recovery paths). It drills a point-in-time — the newest run at
// or before asOf for each DLE — not only the latest run. At most `sample` targets
// are returned (sample <= 0 = every due DLE); a DLE already drilled OK within the
// window is not reselected. The runs must be in run order (oldest first).
func Select(dles []string, archives []record.Archive, asOf string, ledger *Ledger, window time.Duration, sample int, now time.Time) []Target {
	var due []candidate
	for _, dle := range dles {
		targetRun := newestRunForDLE(archives, dle, asOf)
		if targetRun == "" {
			continue // no recovery point for this DLE as of the date
		}
		steps, err := recovery.Chain(archives, dle, targetRun)
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
				RunID:    targetRun,
				AsOf:     asOf,
				ChainLen: len(steps),
				FullAge:  fullAgeDays(steps[0].RunID, asOf, now),
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

// newestRunForDLE returns the id of the newest run at or before asOf that holds an
// archive for the DLE, or "" if none. It defers to recovery.AsOf over the DLE's own
// archives, so a drill's as-of point (date, or date+time) resolves identically to a
// restore's.
func newestRunForDLE(archives []record.Archive, dle, asOf string) string {
	id, err := recovery.AsOf(record.ArchivesOf(archives, dle), asOf)
	if err != nil {
		return "" // no recovery point at or before asOf (or an unparsable as-of)
	}
	return id
}

// fullAgeDays is the age in days of the full the chain relies on, measured to asOf
// (the point being drilled). It falls back to now when asOf does not parse.
func fullAgeDays(fullRunID, asOf string, now time.Time) int {
	fullDate, err := record.ParseDateField(record.RunDate(fullRunID))
	if err != nil {
		return 0
	}
	ref, err := record.ParseDateField(asOf)
	if err != nil {
		ref = now
	}
	d := int(ref.Sub(fullDate).Hours() / 24)
	if d < 0 {
		d = 0
	}
	return d
}
