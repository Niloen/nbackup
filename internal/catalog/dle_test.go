package catalog

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// TestStaleDLEs pins the three cases a staleness alert must distinguish: a DLE
// backed up within the window (not reported), a DLE whose newest archive predates
// the window (reported with its last-backup time), and a configured DLE with no
// archive in the catalog at all (reported as never backed up).
func TestStaleDLEs(t *testing.T) {
	dir := t.TempDir()
	vol := newVolume(t, dir)

	fresh := time.Now().Add(-1 * time.Hour)
	old := time.Now().Add(-40 * 24 * time.Hour)

	putRun(t, vol, committedRun("run-2026-06-01.000000", "2026-06-01", 1,
		record.Archive{DLE: "h-fresh", Level: 0, Compressed: 100, CreatedAt: old},
		record.Archive{DLE: "h-stale", Level: 0, Compressed: 100, CreatedAt: old}))
	// h-fresh gets a second, more recent archive; h-stale's newest stays old.
	putRun(t, vol, committedRun("run-2026-07-01.000000", "2026-07-01", 1,
		record.Archive{DLE: "h-fresh", Level: 1, Compressed: 10, CreatedAt: fresh}))

	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.EnsureFresh("disk", vol); err != nil {
		t.Fatal(err)
	}

	window := 30 * 24 * time.Hour
	now := time.Now()
	stale := cat.StaleDLEs([]string{"h-fresh", "h-stale", "h-never"}, window, now)

	byDLE := map[string]StaleDLE{}
	for _, s := range stale {
		byDLE[s.DLE] = s
	}
	if _, ok := byDLE["h-fresh"]; ok {
		t.Errorf("h-fresh backed up %s ago, should not be reported stale", now.Sub(fresh))
	}
	got, ok := byDLE["h-stale"]
	if !ok {
		t.Fatalf("h-stale not reported stale")
	}
	if !got.LastBackup.Equal(old) {
		t.Errorf("h-stale LastBackup = %v, want %v", got.LastBackup, old)
	}
	never, ok := byDLE["h-never"]
	if !ok {
		t.Fatalf("h-never (no archive at all) not reported stale")
	}
	if !never.LastBackup.IsZero() {
		t.Errorf("h-never LastBackup = %v, want zero", never.LastBackup)
	}
	if len(stale) != 2 {
		t.Errorf("StaleDLEs = %+v, want exactly h-stale and h-never", stale)
	}
}
