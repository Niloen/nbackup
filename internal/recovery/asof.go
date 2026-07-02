// asof.go resolves an as-of point (a date, or a date+time instant) to the target
// run — the shared resolution used by the browse view, a whole-DLE restore, and the
// cost forecast, so a bare date means the same run everywhere.
package recovery

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// AsOf resolves an as-of point to the target run: the most recent run at or
// before it. Runs must be in run order. The point is either a plain date
// (YYYY-MM-DD — the most recent run whose run date is on or before it, the
// long-standing behavior) or a date with a time component (YYYY-MM-DD HH[:MM[:SS]],
// interpreted in UTC) — the most recent run committed at or before the end of that
// period, so an earlier same-day run is reachable by naming its time.
func AsOf(archives []record.Archive, asOf string) (string, error) {
	bound, hasTime, err := parseAsOf(asOf)
	if err != nil {
		return "", err
	}
	// Group the archives into runs, in run order, each with its commit time (the latest
	// archive that landed in it).
	last := map[string]time.Time{}
	var ids []string
	for _, a := range archives {
		if _, seen := last[a.Run]; !seen {
			ids = append(ids, a.Run)
		}
		if a.CreatedAt.After(last[a.Run]) {
			last[a.Run] = a.CreatedAt
		}
	}
	sort.Slice(ids, func(i, j int) bool { return record.RunIDLess(ids[i], ids[j]) })
	target := ""
	for _, id := range ids {
		if hasTime {
			if !runTime(id, last[id]).After(bound) { // committed at or before the period's end
				target = id
			}
		} else if record.RunDate(id) <= asOf {
			target = id
		}
	}
	if target == "" {
		return "", fmt.Errorf("no backup on or before %s", asOf)
	}
	return target, nil
}

// ValidateAsOf checks an as-of string parses — a bare YYYY-MM-DD, or a date with
// an hour/minute/second time component (UTC) — and returns it trimmed. It is the
// one place the accepted layouts live, so the CLI's flag validation and AsOf's
// resolution can never drift apart.
func ValidateAsOf(s string) (string, error) {
	s = strings.TrimSpace(s)
	if _, _, err := parseAsOf(s); err != nil {
		return "", err
	}
	return s, nil
}

// parseAsOf interprets an as-of string. A bare YYYY-MM-DD selects by run date
// (hasTime false). A date with an hour/minute/second selects by time: bound is the
// exclusive end of the named period (the hour, minute, or second), so "2026-06-29 14"
// matches the latest run committed during or before hour 14.
func parseAsOf(asOf string) (bound time.Time, hasTime bool, err error) {
	for _, p := range []struct {
		layout string
		window time.Duration
	}{
		{"2006-01-02 15:04:05", time.Second},
		{"2006-01-02 15:04", time.Minute},
		{"2006-01-02 15", time.Hour},
	} {
		if t, e := time.ParseInLocation(p.layout, asOf, time.UTC); e == nil {
			return t.Add(p.window), true, nil
		}
	}
	if _, e := time.ParseInLocation("2006-01-02", asOf, time.UTC); e == nil {
		return time.Time{}, false, nil
	}
	return time.Time{}, false, fmt.Errorf("invalid as-of %q: want YYYY-MM-DD or 'YYYY-MM-DD HH[:MM[:SS]]'", asOf)
}

// runTime is a run's effective recovery instant: when its last archive committed, falling
// back to its run date at midnight UTC for a run whose archives carry no commit time (e.g.
// one rebuilt from older media). last is the latest CreatedAt across the run's archives.
func runTime(runID string, last time.Time) time.Time {
	if !last.IsZero() {
		return last
	}
	t, _ := time.ParseInLocation("2006-01-02", record.RunDate(runID), time.UTC)
	return t
}
