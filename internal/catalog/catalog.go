// Package catalog is NBackup's local cache and bookkeeping layer, analogous to
// Amanda's curinfo/tapelist/catalog databases. Because the media store may be
// slow or offline (tape, Glacier), the catalog keeps a local index of slots so
// that planning, listing, restore-location, pruning, and budget reporting never
// touch the media. The slots on the store remain the source of truth: the cache
// is a projection of them and can be rebuilt by rescanning the store. Per-DLE
// run History is derived from the index, so it is never separately persisted.
//
// The only non-derivable local state is the GNU tar snapshot library (.snar
// files), which lives in the workdir and is precious — losing it forces a full.
package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/slot"
)

const (
	// CacheFile is the slot-index cache stored in the workdir.
	CacheFile = "catalog.json"
	// DirSnapshots holds per-DLE, per-level GNU tar snapshot files.
	DirSnapshots = "snapshots"
)

// Catalog is a local cache of sealed slots plus the snapshot library. It holds
// no long-lived store reference; the store is passed in only to refresh.
type Catalog struct {
	workdir string
	index   []*slot.Slot // cached sealed slots, sorted in run order
	loaded  bool         // whether the cache was loaded or refreshed this session
}

// Open loads the catalog cache from the workdir. If the cache file is absent,
// the catalog is empty and not yet loaded (EnsureFresh will populate it).
func Open(workdir string) (*Catalog, error) {
	c := &Catalog{workdir: workdir}
	data, err := os.ReadFile(filepath.Join(workdir, CacheFile))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	var slots []*slot.Slot
	if err := json.Unmarshal(data, &slots); err != nil {
		return nil, fmt.Errorf("parse catalog cache: %w", err)
	}
	c.setIndex(slots)
	c.loaded = true
	return c, nil
}

// EnsureFresh populates an empty cache from the store the first time it is
// needed (e.g. a lost cache or a catalog created before caching). If slots are
// found, the cache is materialized to disk once. Up-to-date caches are left
// untouched; out-of-band drift is reconciled with an explicit Rebuild.
func (c *Catalog) EnsureFresh(store media.Store) error {
	if c.loaded {
		return nil
	}
	if err := c.refresh(store); err != nil {
		return err
	}
	c.loaded = true
	if len(c.index) > 0 {
		return c.persist()
	}
	return nil
}

// Rebuild rescans the store and replaces the cache, persisting it. This is the
// explicit reconcile path (the only operation that reads every slot's metadata
// off the media).
func (c *Catalog) Rebuild(store media.Store) (int, error) {
	if err := c.refresh(store); err != nil {
		return 0, err
	}
	c.loaded = true
	if err := c.persist(); err != nil {
		return 0, err
	}
	return len(c.index), nil
}

// refresh reads sealed slots from the store into the in-memory index.
func (c *Catalog) refresh(store media.Store) error {
	ids, err := store.ListSlots()
	if err != nil {
		return err
	}
	var slots []*slot.Slot
	for _, id := range ids {
		s, err := readSlot(store, id)
		if err != nil || !s.IsSealed() {
			continue // skip partial/open/unreadable slots
		}
		slots = append(slots, s)
	}
	c.setIndex(slots)
	return nil
}

// Slots returns the cached sealed slots in run order.
func (c *Catalog) Slots() []*slot.Slot { return c.index }

// ReadSlot returns a cached slot by ID.
func (c *Catalog) ReadSlot(id string) (*slot.Slot, error) {
	for _, s := range c.index {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, fmt.Errorf("slot %s not in catalog (run `nb catalog rebuild` if it exists on media)", id)
}

// TotalBytes sums the recorded compressed size across cached slots.
func (c *Catalog) TotalBytes() int64 {
	var total int64
	for _, s := range c.index {
		total += s.TotalBytes
	}
	return total
}

// Add records a newly sealed slot in the cache and persists it.
func (c *Catalog) Add(s *slot.Slot) error {
	filtered := c.index[:0:0]
	for _, e := range c.index {
		if e.ID != s.ID {
			filtered = append(filtered, e)
		}
	}
	c.setIndex(append(filtered, s))
	c.loaded = true
	return c.persist()
}

// Remove drops a slot from the cache and persists it.
func (c *Catalog) Remove(id string) error {
	var kept []*slot.Slot
	for _, s := range c.index {
		if s.ID != id {
			kept = append(kept, s)
		}
	}
	c.setIndex(kept)
	return c.persist()
}

// History derives per-DLE run history from the cached slots (source of truth).
func (c *Catalog) History() *History {
	h := &History{DLEs: map[string]*DLEState{}}
	for _, s := range c.index { // already in run order
		for _, a := range s.Archives {
			d := h.DLE(a.DLE)
			d.Runs = append(d.Runs, RunRecord{Date: s.Date, Slot: s.ID, Level: a.Level})
			if a.Level == 0 {
				d.LastFullDate = s.Date
				d.LastFullSlot = s.ID
				d.LastFullBytes = a.Compressed
			}
		}
	}
	return h
}

// SnapshotPath is the local location of a DLE's snapshot for a given level.
func (c *Catalog) SnapshotPath(dleName string, level int) string {
	return filepath.Join(c.workdir, DirSnapshots, dleName, fmt.Sprintf("L%d.snar", level))
}

// SnapshotExists reports whether a snapshot file exists for the level.
func (c *Catalog) SnapshotExists(dleName string, level int) bool {
	_, err := os.Stat(c.SnapshotPath(dleName, level))
	return err == nil
}

func (c *Catalog) setIndex(slots []*slot.Slot) {
	sort.Slice(slots, func(i, j int) bool { return slot.Less(slots[i], slots[j]) })
	c.index = slots
}

func (c *Catalog) persist() error {
	if err := os.MkdirAll(c.workdir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.index, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := filepath.Join(c.workdir, CacheFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(c.workdir, CacheFile))
}

// readSlot loads a single slot's metadata from the store.
func readSlot(store media.Store, id string) (*slot.Slot, error) {
	rc, err := store.Open(id, slot.FileSlot)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return slot.ParseSlot(data)
}

// SealedID reports whether a slot ID exists and is sealed on the store. Used by
// the write path to allocate non-colliding slot IDs.
func SealedID(store media.Store, id string) (bool, error) {
	s, err := readSlot(store, id)
	if err != nil {
		return false, err
	}
	return s.IsSealed(), nil
}
