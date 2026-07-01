package retention

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// mkRun builds a run dated `date` (YYYY-MM-DD) holding the given archives,
// each "dle:level". It seals the run at the date's midnight, so age tests that
// reason in whole days behave as the date implies; mkRunAt sets a precise commit
// instant for sub-day age tests.
func mkRun(id, date string, archives ...record.Archive) []record.Archive {
	t, _ := record.ParseDateField(date)
	return mkRunAt(id, date, t, archives...)
}

// mkRunAt is mkRun with an explicit commit instant, stamped on each archive's CreatedAt
// (retention ages per archive), for exercising minimum_age below a day. It returns the run's
// archives, each tagged with the run id — the corpus the policy layer works on.
func mkRunAt(id, date string, at time.Time, archives ...record.Archive) []record.Archive {
	for i := range archives {
		archives[i].Run = id
		archives[i].CreatedAt = at
	}
	return archives
}

// cat flattens several runs' archives into the one corpus the policy functions take.
func cat(runs ...[]record.Archive) []record.Archive {
	var out []record.Archive
	for _, s := range runs {
		out = append(out, s...)
	}
	return out
}

func arch(dle string, level int) record.Archive { return record.Archive{DLE: dle, Level: level} }

// The live recovery chain — the last full plus every later incremental — is kept
// in full, even past minAge: a whole-DLE restore as of the tip's date replays the
// full and the incremental, so dropping the tip would lose the latest state (and,
// for climbing levels, dropping a middle incremental would break the chain). Only
// a chain superseded by a newer full is reclaimable (TestFloor_SupersededChain...).
func TestFloor_LiveChainKept(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	runs := cat(
		mkRun("run-2026-01-01.001", "2026-01-01", arch("app", 0)), // the base full
		mkRun("run-2026-01-02.001", "2026-01-02", arch("app", 1)), // tip incremental, no newer full
	)
	got := Compute(runs, 0, now) // minAge 0 so age never keeps

	if reason, ok := got.Reason("run-2026-01-01.001"); !ok {
		t.Errorf("base full run must be kept as the last recovery path; got %v", got)
	} else if want := "last recovery path"; reason != want {
		t.Errorf("full reason = %q, want %q", reason, want)
	}
	if reason, ok := got.Reason("run-2026-01-02.001"); !ok {
		t.Errorf("tip incremental must be kept as part of the live recovery chain; got %v", got)
	} else if want := "in this DLE's recovery chain"; reason != want {
		t.Errorf("tip reason = %q, want %q", reason, want)
	}
}

// A chain superseded by a newer full is reclaimable past minAge: once a later full
// exists, the old full + its incrementals are no longer a needed recovery path.
func TestFloor_SupersededChainReclaimable(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	runs := cat(
		mkRun("run-2026-01-01.001", "2026-01-01", arch("app", 0)), // old full (superseded)
		mkRun("run-2026-01-02.001", "2026-01-02", arch("app", 1)), // old incremental (superseded)
		mkRun("run-2026-02-01.001", "2026-02-01", arch("app", 0)), // newer full
	)
	got := Compute(runs, 0, now) // minAge 0

	for _, id := range []string{"run-2026-01-01.001", "run-2026-01-02.001"} {
		if got.Keeps(id) {
			t.Errorf("superseded run %s was kept, want reclaimable: %v", id, got)
		}
	}
	if !got.Keeps("run-2026-02-01.001") {
		t.Errorf("the newer full must be kept; got %v", got)
	}
}

// When a run holds a full of one DLE and an incremental of another, the
// keep reason must name the DLE whose full it actually carries.
func TestFloor_ReasonNamesTheProtectingFull(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	runs := cat(
		mkRun("run-2026-01-01.001", "2026-01-01", arch("etc", 0)),
		// etc gets a later full here; home gets its only full here too.
		mkRun("run-2026-01-02.001", "2026-01-02", arch("etc", 1), arch("home", 0)),
	)
	got := Compute(runs, 0, now)

	if reason, _ := got.Reason("run-2026-01-02.001"); reason != "last recovery path" {
		t.Errorf("reason = %q, want it to name home (its full), not etc (a mere incremental)", reason)
	}
}

