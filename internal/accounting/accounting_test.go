package accounting

import (
	"testing"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/record"
)

// TestArchivePositionsCommitFirst pins the crash-safe removal order: the commit
// footer (the archive's marker) is reclaimed FIRST, then the index, then the parts —
// so a crash mid-prune leaves footer-less orphans a rebuild ignores, never a footer
// whose parts are already gone (which would rebuild into a phantom committed archive).
func TestArchivePositionsCommitFirst(t *testing.T) {
	ps := []catalog.Placement{{
		Medium: "disk",
		Archives: []catalog.ArchivePos{{
			DLE:    "app",
			Level:  0,
			Parts:  []record.FilePos{{Pos: 0}, {Pos: 1}},
			Index:  record.FilePos{Pos: 2},
			Commit: record.FilePos{Pos: 3},
		}},
	}}
	got := archivePositions(ps, "disk", "app")
	want := []int{3, 2, 0, 1} // commit, index, parts
	if len(got) != len(want) {
		t.Fatalf("positions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position[%d] = %d, want %d (full order %v, want %v)", i, got[i], want[i], got, want)
		}
	}
	// An archive with no member index omits that slot, still commit-first.
	ps[0].Archives[0].Index = record.FilePos{}
	if got := archivePositions(ps, "disk", "app"); len(got) != 3 || got[0] != 3 {
		t.Fatalf("without index, order = %v, want [3 0 1] (commit first)", got)
	}
}
