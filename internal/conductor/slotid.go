package conductor

import (
	"errors"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// localDay is the calendar day of instant in loc, at midnight — the operator's
// wall-clock date, which the slot id carries. Taking loc explicitly (rather than
// reading time.Local directly) keeps the day rule unit-testable across zones.
func localDay(instant time.Time, loc *time.Location) time.Time {
	y, m, d := instant.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// latestSlotDate returns the most recent slot date (YYYY-MM-DD) across the whole
// catalog, or ("", false) when no slots exist. Dates are lexically comparable.
func (c *Conductor) latestSlotDate() (string, bool) {
	latest := ""
	for _, s := range c.d.Cat.Slots() {
		if d := s.Date(); d > latest {
			latest = d
		}
	}
	return latest, latest != ""
}

// PlannedSlotID returns the slot id a real dump on date would seal next: the next
// free same-day sequence given the sealed slots already in the catalog. It is the
// preview peer of allocSlotID (which additionally reclaims an unsealed orphan on the
// loaded volume) and exists so `nb dump --dry-run` names the slot a real run would
// produce — not always `.1` — when the date is already sealed.
func (c *Conductor) PlannedSlotID(date time.Time) string {
	have := map[string]bool{}
	for _, s := range c.d.Cat.Slots() {
		have[s.ID] = true
	}
	ds := record.DateString(date)
	for seq := 1; ; seq++ {
		id := record.IDFromParts(ds, seq)
		if !have[id] {
			return id
		}
	}
}

// allocSlotID picks the slot ID for a run on the given date: the first run of
// the day is "slot-DATE", later runs get the next free ".N". A leftover unsealed
// slot from a failed attempt is reclaimed. This consults the volume (the write
// path may touch media) so it is robust to a stale cache.
func (c *Conductor) allocSlotID(date time.Time) (id string, seq int, err error) {
	files, err := c.d.Vol.Files()
	if err != nil {
		// A changer with nothing loaded yet (a fresh library before its first mount,
		// e.g. auto_label on a blank pool) has no files to scan for orphans. The
		// catalog still seeds every known slot id pool-globally below, so treat an
		// empty drive as "no extra files" rather than a hard failure — letting a
		// first dump proceed to PrepareWrite, which mounts and auto-labels a bay.
		if !errors.Is(err, media.ErrNoVolume) {
			return "", 0, err
		}
		files = nil
	}
	present := map[string]bool{} // slot id -> exists (catalog or loaded volume)
	sealed := map[string]bool{}  // slot id -> sealed (immutable; never reuse the id)
	// Seed from the catalog, which indexes every sealed slot across the whole pool.
	// A slot id is pool-global, so a same-day rerun must take the next free .N even
	// when an earlier run sealed onto a different volume (or medium) than the one now
	// loaded — scanning only the loaded volume's Files() would miss it and reuse the
	// id, shadowing that earlier run in the catalog. Catalog slots are sealed by
	// construction (Record runs only after Seal).
	for _, s := range c.d.Cat.Slots() {
		present[s.ID] = true
		sealed[s.ID] = true
	}
	// The loaded volume may also carry an orphan from a failed attempt that the catalog
	// never recorded; note it so its id can be reclaimed below. A slot with any committed
	// archive (a commit footer) is a real recovery point — its id is never reused; one with
	// only uncommitted parts is a reclaimable orphan.
	for _, f := range files {
		present[f.Header.Slot] = true
		if f.Header.Kind == record.KindCommit {
			sealed[f.Header.Slot] = true
		}
	}
	day := record.DateString(date)
	for seq = 1; ; seq++ {
		id = record.IDFromParts(day, seq)
		if !present[id] {
			return id, seq, nil
		}
		if sealed[id] {
			continue // a sealed slot occupies this id; try the next sequence
		}
		// Unsealed leftover from a failed attempt: reclaim its files. A medium that
		// cannot remove individual files (tape — space is reclaimed by relabeling the
		// whole volume) leaves the orphan in place; a scan ignores it (it has no seal),
		// and it is reclaimed on the next relabel. Take the next id rather than failing.
		removed := true
		for _, f := range files {
			if f.Header.Slot != id {
				continue
			}
			if err := c.d.Vol.RemoveFile(f.Pos); err != nil {
				if errors.Is(err, media.ErrNoFileRemoval) {
					removed = false
					break
				}
				return "", 0, err
			}
		}
		if !removed {
			continue
		}
		return id, seq, nil
	}
}
