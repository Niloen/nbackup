package conductor

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// TestLocalDay pins the slot-date rule: a slot is dated the run instant's *local*
// calendar day, so the same UTC instant yields a different date either side of
// midnight depending on the zone's offset.
func TestLocalDay(t *testing.T) {
	// 23:00 UTC on the 28th: already the 29th in UTC+2, still the 28th in UTC-5.
	inst := time.Date(2026, 6, 28, 23, 0, 0, 0, time.UTC)
	cases := []struct {
		loc  *time.Location
		want string
	}{
		{time.FixedZone("east", 2*3600), "2026-06-29"},
		{time.FixedZone("west", -5*3600), "2026-06-28"},
		{time.UTC, "2026-06-28"},
	}
	for _, c := range cases {
		if got := record.DateString(localDay(inst, c.loc)); got != c.want {
			t.Errorf("localDay(%s, %s) = %s, want %s", inst, c.loc, got, c.want)
		}
	}
}
