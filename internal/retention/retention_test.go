package retention

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// mkSlot builds a slot dated `date` (YYYY-MM-DD) holding the given archives,
// each "dle:level".
func mkSlot(id, date string, archives ...record.Archive) *record.Slot {
	return &record.Slot{ID: id, Date: date, Archives: archives}
}

func arch(dle string, level int) record.Archive { return record.Archive{DLE: dle, Level: level} }

// The live recovery chain — the last full plus every later incremental — is kept
// in full, even past minAge: a whole-DLE restore as of the tip's date replays the
// full and the incremental, so dropping the tip would lose the latest state (and,
// for climbing levels, dropping a middle incremental would break the chain). Only
// a chain superseded by a newer full is reclaimable (TestFloor_SupersededChain...).
func TestFloor_LiveChainKept(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	slots := []*record.Slot{
		mkSlot("slot-2026-01-01", "2026-01-01", arch("app", 0)), // the base full
		mkSlot("slot-2026-01-02", "2026-01-02", arch("app", 1)), // tip incremental, no newer full
	}
	got := Compute(slots, 0, now) // minAge 0 so age never keeps

	if reason, ok := got.Reason("slot-2026-01-01"); !ok {
		t.Errorf("base full slot must be kept as the last recovery path; got %v", got)
	} else if want := "last recovery path for DLE app"; reason != want {
		t.Errorf("full reason = %q, want %q", reason, want)
	}
	if reason, ok := got.Reason("slot-2026-01-02"); !ok {
		t.Errorf("tip incremental must be kept as part of the live recovery chain; got %v", got)
	} else if want := "in DLE app's recovery chain"; reason != want {
		t.Errorf("tip reason = %q, want %q", reason, want)
	}
}

// A chain superseded by a newer full is reclaimable past minAge: once a later full
// exists, the old full + its incrementals are no longer a needed recovery path.
func TestFloor_SupersededChainReclaimable(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	slots := []*record.Slot{
		mkSlot("slot-2026-01-01", "2026-01-01", arch("app", 0)), // old full (superseded)
		mkSlot("slot-2026-01-02", "2026-01-02", arch("app", 1)), // old incremental (superseded)
		mkSlot("slot-2026-02-01", "2026-02-01", arch("app", 0)), // newer full
	}
	got := Compute(slots, 0, now) // minAge 0

	for _, id := range []string{"slot-2026-01-01", "slot-2026-01-02"} {
		if got.Keeps(id) {
			t.Errorf("superseded slot %s was kept, want reclaimable: %v", id, got)
		}
	}
	if !got.Keeps("slot-2026-02-01") {
		t.Errorf("the newer full must be kept; got %v", got)
	}
}

// When a slot holds a full of one DLE and an incremental of another, the
// keep reason must name the DLE whose full it actually carries.
func TestFloor_ReasonNamesTheProtectingFull(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	slots := []*record.Slot{
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
	slots := []*record.Slot{mkSlot("slot-2026-01-04", "2026-01-04", arch("app", 1))}
	got := Compute(slots, 7*24*time.Hour, now)

	if reason, _ := got.Reason("slot-2026-01-04"); reason != "within minimum age (7d)" {
		t.Errorf("reason = %q, want \"within minimum age (7d)\"", reason)
	}
}
