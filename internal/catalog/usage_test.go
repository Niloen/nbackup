package catalog

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// usageCatalog opens a catalog in a temp workdir with a deterministic clock: each
// persist stamps a strictly later instant, so tests can assert order without racing
// the wall clock.
func usageCatalog(t *testing.T) *Catalog {
	t.Helper()
	cat, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tick := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	cat.now = func() time.Time {
		tick = tick.Add(time.Minute)
		return tick
	}
	return cat
}

func usageArchive(run, dle string, size int64) record.Archive {
	return record.Archive{Run: run, DLE: dle, Level: 0, Compressed: size, CreatedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
}

func pos(p int) archiveio.ArchivePos {
	return archiveio.ArchivePos{Parts: []archiveio.FilePos{{Pos: p}}, Commit: archiveio.FilePos{Pos: p + 1}}
}

// TestUsageLedgerRecordsRiseAndFall pins the ledger's contract: every byte-changing
// persist records a per-medium sample — adds rise, removals fall, and a medium
// emptied of its last archive records its fall to zero (the union diff).
func TestUsageLedgerRecordsRiseAndFall(t *testing.T) {
	cat := usageCatalog(t)

	if err := cat.AddArchive(usageArchive("run-2026-07-01.001200", "a", 100), "disk", pos(1)); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddArchive(usageArchive("run-2026-07-02.001200", "a", 50), "disk", pos(3)); err != nil {
		t.Fatal(err)
	}
	series := cat.MediumUsage("disk")
	if len(series) != 2 || series[0].Used != 100 || series[1].Used != 150 {
		t.Fatalf("after two adds, series = %+v; want used [100, 150]", series)
	}
	if series[1].Runs != 2 {
		t.Errorf("second sample Runs = %d, want 2", series[1].Runs)
	}
	if !series[0].At.Before(series[1].At) {
		t.Errorf("samples not in time order: %v then %v", series[0].At, series[1].At)
	}

	// A prune-style removal records the fall...
	if _, _, err := cat.RemoveArchive("run-2026-07-01.001200", "disk", "a"); err != nil {
		t.Fatal(err)
	}
	// ...and removing the last archive records the fall to zero, even though the
	// medium no longer appears in any placement (the union over ledger-known media).
	if _, _, err := cat.RemoveArchive("run-2026-07-02.001200", "disk", "a"); err != nil {
		t.Fatal(err)
	}
	series = cat.MediumUsage("disk")
	if len(series) != 4 || series[2].Used != 50 || series[3].Used != 0 {
		t.Fatalf("after removals, series = %+v; want used [100, 150, 50, 0]", series)
	}
	if series[3].Runs != 0 {
		t.Errorf("emptied medium's sample Runs = %d, want 0", series[3].Runs)
	}
}

// TestUsageLedgerIgnoresNonByteMutations pins that persists which move no bytes —
// volume registry updates, force-full directives — record nothing: the ledger is a
// step series of real changes, not a per-persist heartbeat.
func TestUsageLedgerIgnoresNonByteMutations(t *testing.T) {
	cat := usageCatalog(t)
	if err := cat.AddArchive(usageArchive("run-2026-07-01.001200", "a", 100), "disk", pos(1)); err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordVolume(record.Label{Name: "DAILY-01", Pool: "tape"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetVolumeBarcode("DAILY-01", "BC001"); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetForceFull("a"); err != nil {
		t.Fatal(err)
	}
	if got := len(cat.MediumUsage("disk")); got != 1 {
		t.Errorf("non-byte mutations added samples: %d, want 1", got)
	}
}

// TestUsageLedgerSurvivesReopen pins persistence: a reopened catalog serves the
// recorded series and keeps diffing against it (no duplicate sample for an unchanged
// medium, a changed one appends).
func TestUsageLedgerSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.AddArchive(usageArchive("run-2026-07-01.001200", "a", 100), "disk", pos(1)); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.MediumUsage("disk"); len(got) != 1 || got[0].Used != 100 {
		t.Fatalf("reopened series = %+v; want the one recorded sample", got)
	}
	// A no-byte-change persist after reopen must not re-record the same total.
	if err := reopened.RecordVolume(record.Label{Name: "V", Pool: "tape"}); err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.MediumUsage("disk")); got != 1 {
		t.Errorf("reopen re-recorded an unchanged medium: %d samples, want 1", got)
	}
	// A real change appends against the reloaded diff base.
	if err := reopened.AddArchive(usageArchive("run-2026-07-02.001200", "a", 25), "disk", pos(3)); err != nil {
		t.Fatal(err)
	}
	if got := reopened.MediumUsage("disk"); len(got) != 2 || got[1].Used != 125 {
		t.Fatalf("post-reopen add: series = %+v; want used [100, 125]", got)
	}
}

// TestUsageLedgerRebuildRecordsOnce pins that a rebuild — one persist at the end of
// the scan — records at most one sample per medium, never a per-archive flood.
func TestUsageLedgerRebuildRecordsOnce(t *testing.T) {
	dir := t.TempDir()
	vol := newVolume(t, t.TempDir())
	putRun(t, vol, committedRun("run-2026-07-01.001200", "", 0,
		record.Archive{DLE: "a", Level: 0, Compressed: 100},
		record.Archive{DLE: "b", Level: 0, Compressed: 200},
	))
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Rebuild(map[string]media.Volume{"disk": vol}, true); err != nil {
		t.Fatal(err)
	}
	series := cat.MediumUsage("disk")
	if len(series) != 1 {
		t.Fatalf("rebuild recorded %d samples, want exactly 1: %+v", len(series), series)
	}
	if series[0].Used != 300 || series[0].Runs != 1 {
		t.Errorf("rebuild sample = %+v; want used 300, runs 1", series[0])
	}
}

// TestThinUsageKeepsDailyShape pins the compaction rule: recent samples keep full
// granularity, old ones thin to the last per medium per day — the step curve's daily
// closing values survive.
func TestThinUsageKeepsDailyShape(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour)
	all := []UsageSample{
		{At: old.Add(1 * time.Hour), Medium: "disk", Used: 10},
		{At: old.Add(2 * time.Hour), Medium: "disk", Used: 20}, // same old day: only this survives
		{At: old.Add(2 * time.Hour), Medium: "tape", Used: 5},  // other medium, same day: kept
		{At: now.Add(-time.Hour), Medium: "disk", Used: 30},    // recent: kept verbatim
	}
	thinned := thinUsage(all, now)
	if len(thinned) != 3 {
		t.Fatalf("thinned = %+v; want 3 samples", thinned)
	}
	if thinned[0].Used != 20 || thinned[0].Medium != "disk" {
		t.Errorf("old day's survivor = %+v; want the day's last disk sample (20)", thinned[0])
	}
	if thinned[1].Medium != "tape" || thinned[2].Used != 30 {
		t.Errorf("thinned = %+v; want tape sample and the recent disk sample kept", thinned)
	}
}
