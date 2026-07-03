package conductor

import (
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// localDay is the calendar day of instant in loc, at midnight — the operator's
// wall-clock date, which the run id carries. Taking loc explicitly (rather than
// reading time.Local directly) keeps the day rule unit-testable across zones.
func localDay(instant time.Time, loc *time.Location) time.Time {
	y, m, d := instant.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// latestRunDate returns the most recent run date (YYYY-MM-DD) across the whole
// catalog, or ("", false) when no runs exist. Dates are lexically comparable.
func (c *Conductor) latestRunDate() (string, bool) {
	latest := ""
	for _, s := range c.d.Cat.Runs() {
		if d := s.Date(); d > latest {
			latest = d
		}
	}
	return latest, latest != ""
}

// mintRunID names a run from its start instant's local wall clock (Amanda's dump
// datestamp): the id is minted, never allocated against existing state, so a pruned
// run's id is never reused and no medium is touched — nothing here can scan a tape.
// The one guard is monotonicity: an id at or below one already in the catalog (a
// --date run pinned to midnight when the day already has runs, or a clock stepped
// backwards) would break "run ids sort as time", so it is bumped to one second past
// the latest known id instead. The catalog indexes every sealed run pool-globally,
// so the guard holds across volumes and media.
func (c *Conductor) mintRunID(instant time.Time, loc *time.Location) string {
	id := record.IDFromTime(instant.In(loc))
	latest := ""
	for _, s := range c.d.Cat.Runs() {
		if s.ID > latest {
			latest = s.ID
		}
	}
	if latest == "" || id > latest {
		return id
	}
	t, err := record.IDTime(latest, loc)
	if err != nil {
		// A non-canonical id in the catalog cannot anchor the bump; the clock-minted
		// id is still well-formed, so use it rather than fail the run.
		return id
	}
	return record.IDFromTime(t.Add(time.Second))
}

// PlannedRunID returns the run id a real dump at instant would mint — the preview
// peer of mintRunID, for `nb dump --dry-run`. Minting is pure (clock + catalog), so
// preview and real run share the one implementation; they differ only in the instant
// the caller passes.
func (c *Conductor) PlannedRunID(instant time.Time) string {
	return c.mintRunID(instant, time.Local)
}