// The floor is per-archive: in one run, a DLE whose chain has been superseded by a
// newer full is reclaimable while a run-mate DLE still in its live chain is kept.
// This is what lets per-DLE pruning free the dead archive without touching the live
// one — a run-granular floor would keep the whole run for the live DLE's sake.
func TestFloor_PerArchiveWithinOneRun(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	runs := cat(
		// Day 1: both DLEs full.
		mkRun("run-2026-01-01.001", "2026-01-01", arch("app", 0), arch("db", 0)),
		// Day 2: app gets a newer full (superseding its day-1 full); db only increments,
		// so db's day-1 full is still its last recovery path.
		mkRun("run-2026-02-01.001", "2026-02-01", arch("app", 0), arch("db", 1)),
	)
	got := Compute(runs, 0, now) // minAge 0 so only chain/last-recovery pin

	if got.KeepsArchive("run-2026-01-01.001", "app") {
		t.Errorf("app's day-1 full is superseded — its archive should be reclaimable")
	}
	if !got.KeepsArchive("run-2026-01-01.001", "db") {
		t.Errorf("db's day-1 full is its last recovery path — its archive must be kept")
	}
	// The run as a whole is still pinned (db keeps it), so a run-granular pass would
	// strand app's dead archive behind db.
	if !got.Keeps("run-2026-01-01.001") {
		t.Errorf("run must be kept at the run level because db still needs it")
	}
}

// The minimum-age floor keeps young runs regardless of level, and renders
// the age in the config's day vocabulary.
func TestFloor_MinAgeReasonInDays(t *testing.T) {
	now := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	runs := cat(mkRun("run-2026-01-04.001", "2026-01-04", arch("app", 1)))
	got := Compute(runs, 7*24*time.Hour, now)

	if reason, _ := got.Reason("run-2026-01-04.001"); reason != "within minimum age (7d)" {
		t.Errorf("reason = %q, want \"within minimum age (7d)\"", reason)
	}
}

// Age is measured from the commit instant, not the day-granular Date: two runs
// dated the same day are judged independently by a sub-day minimum_age, so one
// committed within the window is kept while an earlier one that day ages out.
// This is the case where a same-day incremental on an old full would otherwise
// pin that full forever because every run dated "today" read as age zero.
func TestFloor_MinAgeSubDay(t *testing.T) {
	now := time.Date(2026, 1, 4, 11, 0, 0, 0, time.UTC)
	minAge := time.Hour
	runs := cat(
		// A newer full exists, so neither old run is a last recovery path; only
		// age and the live chain can pin them. The young incremental builds on the
		// NEWER full, so it cannot drag the old full back into a chain.
		mkRunAt("run-2026-01-04.001", "2026-01-04", time.Date(2026, 1, 4, 4, 0, 0, 0, time.UTC), arch("app", 0)), // old full, committed 04:00 (7h ago)
		mkRunAt("run-2026-01-04.2", "2026-01-04", time.Date(2026, 1, 4, 8, 0, 0, 0, time.UTC), arch("app", 0)),   // newer full, committed 08:00 (3h ago)
		mkRunAt("run-2026-01-04.3", "2026-01-04", time.Date(2026, 1, 4, 10, 30, 0, 0, time.UTC), arch("app", 1)), // incremental, committed 10:30 (30m ago, young)
	)
	got := Compute(runs, minAge, now)

	if got.Keeps("run-2026-01-04.001") {
		t.Errorf("old full committed 7h ago must age out under a 1h minimum_age; got %v", got)
	}
	if !got.Keeps("run-2026-01-04.3") {
		t.Errorf("incremental committed 30m ago must be kept within the 1h minimum_age")
	}
}
