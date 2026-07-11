package retention

import (
	"strings"
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
	got := Compute(runs, nil, 0, now) // minAge 0 so age never keeps

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
	got := Compute(runs, nil, 0, now) // minAge 0

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
	got := Compute(runs, nil, 0, now)

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
	got := Compute(runs, nil, 0, now) // minAge 0 so only chain/last-recovery pin

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
	got := Compute(runs, nil, 7*24*time.Hour, now)

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
	got := Compute(runs, nil, minAge, now)

	if got.Keeps("run-2026-01-04.001") {
		t.Errorf("old full committed 7h ago must age out under a 1h minimum_age; got %v", got)
	}
	if !got.Keeps("run-2026-01-04.3") {
		t.Errorf("incremental committed 30m ago must be kept within the 1h minimum_age")
	}
}

// Precedence and classification key on the typed Kind, never the rendered text:
// a floor whose reason strings are deliberately reworded still ranks
// age > last-full > chain (Reason) and still classifies each pin by its Kind
// (KindArchive), so rewording a user-facing message can never silently change
// either. Constructed directly so the texts share no wording with Compute's.
func TestFloor_KindDrivesRankAndClassification(t *testing.T) {
	f := Floor{reasons: map[archiveRef]pin{
		{"run-1", "a"}: {KindAge, "reworded age text"},
		{"run-1", "b"}: {KindLastFull, "reworded last-full text"},
		{"run-1", "c"}: {KindChain, "reworded chain text"},
	}}
	if reason, ok := f.Reason("run-1"); !ok || reason != "reworded age text" {
		t.Errorf("Reason = %q, %v; want the KindAge pin to outrank the others", reason, ok)
	}
	delete(f.reasons, archiveRef{"run-1", "a"})
	if reason, ok := f.Reason("run-1"); !ok || reason != "reworded last-full text" {
		t.Errorf("Reason = %q, %v; want the KindLastFull pin to outrank KindChain", reason, ok)
	}
	delete(f.reasons, archiveRef{"run-1", "b"})
	if reason, ok := f.Reason("run-1"); !ok || reason != "reworded chain text" {
		t.Errorf("Reason = %q, %v; want the KindChain pin", reason, ok)
	}
	if kind, ok := f.KindArchive("run-1", "c"); !ok || kind != KindChain {
		t.Errorf("KindArchive = %v, %v; want KindChain regardless of the text", kind, ok)
	}
	if _, ok := f.KindArchive("run-1", "a"); ok {
		t.Errorf("KindArchive must report ok=false for an unpinned archive")
	}
}

// Compute stamps the Kind the callers classify on: an age pin is KindAge (the one
// MediumResidualIsAgeBound treats as releasable by shortening minimum_age),
// a last-full pin is KindLastFull, a chain pin is KindChain.
func TestFloor_ComputeStampsKinds(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	runs := cat(
		mkRun("run-2026-01-01.001", "2026-01-01", arch("app", 0)),
		mkRun("run-2026-01-02.001", "2026-01-02", arch("app", 1)),
		mkRunAt("run-2026-02-28.001", "2026-02-28", now.Add(-time.Hour), arch("db", 1)),
	)
	got := Compute(runs, nil, 24*time.Hour, now)

	if kind, ok := got.KindArchive("run-2026-02-28.001", "db"); !ok || kind != KindAge {
		t.Errorf("young archive: kind = %v, %v; want KindAge", kind, ok)
	}
	if kind, ok := got.KindArchive("run-2026-01-01.001", "app"); !ok || kind != KindLastFull {
		t.Errorf("last full: kind = %v, %v; want KindLastFull", kind, ok)
	}
	if kind, ok := got.KindArchive("run-2026-01-02.001", "app"); !ok || kind != KindChain {
		t.Errorf("chain incremental: kind = %v, %v; want KindChain", kind, ok)
	}
}

// TestComputeCondemns pins the floor's opposite verdict: an archive no restore
// anywhere can use (judged against the corpus, so a base copy on another medium
// spares it) is condemned — never pinned, not even by the chain rule — while a
// young unrestorable archive keeps an age pin that says so instead.
func TestComputeCondemns(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	arch := func(run, dle string, level int, base string, created time.Time) record.Archive {
		return record.Archive{Run: run, DLE: dle, Level: level, BaseRun: base, CreatedAt: created}
	}
	stranded := arch("run-2026-06-02.000001", "app", 1, "run-2026-06-01.000001", old)
	newFull := arch("run-2026-06-10.000001", "app", 0, "", old)

	t.Run("a broken chain is condemned and never pinned", func(t *testing.T) {
		archives := []record.Archive{stranded, newFull}
		f := Compute(archives, archives, 0, now)
		if !f.CondemnsArchive(stranded.Run, "app") {
			t.Fatal("the stranded incremental must be condemned")
		}
		if err, ok := f.Condemned(stranded.Run, "app"); !ok || err == nil {
			t.Fatalf("Condemned = (%v, %v), want the chain error", err, ok)
		}
		if f.KeepsArchive(stranded.Run, "app") {
			t.Fatal("condemned and kept are exclusive — it must not be pinned")
		}
		if f.CondemnsArchive(newFull.Run, "app") || !f.KeepsArchive(newFull.Run, "app") {
			t.Fatal("the intact full must be kept, not condemned")
		}
	})

	t.Run("a base copy elsewhere in the corpus spares the archive", func(t *testing.T) {
		base := arch("run-2026-06-01.000001", "app", 0, "", old) // another medium: corpus-only
		f := Compute([]record.Archive{stranded}, []record.Archive{base, stranded}, 0, now)
		if f.CondemnsArchive(stranded.Run, "app") {
			t.Fatal("restorable across media — must not be condemned")
		}
	})

	t.Run("young unrestorable is age-pinned with the reason, not condemned", func(t *testing.T) {
		fresh := arch("run-2026-07-08.000001", "app", 1, "run-2026-06-01.000001", now.Add(-time.Hour))
		f := Compute([]record.Archive{fresh}, []record.Archive{fresh}, 24*time.Hour, now)
		if f.CondemnsArchive(fresh.Run, "app") {
			t.Fatal("within minimum_age: the WORM guard defers the condemnation")
		}
		reason, ok := f.ReasonArchive(fresh.Run, "app")
		if !ok || !strings.Contains(reason, "unrestorable") {
			t.Fatalf("reason = %q (%v), want an age pin saying it is unrestorable", reason, ok)
		}
		if kind, _ := f.KindArchive(fresh.Run, "app"); kind != KindAge {
			t.Fatalf("kind = %v, want KindAge", kind)
		}
	})

	t.Run("nil corpus renders no condemnations", func(t *testing.T) {
		f := Compute([]record.Archive{stranded}, nil, 0, now)
		if f.CondemnsArchive(stranded.Run, "app") {
			t.Fatal("nil corpus must judge nothing")
		}
	})
}
