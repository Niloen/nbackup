package catalog

import (
	"io"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/slot"

	_ "github.com/Niloen/nbackup/internal/media/disk"
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
func putSlot(t *testing.T, v media.Volume, s *slot.Slot) {
	t.Helper()
	for _, a := range s.Archives {
		h := media.Header{Slot: s.ID, Kind: media.KindArchive, DLE: a.DLE, Level: a.Level, Codec: a.Codec}
		if _, err := v.AppendFile(h, func(w io.Writer) error {
			_, e := w.Write([]byte("payload"))
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	if s.Status != slot.StatusSealed {
		return
	}
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.AppendFile(media.Header{Slot: s.ID, Kind: media.KindSeal}, func(w io.Writer) error {
		_, e := w.Write(data)
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

func sealed(id, date string, seq int, archives ...slot.Archive) *slot.Slot {
	return &slot.Slot{ID: id, Date: date, Sequence: seq, Status: slot.StatusSealed,
		SealedAt: time.Unix(0, 0).UTC(), Archives: archives, TotalBytes: 100}
}

// TestCacheLifecycle covers refresh-from-volume, position indexing, persistence,
// reload without the volume, and history derivation.
func TestCacheLifecycle(t *testing.T) {
	dir := t.TempDir() // serves as both volume root and workdir
	vol := newVolume(t, dir)

	putSlot(t, vol, sealed("slot-2026-06-20", "2026-06-20", 1,
		slot.Archive{DLE: "h-data", Level: 0}))
	putSlot(t, vol, sealed("slot-2026-06-21", "2026-06-21", 1,
		slot.Archive{DLE: "h-data", Level: 1}))
	// An unsealed slot (archives but no seal) must be ignored by the cache.
	putSlot(t, vol, &slot.Slot{ID: "slot-2026-06-22", Date: "2026-06-22", Sequence: 1,
		Status: slot.StatusOpen, Archives: []slot.Archive{{DLE: "h-data", Level: 1}}})

	// Cold open: no cache yet, then EnsureFresh populates and persists it.
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Slots()) != 0 {
		t.Fatalf("expected empty cache before EnsureFresh, got %d", len(cat.Slots()))
	}
	if err := cat.EnsureFresh(vol); err != nil {
		t.Fatal(err)
	}
	if got := len(cat.Slots()); got != 2 {
		t.Fatalf("expected 2 sealed slots indexed, got %d", got)
	}
	if _, ok := cat.Position("slot-2026-06-20", "h-data", 0); !ok {
		t.Errorf("expected a recorded position for slot-2026-06-20 h-data L0")
	}

	// Reopen: cache loads from disk; reads (incl. positions) work with NO volume.
	cat2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cat2.Slots()); got != 2 {
		t.Fatalf("reloaded cache has %d slots, want 2", got)
	}
	if _, ok := cat2.Position("slot-2026-06-21", "h-data", 1); !ok {
		t.Errorf("reloaded cache lost the position index")
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
	n, err := cat2.Rebuild(vol)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rebuild indexed %d, want 2", n)
	}
}
