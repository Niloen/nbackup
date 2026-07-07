package catalog

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// spanLib builds a 3-reel manual station holding one archive spanning vol-a →
// vol-b → vol-c, the commit footer (with the part map — the archive's TOC — and
// aligned seals) on vol-c. A manual changer's scan reads ONLY the loaded reel, so
// tests feed reels one at a time exactly like an operator with a shoebox of tapes.
func spanLib(t *testing.T) (open func() media.Volume, load func(v media.Volume, slot int)) {
	t.Helper()
	dir := t.TempDir()
	open = func() media.Volume {
		v, err := media.OpenVolume("tape", media.Options{"dir": dir, "manual": "true", "slots": "3", "volume_size": "1048576"}, "")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	load = func(v media.Volume, slot int) {
		t.Helper()
		if err := v.(media.Changer).Load(slot, 0); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(0, 0).UTC()
	v := open()
	lv := v.(media.Labeled)

	var partPos [3]int
	for i, name := range []string{"vol-a", "vol-b", "vol-c"} {
		load(v, i+1)
		if err := lv.WriteLabel(record.Label{Name: name, Pool: "tape", Epoch: 1, WrittenAt: now}); err != nil {
			t.Fatal(err)
		}
		partPos[i] = writePart(t, v, "run-2026-06-21.001", "h-data", 0, i)
	}
	// The footer lands on vol-c (still loaded), carrying the full TOC + seals.
	putCommit(t, v, "run-2026-06-21.001", record.Archive{
		DLE: "h-data", Level: 0, Parts: 3, Compressed: 36,
		PartSeals: []record.PartSeal{{Size: 12}, {Size: 12}, {Size: 12}},
		PartMap: []record.PartLoc{
			{Label: "vol-a", Epoch: 1, Pos: partPos[0]},
			{Label: "vol-b", Epoch: 1, Pos: partPos[1]},
			{Label: "vol-c", Epoch: 1, Pos: partPos[2]},
		},
	})
	return open, load
}

// TestAdditiveRebuildGuidedByTOC is the disaster-recovery flow end to end: feed
// the commit-holding reel first — the footer's part map completes the placement
// (all three parts, aligned seals, correct part order) and names the two reels
// still missing; feed them one at a time with additive rebuilds until the
// worklist is empty. Re-scans are idempotent, including the stored fill.
func TestAdditiveRebuildGuidedByTOC(t *testing.T) {
	open, load := spanLib(t)
	c := priced(t) // prices medium "tapes"; rename below binds pool "tape" too
	c.PriceWith(func(medium string) (func(kind string, payload int64) int64, bool) {
		if medium != "tape" {
			return nil, false
		}
		return testCost, true
	})

	// Pass 1: only vol-c (the footer reel) is loaded.
	v := open()
	load(v, 3)
	rep, err := c.Rebuild(map[string]media.Volume{"tape": v}, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runs != 1 || len(rep.OrphanRuns) != 0 {
		t.Fatalf("footer reel alone should index the run cleanly, got %+v", rep)
	}
	ps := c.Placements("run-2026-06-21.001")
	if len(ps) != 1 || len(ps[0].Archives) != 1 {
		t.Fatalf("placements = %+v", ps)
	}
	pa := ps[0].Archives[0]
	if len(pa.Parts) != 3 || len(pa.Seals) != 3 {
		t.Fatalf("the TOC should complete the placement: parts=%d seals=%d", len(pa.Parts), len(pa.Seals))
	}
	if pa.Parts[0].Label != "vol-a" || pa.Parts[1].Label != "vol-b" || pa.Parts[2].Label != "vol-c" {
		t.Fatalf("part order must follow the TOC, got %+v", pa.Parts)
	}
	missing := c.MissingVolumes()
	if len(missing) != 2 || missing[0].Label != "vol-a" || missing[1].Label != "vol-b" {
		t.Fatalf("worklist should name the two unseen reels, got %+v", missing)
	}
	// Fill: only the scanned reel is registered and charged (its part + footer +
	// its own label); the missing reels carry no figure yet.
	vc, _ := c.Volume("vol-c")
	wantC := testCost(record.KindLabel, 0) + testCost(record.KindArchive, 12) + testCost(record.KindCommit, 0)
	if vc.Used != wantC {
		t.Fatalf("vol-c Used = %d, want %d", vc.Used, wantC)
	}

	// Pass 2: feed vol-a. The worklist shrinks; the reel gets registered + charged.
	v = open()
	load(v, 1)
	if _, err := c.Rebuild(map[string]media.Volume{"tape": v}, false); err != nil {
		t.Fatal(err)
	}
	if missing := c.MissingVolumes(); len(missing) != 1 || missing[0].Label != "vol-b" {
		t.Fatalf("after vol-a the worklist should be [vol-b], got %+v", missing)
	}
	va, _ := c.Volume("vol-a")
	wantA := testCost(record.KindLabel, 0) + testCost(record.KindArchive, 12)
	if va.Used != wantA {
		t.Fatalf("vol-a Used = %d, want %d", va.Used, wantA)
	}

	// Pass 3: feed vol-b — worklist empty, catalog complete. Re-feeding vol-c is
	// a net no-op (idempotent re-scan, fill included).
	v = open()
	load(v, 2)
	if _, err := c.Rebuild(map[string]media.Volume{"tape": v}, false); err != nil {
		t.Fatal(err)
	}
	if missing := c.MissingVolumes(); len(missing) != 0 {
		t.Fatalf("worklist should be empty, got %+v", missing)
	}
	v = open()
	load(v, 3)
	if _, err := c.Rebuild(map[string]media.Volume{"tape": v}, false); err != nil {
		t.Fatal(err)
	}
	if vc, _ := c.Volume("vol-c"); vc.Used != wantC {
		t.Fatalf("re-scan must not move vol-c's fill: %d, want %d", vc.Used, wantC)
	}
	if pa := c.Placements("run-2026-06-21.001")[0].Archives[0]; len(pa.Parts) != 3 {
		t.Fatalf("re-scan must keep the placement complete, got %d parts", len(pa.Parts))
	}
}

// TestAdditiveRebuildReportsFooterlessParts is the other half of the rescue: a
// parts-only reel scanned before any footer reel yields no run yet but a loud
// orphan report naming the run — the cue to keep feeding tapes. Once the footer
// reel arrives the run resolves and the report clears.
func TestAdditiveRebuildReportsFooterlessParts(t *testing.T) {
	open, load := spanLib(t)
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	v := open()
	load(v, 1) // vol-a: part 0 only, footer elsewhere
	rep, err := c.Rebuild(map[string]media.Volume{"tape": v}, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runs != 0 {
		t.Fatalf("no footer scanned yet: want 0 runs, got %d", rep.Runs)
	}
	if len(rep.OrphanRuns) != 1 || rep.OrphanRuns[0].Run != "run-2026-06-21.001" ||
		len(rep.OrphanRuns[0].Labels) != 1 || rep.OrphanRuns[0].Labels[0] != "vol-a" {
		t.Fatalf("the pass must report the footerless parts, got %+v", rep.OrphanRuns)
	}

	v = open()
	load(v, 3) // the footer reel resolves the run
	rep, err = c.Rebuild(map[string]media.Volume{"tape": v}, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runs != 1 || len(rep.OrphanRuns) != 0 {
		t.Fatalf("footer reel should resolve the run and clear the report, got %+v", rep)
	}
}

// TestAdditiveRebuildDropsRelabeledEpoch: a scanned reel is whole-volume truth —
// records referencing its label at an older epoch describe a physically wiped
// reel and must go, exactly as a live recycle reconciles them.
func TestAdditiveRebuildDropsRelabeledEpoch(t *testing.T) {
	open, load := spanLib(t)
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	v := open()
	load(v, 3)
	if _, err := c.Rebuild(map[string]media.Volume{"tape": v}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadRun("run-2026-06-21.001"); err != nil {
		t.Fatal("the run should be indexed before the relabel")
	}

	// Wipe vol-a with a new epoch (a recycle done behind the catalog's back),
	// then feed it to an additive rebuild.
	v = open()
	load(v, 1)
	if err := v.(media.Labeled).WriteLabel(record.Label{Name: "vol-a", Pool: "tape", Epoch: 2, WrittenAt: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	rep, err := c.Rebuild(map[string]media.Volume{"tape": v}, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runs != 0 {
		t.Fatalf("the spanning run lost a reel to the relabel and must drop, got %d runs", rep.Runs)
	}
	if missing := c.MissingVolumes(); len(missing) != 0 {
		t.Fatalf("no placements remain, so nothing should be reported missing, got %+v", missing)
	}
}

// TestRebuildSkipsForeignPoolCartridges: two pools sharing one physical changer
// must not bleed into each other — a cartridge labeled for another pool is that
// pool's to absorb (under its own medium name), never this one's. Regression for
// the mhvtl road-test finding where a fresh pool inherited 59 foreign archives.
func TestRebuildSkipsForeignPoolCartridges(t *testing.T) {
	dir := t.TempDir()
	open := func() media.Volume {
		v, err := media.OpenVolume("tape", media.Options{"dir": dir, "slots": "2", "volume_size": "1048576"}, "")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	now := time.Unix(0, 0).UTC()
	v := open()
	ch, lv := v.(media.Changer), v.(media.Labeled)

	// Slot 1: a cartridge of pool "mine" with one committed run.
	if err := ch.Load(1, 0); err != nil {
		t.Fatal(err)
	}
	if err := lv.WriteLabel(record.Label{Name: "mine-01", Pool: "mine", Epoch: 1, WrittenAt: now}); err != nil {
		t.Fatal(err)
	}
	putRun(t, v, committedRun("run-2026-06-21.001", "2026-06-21", 1,
		record.Archive{DLE: "h-mine", Level: 0, Compressed: 10}))

	// Slot 2: a cartridge of pool "theirs" with its own committed run.
	if err := ch.Load(2, 0); err != nil {
		t.Fatal(err)
	}
	if err := lv.WriteLabel(record.Label{Name: "theirs-01", Pool: "theirs", Epoch: 1, WrittenAt: now}); err != nil {
		t.Fatal(err)
	}
	putRun(t, v, committedRun("run-2026-06-22.001", "2026-06-22", 1,
		record.Archive{DLE: "h-theirs", Level: 0, Compressed: 10}))

	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := c.Rebuild(map[string]media.Volume{"mine": open()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runs != 1 {
		t.Fatalf("rebuilding pool \"mine\" indexed %d runs, want only its own 1", rep.Runs)
	}
	if on := c.ArchivesOn("mine"); len(on) != 1 || on[0].DLE != "h-mine" {
		t.Fatalf("ArchivesOn(mine) = %+v, want only h-mine", on)
	}
	if _, known := c.Volume("theirs-01"); known {
		t.Fatal("the foreign pool's cartridge must not enter this rebuild's registry")
	}
}
