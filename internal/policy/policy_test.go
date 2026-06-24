package policy

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/slot"
)

// mkSlot builds a slot dated `date` (YYYY-MM-DD) holding the given archives,
// each "dle:level".
func mkSlot(id, date string, archives ...slot.Archive) *slot.Slot {
	return &slot.Slot{ID: id, Date: date, Archives: archives}
}

func arch(dle string, level int) slot.Archive { return slot.Archive{DLE: dle, Level: level} }

// An incremental-only slot whose base full lives in an older slot must NOT be
// protected as a "last recovery path" — only the full is the recovery floor.
func TestProtected_IncrementalOnlySlotNotPinned(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	slots := []*slot.Slot{
		mkSlot("slot-2026-01-01", "2026-01-01", arch("app", 0)), // the base full
		mkSlot("slot-2026-01-02", "2026-01-02", arch("app", 1)), // tip incremental, no newer full
	}
	got := Protected(slots, 0, now) // minAge 0 so age never protects

	if _, ok := got["slot-2026-01-02"]; ok {
		t.Errorf("incremental-only tip slot was protected, want reclaimable: %v", got)
	}
	if reason, ok := got["slot-2026-01-01"]; !ok {
		t.Errorf("base full slot must be protected as the last recovery path; got %v", got)
	} else if want := "last recovery path for DLE app"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
}

// When a slot holds a full of one DLE and an incremental of another, the
// protection reason must name the DLE whose full it actually carries.
func TestProtected_ReasonNamesTheProtectingFull(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	slots := []*slot.Slot{
		mkSlot("slot-2026-01-01", "2026-01-01", arch("etc", 0)),
		// etc gets a later full here; home gets its only full here too.
		mkSlot("slot-2026-01-02", "2026-01-02", arch("etc", 1), arch("home", 0)),
	}
	got := Protected(slots, 0, now)

	if reason := got["slot-2026-01-02"]; reason != "last recovery path for DLE home" {
		t.Errorf("reason = %q, want it to name home (its full), not etc (a mere incremental)", reason)
	}
}

// The minimum-age floor protects young slots regardless of level, and renders
// the age in the config's day vocabulary.
func TestProtected_MinAgeReasonInDays(t *testing.T) {
	now := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	slots := []*slot.Slot{mkSlot("slot-2026-01-04", "2026-01-04", arch("app", 1))}
	got := Protected(slots, 7*24*time.Hour, now)

	if reason := got["slot-2026-01-04"]; reason != "within minimum age (7d)" {
		t.Errorf("reason = %q, want \"within minimum age (7d)\"", reason)
	}
}
