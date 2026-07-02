package recovery

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// timedRuns builds two same-day runs that differ only by commit time, so an as-of
// with a time component can pick the earlier one that a bare date cannot distinguish.
func timedRuns() []record.Archive {
	return []record.Archive{{
		Run: "run-2026-06-29.001", DLE: "app", Level: 0,
		CreatedAt: time.Date(2026, 6, 29, 10, 30, 0, 0, time.UTC),
	}, {
		Run: "run-2026-06-29.002", DLE: "app", Level: 1,
		CreatedAt: time.Date(2026, 6, 29, 16, 0, 0, 0, time.UTC),
	}}
}

// TestAsOfTimeComponent covers the time-component resolution path: an as-of with an
// hour/minute/second selects by commit time, reaching an earlier same-day run that a
// bare date (which resolves to the day's latest run) cannot.
func TestAsOfTimeComponent(t *testing.T) {
	runs := timedRuns()
	for _, tc := range []struct {
		asOf, want string
	}{
		{"2026-06-29 14", "run-2026-06-29.001"},       // hour 14: only the 10:30 run has committed
		{"2026-06-29 16", "run-2026-06-29.002"},       // hour 16: the 16:00 run is in-window
		{"2026-06-29 10:30", "run-2026-06-29.001"},    // minute precision
		{"2026-06-29 10:30:00", "run-2026-06-29.001"}, // second precision
		{"2026-06-29 16:00:00", "run-2026-06-29.002"}, // second precision, later run
	} {
		got, err := AsOf(runs, tc.asOf)
		if err != nil || got != tc.want {
			t.Errorf("AsOf(%q) = %q, %v; want %q", tc.asOf, got, err, tc.want)
		}
	}
}

// TestAsOfTimeComponentBeforeAll confirms a time-component as-of earlier than every
// run's commit time resolves to no backup.
func TestAsOfTimeComponentBeforeAll(t *testing.T) {
	if got, err := AsOf(timedRuns(), "2026-06-29 09"); err == nil {
		t.Errorf("AsOf before all commit times = %q, want error", got)
	}
}

// TestAsOfTimeComponentZeroCreatedAt exercises the zero-CreatedAt fallback under a
// time-component as-of: an archive with no commit time falls back to its run date at
// midnight UTC, so it is still reachable by naming a later time the same day.
func TestAsOfTimeComponentZeroCreatedAt(t *testing.T) {
	runs := []record.Archive{{Run: "run-2026-06-29.001", DLE: "app", Level: 0}} // zero CreatedAt
	got, err := AsOf(runs, "2026-06-29 14")
	if err != nil || got != "run-2026-06-29.001" {
		t.Errorf("AsOf(%q) with zero CreatedAt = %q, %v; want run-2026-06-29.001", "2026-06-29 14", got, err)
	}
}

// TestRunTime pins runTime's two paths: a non-zero last commit is returned as-is, and
// a zero last falls back to the run's date at midnight UTC.
func TestRunTime(t *testing.T) {
	last := time.Date(2026, 6, 29, 16, 0, 0, 0, time.UTC)
	if got := runTime("run-2026-06-29.001", last); !got.Equal(last) {
		t.Errorf("runTime with non-zero last = %v, want %v", got, last)
	}
	wantMidnight := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	if got := runTime("run-2026-06-29.001", time.Time{}); !got.Equal(wantMidnight) {
		t.Errorf("runTime fallback = %v, want %v", got, wantMidnight)
	}
}

// TestValidateAsOf covers the accepted layouts (bare date and date+time), whitespace
// trimming, and rejection of garbage — the one place the CLI and AsOf share.
func TestValidateAsOf(t *testing.T) {
	for _, in := range []string{"2026-06-29", "2026-06-29 14", "2026-06-29 14:05", "2026-06-29 14:05:06"} {
		if _, err := ValidateAsOf(in); err != nil {
			t.Errorf("ValidateAsOf(%q) = %v, want ok", in, err)
		}
	}
	if got, err := ValidateAsOf("  2026-06-29  "); err != nil || got != "2026-06-29" {
		t.Errorf("ValidateAsOf trims to %q, %v; want %q", got, err, "2026-06-29")
	}
	for _, in := range []string{"", "nonsense", "2026/06/29", "2026-13-40"} {
		if _, err := ValidateAsOf(in); err == nil {
			t.Errorf("ValidateAsOf(%q) = nil error, want rejection", in)
		}
	}
}
