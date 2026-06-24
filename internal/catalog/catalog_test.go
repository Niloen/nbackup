package catalog

import (
	"io"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/format"
	"github.com/Niloen/nbackup/internal/media"

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

// putSlot writes a slot's archive files and (if sealed) its seal record onto the
// volume, the way the writer would.
func putSlot(t *testing.T, v media.Volume, s *format.Slot) {
	t.Helper()
	for _, a := range s.Archives {
		h := format.Header{Slot: s.ID, Kind: format.KindArchive, DLE: a.DLE, Level: a.Level, Codec: a.Codec}
		if _, err := v.AppendFile(h, func(w io.Writer) error {
			_, e := w.Write([]byte("payload"))
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	if s.Status != format.StatusSealed {
		return
	}
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.AppendFile(format.Header{Slot: s.ID, Kind: format.KindSeal}, func(w io.Writer) error {
		_, e := w.Write(data)
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

func sealed(id, date string, seq int, archives ...format.Archive) *format.Slot {
	return &format.Slot{ID: id, Date: date, Sequence: seq, Status: format.StatusSealed,
		SealedAt: time.Unix(0, 0).UTC(), Archives: archives, TotalBytes: 100}
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

	putSlot(t, vol, sealed("slot-2026-06-20", "2026-06-20", 1,
		format.Archive{DLE: "h-data", Level: 0}))
	putSlot(t, vol, sealed("slot-2026-06-21", "2026-06-21", 1,
		format.Archive{DLE: "h-data", Level: 1}))
	// An unsealed slot (archives but no seal) must be ignored by the cache.
	putSlot(t, vol, &format.Slot{ID: "slot-2026-06-22", Date: "2026-06-22", Sequence: 1,
		Status: format.StatusOpen, Archives: []format.Archive{{DLE: "h-data", Level: 1}}})

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

// writePart writes one archive part (with its part index) onto the mounted volume.
func writePart(t *testing.T, v media.Volume, slotID, dle string, level, part int) int {
	t.Helper()
	pos, err := v.AppendFile(format.Header{Slot: slotID, Kind: format.KindArchive, DLE: dle, Level: level, Part: part},
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
	if err := lv.WriteLabel(format.Label{Name: "vol-a", Pool: "tape", Epoch: 1, WrittenAt: now}); err != nil {
		t.Fatal(err)
	}
	writePart(t, vol, "slot-2026-06-21", "h-data", 0, 0)

	// Bay 2 holds part 1 and the seal that commits the (2-part) archive.
	if err := ch.Mount("bay-02"); err != nil {
		t.Fatal(err)
	}
	if err := lv.WriteLabel(format.Label{Name: "vol-b", Pool: "tape", Epoch: 1, WrittenAt: now}); err != nil {
		t.Fatal(err)
	}
	writePart(t, vol, "slot-2026-06-21", "h-data", 0, 1)
	s := sealed("slot-2026-06-21", "2026-06-21", 1, format.Archive{DLE: "h-data", Level: 0, Parts: 2})
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vol.AppendFile(format.Header{Slot: s.ID, Kind: format.KindSeal}, func(w io.Writer) error {
		_, e := w.Write(data)
		return e
	}); err != nil {
		t.Fatal(err)
	}

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
	if p.Seal.Label != "vol-b" {
		t.Fatalf("seal volume = %q, want vol-b", p.Seal.Label)
	}
	if got := p.Labels(); len(got) != 2 {
		t.Fatalf("placement volumes = %v, want 2", got)
	}
}
