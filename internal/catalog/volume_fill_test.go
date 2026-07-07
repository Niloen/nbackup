package catalog

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/record"
)

// testCost is a fake medium's file-cost rule for exercising the stored volume
// fill: framing of 10 per file, unrecorded meta payloads (label, commit) bounded
// at 100. Fake numbers on purpose — the catalog must stay neutral and just apply
// whatever rule the resolver hands it.
func testCost(kind string, payload int64) int64 {
	switch kind {
	case record.KindLabel, record.KindCommit:
		payload = 100
	}
	return 10 + payload
}

// priced opens a catalog with the fake cost rule wired for the "tapes" medium
// (and its pool of the same name), the way the engine wires the real resolver.
func priced(t *testing.T) *Catalog {
	t.Helper()
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.PriceWith(func(medium string) (func(kind string, payload int64) int64, bool) {
		if medium != "tapes" {
			return nil, false
		}
		return testCost, true
	})
	return c
}

// TestVolumeFillStored locks the stored fill's lifecycle: a labeled reel starts
// at its label file's charge, each placement mutation moves its own charge inside
// the same catalog write (add, per-archive remove, whole-placement remove — a
// spanning archive charging only its on-label parts via exact seals, the index
// via the footer's IndexSize, the commit at the bound), a same-epoch re-record
// keeps the figure, and a relabel (epoch bump — the physical wipe) restarts it.
func TestVolumeFillStored(t *testing.T) {
	c := priced(t)
	label := testCost(record.KindLabel, 0)   // 110
	commit := testCost(record.KindCommit, 0) // 110

	now := time.Now()
	for _, name := range []string{"T1", "T2"} {
		if err := c.RecordVolume(record.Label{Name: name, Pool: "tapes", Epoch: 1, WrittenAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	used := func(name string) int64 { v, _ := c.Volume(name); return v.Used }
	if used("T1") != label {
		t.Fatalf("fresh reel Used = %d, want the label file's %d", used("T1"), label)
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
	// An unpriced medium's copy must never move any reel's fill.
	disk := record.Archive{Run: "r3", DLE: "h:/c", Level: 0, Compressed: 5000,
		PartSeals: []record.PartSeal{{Size: 5000}}}
	if err := c.AddArchive(disk, "disk", archiveio.ArchivePos{
		Parts:  []archiveio.FilePos{{Pos: 0}},
		Commit: archiveio.FilePos{Pos: 1},
	}); err != nil {
		t.Fatal(err)
	}

	wantT1 := label + (10 + 1000) + (10 + 999) + commit // span's T1 part + sealless whole + its commit
	wantT2 := label + (10 + 2000) + (10 + 300) + commit // span's T2 part + index + commit
	if used("T1") != wantT1 || used("T2") != wantT2 {
		t.Fatalf("Used T1=%d T2=%d, want %d / %d", used("T1"), used("T2"), wantT1, wantT2)
	}

	// A re-copy of the same archive replaces its record: the old charge leaves
	// with it — no double count.
	if err := c.AddArchive(span, "tapes", archiveio.ArchivePos{
		Parts:  []archiveio.FilePos{{Label: "T1", Epoch: 1, Pos: 1}, {Label: "T2", Epoch: 1, Pos: 1}},
		Commit: archiveio.FilePos{Label: "T2", Epoch: 1, Pos: 3},
		Index:  archiveio.FilePos{Label: "T2", Epoch: 1, Pos: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if used("T1") != wantT1 || used("T2") != wantT2 {
		t.Fatalf("re-copy must not double-charge: T1=%d T2=%d, want %d / %d", used("T1"), used("T2"), wantT1, wantT2)
	}

	// Pruning the sealless image gives its charge back to T1.
	if _, _, err := c.RemoveArchive("r2", "tapes", "h:/b"); err != nil {
		t.Fatal(err)
	}
	if got, want := used("T1"), label+(10+1000); got != want {
		t.Fatalf("Used T1 after prune = %d, want %d", got, want)
	}
	// Dropping the spanning run's whole copy gives back both reels' charges.
	if _, err := c.RemovePlacement("r1", "tapes"); err != nil {
		t.Fatal(err)
	}
	if used("T1") != label || used("T2") != label {
		t.Fatalf("Used after placement drop: T1=%d T2=%d, want label-only %d", used("T1"), used("T2"), label)
	}

	// Same-epoch re-record keeps the figure; an epoch bump (relabel) restarts it.
	if err := c.RecordVolume(record.Label{Name: "T2", Pool: "tapes", Epoch: 1, WrittenAt: now}); err != nil {
		t.Fatal(err)
	}
	if used("T2") != label {
		t.Fatalf("same-epoch re-record moved Used to %d", used("T2"))
	}
	if err := c.AddArchive(old, "tapes", archiveio.ArchivePos{
		Parts:  []archiveio.FilePos{{Label: "T2", Epoch: 1, Pos: 1}},
		Commit: archiveio.FilePos{Label: "T2", Epoch: 1, Pos: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordVolume(record.Label{Name: "T2", Pool: "tapes", Epoch: 2, WrittenAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if used("T2") != label {
		t.Fatalf("relabel should restart Used at the label file, got %d", used("T2"))
	}
}
