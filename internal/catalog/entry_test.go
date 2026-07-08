package catalog

import (
	"testing"

	"github.com/Niloen/nbackup/internal/record"
)

// TestPlacementCovers: coverage is per-archive — a placement holding only some of
// the run's archives (a tripped fan-out lane, a per-archive prune) counts as
// partial, which is what lets the copies displays stop presenting every placement
// as a full copy of the run.
func TestPlacementCovers(t *testing.T) {
	run := &Run{ID: "run-2026-07-08.020000", Archives: []record.Archive{
		{DLE: "etc", Level: 0}, {DLE: "home", Level: 1},
	}}
	full := Placement{Medium: "disk", Archives: []PlacedArchive{
		{DLE: "etc", Level: 0}, {DLE: "home", Level: 1},
	}}
	if held, total := full.Covers(run); held != 2 || total != 2 {
		t.Fatalf("full copy Covers = %d/%d, want 2/2", held, total)
	}
	partial := Placement{Medium: "s3", Archives: []PlacedArchive{{DLE: "home", Level: 1}}}
	if held, total := partial.Covers(run); held != 1 || total != 2 {
		t.Fatalf("partial copy Covers = %d/%d, want 1/2", held, total)
	}
	// Same DLE at another level (a different cycle position) is a different archive.
	stale := Placement{Medium: "s3", Archives: []PlacedArchive{{DLE: "home", Level: 0}}}
	if held, _ := stale.Covers(run); held != 0 {
		t.Fatalf("wrong-level archive counted as coverage: held=%d", held)
	}
}
