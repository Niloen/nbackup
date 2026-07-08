package catalog

import (
	"testing"

	"github.com/Niloen/nbackup/internal/record"
)

// TestPlacementMissing: coverage is per-archive — a placement holding only some
// of a run's archives (a tripped fan-out lane, a per-archive prune) reports
// exactly the gap, which is what copy's resume set, sync's backlog, and the
// coverage displays are all built on.
func TestPlacementMissing(t *testing.T) {
	archives := []record.Archive{
		{DLE: "etc", Level: 0}, {DLE: "home", Level: 1},
	}
	full := Placement{Medium: "disk", Archives: []PlacedArchive{
		{DLE: "etc", Level: 0}, {DLE: "home", Level: 1},
	}}
	if missing := full.Missing(archives); len(missing) != 0 {
		t.Fatalf("full copy Missing = %d, want 0", len(missing))
	}
	partial := Placement{Medium: "s3", Archives: []PlacedArchive{{DLE: "home", Level: 1}}}
	if missing := partial.Missing(archives); len(missing) != 1 || missing[0].DLE != "etc" {
		t.Fatalf("partial copy Missing = %v, want [etc L0]", missing)
	}
	// Same DLE at another level (a different cycle position) is a different archive.
	stale := Placement{Medium: "s3", Archives: []PlacedArchive{{DLE: "home", Level: 0}}}
	if missing := stale.Missing(archives); len(missing) != 2 {
		t.Fatalf("wrong-level archive counted as coverage: missing=%d, want 2", len(missing))
	}
}
