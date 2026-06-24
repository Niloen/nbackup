package retention

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/format"
)

// mkSlot builds a slot dated `date` (YYYY-MM-DD) holding the given archives,
// each "dle:level".
func mkSlot(id, date string, archives ...format.Archive) *format.Slot {
	return &format.Slot{ID: id, Date: date, Archives: archives}
}

func arch(dle string, level int) format.Archive { return format.Archive{DLE: dle, Level: level} }

// An incremental-only slot whose base full lives in an older slot must NOT be
// kept as a "last recovery path" — only the full is the recovery floor.
func TestFloor_IncrementalOnlySlotNotPinned(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	slots := []*format.Slot{
		mkSlot("slot-2026-01-01", "2026-01-01", arch("app", 0)), // the base full
		mkSlot("slot-2026-01-02", "2026-01-02", arch("app", 1)), // tip incremental, no newer full
	}
	got := Compute(slots, 0, now) // minAge 0 so age never keeps

	if got.Keeps("slot-2026-01-02") {
		t.Errorf("incremental-only tip slot was kept, want reclaimable: %v", got)
	}
	if reason, ok := got.Reason("slot-2026-01-01"); !ok {
		t.Errorf("base full slot must be kept as the last recovery path; got %v", got)
	} else if want := "last recovery path for DLE app"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
}

// When a slot holds a full of one DLE and an incremental of another, the
// keep reason must name the DLE whose full it actually carries.
func TestFloor_ReasonNamesTheProtectingFull(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	slots := []*format.Slot{
		mkSlot("slot-2026-01-01", "2026-01-01", arch("etc", 0)),
		// etc gets a later full here; home gets its only full here too.
		mkSlot("slot-2026-01-02", "2026-01-02", arch("etc", 1), arch("home", 0)),
	}
	got := Compute(slots, 0, now)

	if reason, _ := got.Reason("slot-2026-01-02"); reason != "last recovery path for DLE home" {
		t.Errorf("reason = %q, want it to name home (its full), not etc (a mere incremental)", reason)
	}
}

// The minimum-age floor keeps young slots regardless of level, and renders
// the age in the config's day vocabulary.
func TestFloor_MinAgeReasonInDays(t *testing.T) {
	now := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	slots := []*format.Slot{mkSlot("slot-2026-01-04", "2026-01-04", arch("app", 1))}
	got := Compute(slots, 7*24*time.Hour, now)

	if reason, _ := got.Reason("slot-2026-01-04"); reason != "within minimum age (7d)" {
		t.Errorf("reason = %q, want \"within minimum age (7d)\"", reason)
	}
}
