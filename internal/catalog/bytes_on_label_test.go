package catalog

import (
	"testing"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/record"
)

// testCost is a fake medium's file-cost rule for exercising BytesOnLabel: framing
// of 10 per file, unrecorded meta payloads (label, commit) bounded at 100. Fake
// numbers on purpose — the catalog must stay neutral and just apply the rule.
func testCost(kind string, payload int64) int64 {
	switch kind {
	case record.KindLabel, record.KindCommit:
		payload = 100
	}
	return 10 + payload
}

// TestBytesOnLabel locks the derived volume fill: a pure walk over recorded
// placements priced by the medium's cost rule — a spanning archive charges only
// its on-label parts (exact seals), the index its recorded IndexSize, label and
// commit their bounds, and a sealless record falls back to its whole Compressed
// size (conservative: rolls early, never past EOT).
func TestBytesOnLabel(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// A spanning archive: 1000 bytes on T1, 2000 on T2 (sealed per part), its
	// index (300 bytes recorded in the footer) and commit both on T2.
	span := record.Archive{Run: "r1", DLE: "h:/a", Level: 0, Compressed: 3000, IndexSize: 300,
		PartSeals: []record.PartSeal{{Size: 1000}, {Size: 2000}}}
	if err := c.AddArchive(span, "tapes", archiveio.ArchivePos{
		Parts:  []archiveio.FilePos{{Label: "T1", Epoch: 1, Pos: 1}, {Label: "T2", Epoch: 1, Pos: 1}},
		Commit: archiveio.FilePos{Label: "T2", Epoch: 1, Pos: 3},
		Index:  archiveio.FilePos{Label: "T2", Epoch: 1, Pos: 2},
	}); err != nil {
		t.Fatal(err)
	}
	// A sealless record on T1 (an old footer / partial scan): charged whole.
	old := record.Archive{Run: "r2", DLE: "h:/b", Level: 0, Compressed: 999}
	if err := c.AddArchive(old, "tapes", archiveio.ArchivePos{
		Parts:  []archiveio.FilePos{{Label: "T1", Epoch: 1, Pos: 3}},
		Commit: archiveio.FilePos{Label: "T1", Epoch: 1, Pos: 4},
	}); err != nil {
		t.Fatal(err)
	}
	// An address-identified copy (no labels) must never count anywhere.
	disk := record.Archive{Run: "r3", DLE: "h:/c", Level: 0, Compressed: 5000,
		PartSeals: []record.PartSeal{{Size: 5000}}}
	if err := c.AddArchive(disk, "disk", archiveio.ArchivePos{
		Parts:  []archiveio.FilePos{{Pos: 0}},
		Commit: archiveio.FilePos{Pos: 1},
	}); err != nil {
		t.Fatal(err)
	}

	label := testCost(record.KindLabel, 0)   // 110, every labeled volume's file 0
	commit := testCost(record.KindCommit, 0) // 110, bounded
	for name, want := range map[string]int64{
		"T1": label + (10 + 1000) + (10 + 999) + commit, // span's T1 part + sealless whole archive + its commit
		"T2": label + (10 + 2000) + (10 + 300) + commit, // span's T2 part + index + commit
		"T3": label,                                     // a labeled volume holds at least its label file
		"":   0,                                         // must not match the unlabeled disk parts
	} {
		if got := c.BytesOnLabel(name, testCost); got != want {
			t.Errorf("BytesOnLabel(%q) = %d, want %d", name, got, want)
		}
	}
}
