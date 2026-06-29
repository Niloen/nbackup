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

// putSlot writes a committed slot's archive files the way the writer would: each archive's
// part(s), then its member index and commit footer — the per-archive marker.
func putSlot(t *testing.T, v media.Volume, s *record.Slot) {
	t.Helper()
	for _, a := range s.Archives {
		putPart(t, v, s.ID, a)
		putCommit(t, v, s.ID, a)
	}
}

// putPart writes one archive's part file with no commit footer — the orphan a crashed run leaves,
// which the rebuild must ignore.
func putPart(t *testing.T, v media.Volume, slotID string, a record.Archive) {
	t.Helper()
	h := record.Header{Slot: slotID, Kind: record.KindArchive, DLE: a.DLE, Level: a.Level, Compress: a.Compress}
	if _, err := writeFileT(v, h, func(w io.Writer) error {
		_, e := w.Write([]byte("payload"))
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

// putCommit writes an archive's member index (if any) then its commit footer, as Commit does.
func putCommit(t *testing.T, v media.Volume, slotID string, a record.Archive) {
	t.Helper()
	if len(a.Members) > 0 {
		if _, err := writeFileT(v, record.Header{Slot: slotID, Kind: record.KindIndex, DLE: a.DLE, Level: a.Level}, func(w io.Writer) error {
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
	if _, err := writeFileT(v, record.Header{Slot: slotID, Kind: record.KindCommit, DLE: a.DLE, Level: a.Level}, func(w io.Writer) error {
		_, e := w.Write(data)
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

func committedSlot(id, date string, seq int, archives ...record.Archive) *record.Slot {
	return &record.Slot{ID: id, Date: date, Sequence: seq, Archives: archives, TotalBytes: 100}
}

// placementPos finds an archive's first recorded part position in any of a slot's
// placements.
func placementPos(c *Catalog, slotID, dle string, level int) (int, bool) {
	for _, p := range c.Placements(slotID) {
		if parts, ok := p.Parts(dle, level); ok {
			return parts[0].Pos, true
		}
	}
	return 0, false
}

// TestCacheLifecycle covers refresh-from-volume, position indexing, persistence,
// reload without the volume, and history derivation.
func TestCacheLifecycle(t *testing.T) {
	dir := t.TempDir() // serves as both volume root and workdir
	vol := newVolume(t, dir)

	putSlot(t, vol, committedSlot("slot-2026-06-20", "2026-06-20", 1,
		record.Archive{DLE: "h-data", Level: 0, Compressed: 100}))
	putSlot(t, vol, committedSlot("slot-2026-06-21", "2026-06-21", 1,
		record.Archive{DLE: "h-data", Level: 1, Compressed: 100}))
	// A crashed run (archive parts but no commit footer) must be ignored by the rebuild.
	putPart(t, vol, "slot-2026-06-22", record.Archive{DLE: "h-data", Level: 1})

	// Cold open: no cache yet, then EnsureFresh populates and persists it.
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Slots()) != 0 {
		t.Fatalf("expected empty cache before EnsureFresh, got %d", len(cat.Slots()))
	}
	if err := cat.EnsureFresh("disk", vol); err != nil {
		t.Fatal(err)
	}
	if got := len(cat.Slots()); got != 2 {
		t.Fatalf("expected 2 sealed slots indexed, got %d", got)
	}
	if _, ok := placementPos(cat, "slot-2026-06-20", "h-data", 0); !ok {
		t.Errorf("expected a recorded position for slot-2026-06-20 h-data L0")
	}

	// Reopen: cache loads from disk; reads (incl. placements) work with NO volume.
	cat2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cat2.Slots()); got != 2 {
		t.Fatalf("reloaded cache has %d slots, want 2", got)
	}
	if _, ok := placementPos(cat2, "slot-2026-06-21", "h-data", 1); !ok {
		t.Errorf("reloaded cache lost the placement index")
	}
	if b := cat2.MediumBytes("disk"); b != 200 {
		t.Errorf("MediumBytes(disk) = %d, want 200", b)
	}

	// History is derived from the slots.
	h := cat2.History()
	d := h.DLE("h-data")
	if d.LastFullSlot != "slot-2026-06-20" {
		t.Errorf("last full = %q, want slot-2026-06-20", d.LastFullSlot)
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
// DLE's image from a slot that still holds other DLEs drops it from the slot's
// medium-independent content — so `nb dle` never lists an image no medium holds —
// while leaving the surviving DLE (and the slot entry) intact.
func TestRemoveArchiveDropsCopylessDLE(t *testing.T) {
	dir := t.TempDir()
	vol := newVolume(t, dir)

	putSlot(t, vol, committedSlot("slot-2026-06-20", "2026-06-20", 1,
		record.Archive{DLE: "h-leo", Level: 0, Compressed: 100},
		record.Archive{DLE: "h-shared", Level: 0, Compressed: 100}))

	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.EnsureFresh("disk", vol); err != nil {
		t.Fatal(err)
	}

	placementGone, entryGone, err := cat.RemoveArchive("slot-2026-06-20", "disk", "h-leo")
	if err != nil {
		t.Fatal(err)
	}
	if placementGone {
		t.Errorf("placementGone = true, want false (h-shared still holds the placement)")
	}
	if entryGone {
		t.Errorf("entryGone = true, want false (slot still has a surviving copy)")
	}

	slot, err := cat.ReadSlot("slot-2026-06-20")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range slot.Archives {
		if a.DLE == "h-leo" {
			t.Errorf("slot still lists h-leo after its last copy was removed: %+v", slot.Archives)
		}
	}
	if len(slot.Archives) != 1 || slot.Archives[0].DLE != "h-shared" {
		t.Errorf("slot archives = %+v, want only h-shared", slot.Archives)
	}
	if slot.TotalBytes != 100 {
		t.Errorf("TotalBytes = %d, want 100 (h-leo's 100 dropped)", slot.TotalBytes)
	}
}

// writePart writes one archive part (with its part index) onto the mounted volume.
func writePart(t *testing.T, v media.Volume, slotID, dle string, level, part int) int {
	t.Helper()
	pos, err := writeFileT(v, record.Header{Slot: slotID, Kind: record.KindArchive, DLE: dle, Level: level, Part: part},
		func(w io.Writer) error { _, e := w.Write([]byte("part-payload")); return e })
	if err != nil {
		t.Fatal(err)
	}
	return pos
}

// TestRebuildReassemblesSpannedSlot writes one archive's two parts across two library
// bays — part 0 + seal-less on the first, part 1 + the seal on the second — and
// confirms a rebuild reassembles a single placement spanning both volumes, with the
// parts in order and the seal on the second.
func TestRebuildReassemblesSpannedSlot(t *testing.T) {
	dir := t.TempDir()
	open := func() media.Volume {
		v, err := media.OpenVolume("tape", media.Options{"dir": dir, "bays": "2", "volume_size": "1048576"})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	vol := open()
	ch := vol.(media.Changer)
	lv := vol.(media.Labeled)
	now := time.Unix(0, 0).UTC()

	// Bay 1 holds part 0 of the archive (no seal — it is committed elsewhere).
	if err := ch.Mount("bay-01"); err != nil {
		t.Fatal(err)
	}
	if err := lv.WriteLabel(record.Label{Name: "vol-a", Pool: "tape", Epoch: 1, WrittenAt: now}); err != nil {
		t.Fatal(err)
	}
	writePart(t, vol, "slot-2026-06-21", "h-data", 0, 0)

	// Bay 2 holds part 1 and the commit footer that marks the (2-part) archive complete.
	if err := ch.Mount("bay-02"); err != nil {
		t.Fatal(err)
	}
	if err := lv.WriteLabel(record.Label{Name: "vol-b", Pool: "tape", Epoch: 1, WrittenAt: now}); err != nil {
		t.Fatal(err)
	}
	writePart(t, vol, "slot-2026-06-21", "h-data", 0, 1)
	putCommit(t, vol, "slot-2026-06-21", record.Archive{DLE: "h-data", Level: 0, Parts: 2})

	cat, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Rebuild(map[string]media.Volume{"tape": open()}); err != nil {
		t.Fatal(err)
	}

	ps := cat.Placements("slot-2026-06-21")
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
