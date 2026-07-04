package catalog

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"

	_ "github.com/Niloen/nbackup/internal/media/disk"
	_ "github.com/Niloen/nbackup/internal/media/tape"
)

func newVolume(t *testing.T, path string) media.Volume {
	t.Helper()
	v, err := media.OpenVolume("disk", media.Options{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// putRun writes a committed run's archive files the way the writer would: each archive's
// part(s), then its member index and commit footer — the per-archive marker.
func putRun(t *testing.T, v media.Volume, s *Run) {
	t.Helper()
	for _, a := range s.Archives {
		putPart(t, v, s.ID, a)
		putCommit(t, v, s.ID, a)
	}
}

// putPart writes one archive's part file with no commit footer — the orphan a crashed run leaves,
// which the rebuild must ignore.
func putPart(t *testing.T, v media.Volume, runID string, a record.Archive) {
	t.Helper()
	h := record.Header{Run: runID, Kind: record.KindArchive, DLE: a.DLE, Level: a.Level, Compress: a.Compress}
	if _, err := writeFileT(v, h, func(w io.Writer) error {
		_, e := w.Write([]byte("payload"))
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

// putCommit writes an archive's member index (if any) then its commit footer, as Commit does.
func putCommit(t *testing.T, v media.Volume, runID string, a record.Archive) {
	t.Helper()
	if len(a.Members) > 0 {
		if _, err := writeFileT(v, record.Header{Run: runID, Kind: record.KindIndex, DLE: a.DLE, Level: a.Level}, func(w io.Writer) error {
			return record.EncodeIndex(w, a.Members)
		}); err != nil {
			t.Fatal(err)
		}
	}
	footer := a
	footer.Members = nil
	data, err := record.MarshalCommit(footer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeFileT(v, record.Header{Run: runID, Kind: record.KindCommit, DLE: a.DLE, Level: a.Level}, func(w io.Writer) error {
		_, e := w.Write(data)
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

func committedRun(id, date string, seq int, archives ...record.Archive) *Run {
	_, _ = date, seq // the date and sequence are encoded in the id
	for i := range archives {
		archives[i].Run = id
	}
	return &Run{ID: id, Archives: archives}
}

// placementPos finds an archive's first recorded part position in any of a run's
// placements.
func placementPos(c *Catalog, runID, dle string, level int) (int, bool) {
	for _, p := range c.Placements(runID) {
		if parts, ok := p.Parts(dle, level); ok {
			return parts[0].Pos, true
		}
	}
	return 0, false
}

// TestOrphanFiles pins the prune sweep's detector: it returns exactly the files no
// committed archive references — a crashed run's footer-less parts — and never a file a
// commit footer covers. Detection takes only the volume (no *Catalog argument), so it is
// structurally cache-independent: a stale or empty cache cannot make a committed archive
// look orphaned, because the same footer that proves an archive good is read here.
func TestOrphanFiles(t *testing.T) {
	dir := t.TempDir()
	vol := newVolume(t, dir)

	// One committed archive (part + commit footer) and, in another run, a footer-less
	// orphan part a crashed run left behind.
	putRun(t, vol, committedRun("run-2026-06-20.001", "2026-06-20", 1,
		record.Archive{DLE: "h-data", Level: 0, Compressed: 100}))
	putPart(t, vol, "run-2026-06-22.001", record.Archive{DLE: "h-data", Level: 1})

	orphans, err := OrphanFiles(vol, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 {
		t.Fatalf("OrphanFiles returned %d files, want 1 (the footer-less part only): %+v", len(orphans), orphans)
	}
	if got := orphans[0].Header.Run; got != "run-2026-06-22.001" {
		t.Fatalf("orphan run = %q, want run-2026-06-22.001 (the committed archive must be spared)", got)
	}
}

// TestCacheLifecycle covers refresh-from-volume, position indexing, persistence,
// reload without the volume, and history derivation.
func TestCacheLifecycle(t *testing.T) {
	dir := t.TempDir() // serves as both volume root and workdir
	vol := newVolume(t, dir)

	putRun(t, vol, committedRun("run-2026-06-20.001", "2026-06-20", 1,
		record.Archive{DLE: "h-data", Level: 0, Compressed: 100}))
	putRun(t, vol, committedRun("run-2026-06-21.001", "2026-06-21", 1,
		record.Archive{DLE: "h-data", Level: 1, Compressed: 100}))
	// A crashed run (archive parts but no commit footer) must be ignored by the rebuild.
	putPart(t, vol, "run-2026-06-22.001", record.Archive{DLE: "h-data", Level: 1})

	// Cold open: no cache yet, then EnsureFresh populates and persists it.
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Runs()) != 0 {
		t.Fatalf("expected empty cache before EnsureFresh, got %d", len(cat.Runs()))
	}
	if err := cat.EnsureFresh("disk", vol); err != nil {
		t.Fatal(err)
	}
	if got := len(cat.Runs()); got != 2 {
		t.Fatalf("expected 2 sealed runs indexed, got %d", got)
	}
	if _, ok := placementPos(cat, "run-2026-06-20.001", "h-data", 0); !ok {
		t.Errorf("expected a recorded position for run-2026-06-20.001 h-data L0")
	}

	// Reopen: cache loads from disk; reads (incl. placements) work with NO volume.
	cat2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cat2.Runs()); got != 2 {
		t.Fatalf("reloaded cache has %d runs, want 2", got)
	}
	if _, ok := placementPos(cat2, "run-2026-06-21.001", "h-data", 1); !ok {
		t.Errorf("reloaded cache lost the placement index")
	}
	if b := cat2.MediumBytes("disk"); b != 200 {
		t.Errorf("MediumBytes(disk) = %d, want 200", b)
	}

	// History is derived from the runs.
	h := cat2.History()
	d := h.DLE("h-data")
	if d.LastFullRun != "run-2026-06-20.001" {
		t.Errorf("last full = %q, want run-2026-06-20.001", d.LastFullRun)
	}
	if d.IncrementalsSinceFull() != 1 {
		t.Errorf("incrementals since full = %d, want 1", d.IncrementalsSinceFull())
	}

	// Rebuild reconciles and reports the count.
	n, err := cat2.Rebuild(map[string]media.Volume{"disk": vol})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rebuild indexed %d, want 2", n)
	}
}

// TestRemoveArchiveDropsCopylessDLE confirms that reclaiming the last copy of one
// DLE's image from a run that still holds other DLEs drops it from the run's
// medium-independent content — so `nb dle` never lists an image no medium holds —
// while leaving the surviving DLE (and the run entry) intact.
func TestRemoveArchiveDropsCopylessDLE(t *testing.T) {
	dir := t.TempDir()
	vol := newVolume(t, dir)

	putRun(t, vol, committedRun("run-2026-06-20.001", "2026-06-20", 1,
		record.Archive{DLE: "h-removed", Level: 0, Compressed: 100},
		record.Archive{DLE: "h-shared", Level: 0, Compressed: 100}))

	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.EnsureFresh("disk", vol); err != nil {
		t.Fatal(err)
	}

	placementGone, entryGone, err := cat.RemoveArchive("run-2026-06-20.001", "disk", "h-removed")
	if err != nil {
		t.Fatal(err)
	}
	if placementGone {
		t.Errorf("placementGone = true, want false (h-shared still holds the placement)")
	}
	if entryGone {
		t.Errorf("entryGone = true, want false (run still has a surviving copy)")
	}

	run, err := cat.ReadRun("run-2026-06-20.001")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range run.Archives {
		if a.DLE == "h-removed" {
			t.Errorf("run still lists h-removed after its last copy was removed: %+v", run.Archives)
		}
	}
	if len(run.Archives) != 1 || run.Archives[0].DLE != "h-shared" {
		t.Errorf("run archives = %+v, want only h-shared", run.Archives)
	}
	if run.TotalBytes() != 100 {
		t.Errorf("TotalBytes = %d, want 100 (h-removed's 100 dropped)", run.TotalBytes())
	}
}

// TestPerMediumQueriesAreArchiveGranular pins the copy record's granularity: after a
// per-archive prune reclaims one DLE's image from ONE medium (RemoveArchive), the
// per-medium projections — ArchivesOn and MediumBytes, which drive prune candidacy,
// retention floors, and `nb medium` usage — must stop attributing the pruned archive
// to that medium, while the surviving copy on another medium keeps advertising it.
// (The regression: a run-level "placedOn" attribution re-listed the pruned archive on
// the pruned medium forever — a non-idempotent prune and an "(over!)" usage figure.)
func TestPerMediumQueriesAreArchiveGranular(t *testing.T) {
	dir := t.TempDir()
	vol := newVolume(t, dir)

	putRun(t, vol, committedRun("run-2026-06-20.001", "2026-06-20", 1,
		record.Archive{DLE: "h-pruned", Level: 0, Compressed: 300},
		record.Archive{DLE: "h-kept", Level: 0, Compressed: 100}))

	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.EnsureFresh("disk", vol); err != nil {
		t.Fatal(err)
	}
	// A second copy of both archives on another medium (a sync target), so the run
	// still holds h-pruned somewhere after the disk copy sheds it.
	for _, a := range cat.Runs()[0].Archives {
		pa, ok := findPlaced(cat.Placements("run-2026-06-20.001")[0].Archives, a.DLE, a.Level)
		if !ok {
			t.Fatalf("no disk position for %s", a.DLE)
		}
		if err := cat.AddArchive(a, "mirror", pa.Pos()); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := cat.RemoveArchive("run-2026-06-20.001", "disk", "h-pruned"); err != nil {
		t.Fatal(err)
	}

	// The pruned medium: only the surviving archive counts.
	on := cat.ArchivesOn("disk")
	if len(on) != 1 || on[0].DLE != "h-kept" {
		t.Errorf("ArchivesOn(disk) = %+v, want only h-kept", on)
	}
	if b := cat.MediumBytes("disk"); b != 100 {
		t.Errorf("MediumBytes(disk) = %d, want 100 (h-pruned's 300 must not count)", b)
	}
	// The surviving copy still advertises both.
	if got := len(cat.ArchivesOn("mirror")); got != 2 {
		t.Errorf("ArchivesOn(mirror) = %d archives, want 2", got)
	}
	if b := cat.MediumBytes("mirror"); b != 400 {
		t.Errorf("MediumBytes(mirror) = %d, want 400", b)
	}
	// The run's medium-independent content keeps h-pruned (the mirror holds it).
	run, err := cat.ReadRun("run-2026-06-20.001")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Archives) != 2 {
		t.Errorf("run content = %+v, want both archives (a copy of h-pruned survives)", run.Archives)
	}
	// Holds mirrors the record: the disk copy no longer claims h-pruned.
	for _, p := range cat.Placements("run-2026-06-20.001") {
		if p.Medium == "disk" && p.Holds("h-pruned", 0) {
			t.Errorf("disk placement still claims h-pruned")
		}
		if p.Medium == "mirror" && !p.Holds("h-pruned", 0) {
			t.Errorf("mirror placement lost h-pruned")
		}
	}
}

// TestRebuildReflectsPartiallyPrunedMedium pins symptom (d) of the prune desync: a
// rebuild scans the media themselves, so a medium a per-archive prune partially
// reclaimed must come back holding exactly the archives physically present — the
// pruned archive re-enters the catalog only via the medium that still holds it.
func TestRebuildReflectsPartiallyPrunedMedium(t *testing.T) {
	full := newVolume(t, t.TempDir())    // the sync target: still holds both archives
	partial := newVolume(t, t.TempDir()) // the pruned landing: h-pruned's files are gone

	putRun(t, full, committedRun("run-2026-06-20.001", "2026-06-20", 1,
		record.Archive{DLE: "h-pruned", Level: 0, Compressed: 300},
		record.Archive{DLE: "h-kept", Level: 0, Compressed: 100}))
	putRun(t, partial, committedRun("run-2026-06-20.001", "2026-06-20", 1,
		record.Archive{DLE: "h-kept", Level: 0, Compressed: 100}))

	cat, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Rebuild(map[string]media.Volume{"mirror": full, "disk": partial}); err != nil {
		t.Fatal(err)
	}

	if on := cat.ArchivesOn("disk"); len(on) != 1 || on[0].DLE != "h-kept" {
		t.Errorf("ArchivesOn(disk) after rebuild = %+v, want only h-kept (what the medium holds)", on)
	}
	if b := cat.MediumBytes("disk"); b != 100 {
		t.Errorf("MediumBytes(disk) after rebuild = %d, want 100", b)
	}
	if got := len(cat.ArchivesOn("mirror")); got != 2 {
		t.Errorf("ArchivesOn(mirror) after rebuild = %d archives, want 2", got)
	}
	run, err := cat.ReadRun("run-2026-06-20.001")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Archives) != 2 {
		t.Errorf("run content after rebuild = %+v, want both archives", run.Archives)
	}
}

// writePart writes one archive part (with its part index) onto the mounted volume.
func writePart(t *testing.T, v media.Volume, runID, dle string, level, part int) int {
	t.Helper()
	pos, err := writeFileT(v, record.Header{Run: runID, Kind: record.KindArchive, DLE: dle, Level: level, Part: part},
		func(w io.Writer) error { _, e := w.Write([]byte("part-payload")); return e })
	if err != nil {
		t.Fatal(err)
	}
	return pos
}

// TestRebuildReassemblesSpannedRun writes one archive's two parts across two library
// bays — part 0 + seal-less on the first, part 1 + the seal on the second — and
// confirms a rebuild reassembles a single placement spanning both volumes, with the
// parts in order and the seal on the second.
func TestRebuildReassemblesSpannedRun(t *testing.T) {
	dir := t.TempDir()
	open := func() media.Volume {
		v, err := media.OpenVolume("tape", media.Options{"dir": dir, "slots": "2", "volume_size": "1048576"})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	vol := open()
	ch := vol.(media.Changer)
	lv := vol.(media.Labeled)
	now := time.Unix(0, 0).UTC()

	// Run 1 holds part 0 of the archive (no seal — it is committed elsewhere).
	if err := ch.Load(1, 0); err != nil {
		t.Fatal(err)
	}
	if err := lv.WriteLabel(record.Label{Name: "vol-a", Pool: "tape", Epoch: 1, WrittenAt: now}); err != nil {
		t.Fatal(err)
	}
	writePart(t, vol, "run-2026-06-21.001", "h-data", 0, 0)

	// Run 2 holds part 1 and the commit footer that marks the (2-part) archive complete.
	if err := ch.Load(2, 0); err != nil {
		t.Fatal(err)
	}
	if err := lv.WriteLabel(record.Label{Name: "vol-b", Pool: "tape", Epoch: 1, WrittenAt: now}); err != nil {
		t.Fatal(err)
	}
	writePart(t, vol, "run-2026-06-21.001", "h-data", 0, 1)
	putCommit(t, vol, "run-2026-06-21.001", record.Archive{DLE: "h-data", Level: 0, Parts: 2})

	cat, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Rebuild(map[string]media.Volume{"tape": open()}); err != nil {
		t.Fatal(err)
	}

	ps := cat.Placements("run-2026-06-21.001")
	if len(ps) != 1 {
		t.Fatalf("placements = %d, want 1", len(ps))
	}
	p := ps[0]
	parts, ok := p.Parts("h-data", 0)
	if !ok || len(parts) != 2 {
		t.Fatalf("archive parts = %v (ok=%v), want 2", parts, ok)
	}
	if parts[0].Label != "vol-a" || parts[1].Label != "vol-b" {
		t.Fatalf("part volumes = %q,%q, want vol-a,vol-b", parts[0].Label, parts[1].Label)
	}
	if p.Archives[0].Commit.Label != "vol-b" {
		t.Fatalf("commit volume = %q, want vol-b", p.Archives[0].Commit.Label)
	}
	if got := p.Labels(); len(got) != 2 {
		t.Fatalf("placement volumes = %v, want 2", got)
	}
}

// TestForceFullDirectivePersists verifies the `nb reset` directive round-trips through the
// cache file, survives a Rebuild (it is operator intent, not media-derived, so a scan must
// not drop it), and is gone for good once cleared.
func TestForceFullDirectivePersists(t *testing.T) {
	dir := t.TempDir()
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SetForceFull("h-data"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.ForcedFulls()["h-data"] {
		t.Fatal("force-full directive should persist across Open")
	}
	if _, err := reopened.Rebuild(map[string]media.Volume{}); err != nil {
		t.Fatal(err)
	}
	if !reopened.ForcedFulls()["h-data"] {
		t.Fatal("force-full directive should survive a rebuild")
	}

	if err := reopened.ClearForceFulls(map[string]bool{"h-data": true}); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.ForcedFulls()) != 0 {
		t.Fatalf("cleared directive should not reappear, got %v", again.ForcedFulls())
	}
}

// writeFileT bridges tests to the writer-based AppendFile (callback shape kept for brevity).
func writeFileT(v media.Volume, h record.Header, write func(io.Writer) error) (int, error) {
	fw, err := v.AppendFile(context.Background(), h)
	if err != nil {
		return 0, err
	}
	if err := write(fw); err != nil {
		fw.Close()
		return 0, err
	}
	if err := fw.Close(); err != nil {
		return 0, err
	}
	return fw.Pos(), nil
}

// TestSetVolumeBarcode locks the learned barcode↔label memory behind slot-inventory
// display: the pairing is recorded when a label is actually read, follows a cartridge
// when its label moves (one volume per cartridge), and survives an identity upsert
// (a recycle bumps the epoch without forgetting where the tape lives).
func TestSetVolumeBarcode(t *testing.T) {
	cat, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordVolume(record.Label{Name: "T-A", Pool: "lto", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordVolume(record.Label{Name: "T-B", Pool: "lto", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetVolumeBarcode("T-A", "SIM0001"); err != nil {
		t.Fatal(err)
	}
	if v, _ := cat.Volume("T-A"); v.Barcode != "SIM0001" {
		t.Fatalf("T-A barcode = %q, want SIM0001", v.Barcode)
	}
	// Unknown volume and empty barcode are no-ops.
	if err := cat.SetVolumeBarcode("nope", "SIM0002"); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetVolumeBarcode("T-B", ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := cat.Volume("T-B"); v.Barcode != "" {
		t.Fatalf("T-B barcode = %q, want empty", v.Barcode)
	}
	// A cartridge holds one volume: relabeling it to T-B moves the barcode.
	if err := cat.SetVolumeBarcode("T-B", "SIM0001"); err != nil {
		t.Fatal(err)
	}
	if v, _ := cat.Volume("T-A"); v.Barcode != "" {
		t.Fatalf("T-A should have lost SIM0001, still has %q", v.Barcode)
	}
	// An identity upsert (recycle: epoch bump) keeps the learned pairing.
	if err := cat.RecordVolume(record.Label{Name: "T-B", Pool: "lto", Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	if v, _ := cat.Volume("T-B"); v.Barcode != "SIM0001" || v.Label.Epoch != 2 {
		t.Fatalf("T-B after recycle = %+v, want barcode SIM0001 epoch 2", v)
	}
}

// TestRebuildRestoresPartSeals: the per-part seals ride in the commit footer, so a
// rebuild restores them onto the placement (index-aligned with the part positions) and
// strips them from the run's medium-independent content. A scan that found only some
// parts drops the seals rather than mis-aligning them.
func TestRebuildRestoresPartSeals(t *testing.T) {
	dir := t.TempDir()
	vol := newVolume(t, dir)

	seals := []record.PartSeal{{Size: 4, SHA256: "aa"}, {Size: 3, SHA256: "bb"}}
	arch := record.Archive{Run: "run-2026-06-20.001", DLE: "h-data", Level: 0, Compressed: 7, Parts: 2, PartSeals: seals}
	for part := 0; part < 2; part++ {
		h := record.Header{Run: arch.Run, Kind: record.KindArchive, DLE: arch.DLE, Level: 0, Part: part}
		if _, err := writeFileT(vol, h, func(w io.Writer) error { _, e := w.Write([]byte("data")); return e }); err != nil {
			t.Fatal(err)
		}
	}
	putCommit(t, vol, arch.Run, arch)
	// A second archive whose footer claims 2 sealed parts but with only part 0 on the
	// medium: alignment is broken, so its seals must be dropped.
	partial := record.Archive{Run: arch.Run, DLE: "h-partial", Level: 0, Parts: 2, PartSeals: seals}
	if _, err := writeFileT(vol, record.Header{Run: arch.Run, Kind: record.KindArchive, DLE: "h-partial", Level: 0, Part: 0},
		func(w io.Writer) error { _, e := w.Write([]byte("data")); return e }); err != nil {
		t.Fatal(err)
	}
	putCommit(t, vol, arch.Run, partial)

	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Rebuild(map[string]media.Volume{"disk": vol}); err != nil {
		t.Fatal(err)
	}
	var pa PlacedArchive
	var ok bool
	for _, p := range cat.Placements(arch.Run) {
		if pa, ok = p.Placed("h-data", 0); ok {
			break
		}
	}
	if !ok {
		t.Fatal("rebuilt catalog lost the placement")
	}
	if len(pa.Seals) != 2 || pa.Seals[1] != seals[1] {
		t.Fatalf("rebuilt Seals = %+v, want the footer's %+v", pa.Seals, seals)
	}
	// The run's medium-independent content carries no seals (they are placement facts).
	s, err := cat.ReadRun(arch.Run)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Archive("h-data", 0); len(a.PartSeals) != 0 {
		t.Fatalf("run content carries %d seals, want 0 (stripped onto the placement)", len(a.PartSeals))
	}
	for _, p := range cat.Placements(arch.Run) {
		if ppa, ok := p.Placed("h-partial", 0); ok && len(ppa.Seals) != 0 {
			t.Fatalf("partial scan kept %d seals over %d found part(s) — misaligned", len(ppa.Seals), len(ppa.Parts))
		}
	}
}
