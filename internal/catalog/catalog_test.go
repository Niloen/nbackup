package catalog

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/slot"

	_ "github.com/Niloen/nbackup/internal/media/localdisk"
)

func newStore(t *testing.T, path string) media.Store {
	t.Helper()
	s, err := media.OpenStore("local-disk", media.Options{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func putSlot(t *testing.T, store media.Store, s *slot.Slot) {
	t.Helper()
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(s.ID, slot.FileSlot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func sealed(id, date string, seq int, archives ...slot.Archive) *slot.Slot {
	return &slot.Slot{ID: id, Date: date, Sequence: seq, Status: slot.StatusSealed,
		SealedAt: time.Unix(0, 0).UTC(), Archives: archives, TotalBytes: 100}
}

// TestCacheLifecycle covers refresh-from-store, persistence, reload without the
// store, and history derivation.
func TestCacheLifecycle(t *testing.T) {
	dir := t.TempDir() // serves as both store root and workdir
	store := newStore(t, dir)

	putSlot(t, store, sealed("slot-2026-06-20", "2026-06-20", 1,
		slot.Archive{DLE: "h-data", Level: 0, File: "archives/h-data-L0.tar.zst"}))
	putSlot(t, store, sealed("slot-2026-06-21", "2026-06-21", 1,
		slot.Archive{DLE: "h-data", Level: 1, File: "archives/h-data-L1.tar.zst"}))
	// An open (unsealed) slot must be ignored by the cache.
	putSlot(t, store, &slot.Slot{ID: "slot-2026-06-22", Date: "2026-06-22", Sequence: 1, Status: slot.StatusOpen})

	// Cold open: no cache yet, then EnsureFresh populates and persists it.
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Slots()) != 0 {
		t.Fatalf("expected empty cache before EnsureFresh, got %d", len(cat.Slots()))
	}
	if err := cat.EnsureFresh(store); err != nil {
		t.Fatal(err)
	}
	if got := len(cat.Slots()); got != 2 {
		t.Fatalf("expected 2 sealed slots indexed, got %d", got)
	}

	// Reopen: cache loads from disk; reads work with NO store involved.
	cat2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cat2.Slots()); got != 2 {
		t.Fatalf("reloaded cache has %d slots, want 2", got)
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
	n, err := cat2.Rebuild(store)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rebuild indexed %d, want 2", n)
	}
}
