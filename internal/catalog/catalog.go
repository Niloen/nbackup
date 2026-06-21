// Package catalog is NBackup's bookkeeping layer, analogous to Amanda's catalog
// and curinfo. It lists slots on a media.Store, owns the per-DLE run History,
// and manages the local snapshot library used for incrementals. Slots are the
// source of truth; the workdir holds operational state (history + snapshots)
// that stays local even when the Store is remote.
package catalog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/slot"
)

// DirSnapshots holds per-DLE, per-level GNU tar snapshot files in the workdir.
const DirSnapshots = "snapshots"

// Catalog ties a media.Store (where slots live) to local operational state.
type Catalog struct {
	store   media.Store
	workdir string
	hist    *History
}

// Open loads (or initializes) the catalog for a store and local workdir.
func Open(store media.Store, workdir string) (*Catalog, error) {
	h, err := loadHistory(workdir)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	return &Catalog{store: store, workdir: workdir, hist: h}, nil
}

// Store returns the underlying media store.
func (c *Catalog) Store() media.Store { return c.store }

// History returns the mutable run history.
func (c *Catalog) History() *History { return c.hist }

// SaveHistory persists the run history.
func (c *Catalog) SaveHistory() error { return c.hist.save(c.workdir) }

// SnapshotPath is the local location of a DLE's snapshot for a given level.
func (c *Catalog) SnapshotPath(dleName string, level int) string {
	return filepath.Join(c.workdir, DirSnapshots, dleName, fmt.Sprintf("L%d.snar", level))
}

// Slots returns all readable, sealed-or-open slots, ordered by run order.
func (c *Catalog) Slots() ([]*slot.Slot, error) {
	ids, err := c.store.ListSlots()
	if err != nil {
		return nil, err
	}
	var slots []*slot.Slot
	for _, id := range ids {
		s, err := c.ReadSlot(id)
		if err != nil {
			continue // skip partial/unreadable slots
		}
		slots = append(slots, s)
	}
	sort.Slice(slots, func(i, j int) bool { return slot.Less(slots[i], slots[j]) })
	return slots, nil
}

// ReadSlot loads a single slot's metadata from the store.
func (c *Catalog) ReadSlot(id string) (*slot.Slot, error) {
	rc, err := c.store.Open(id, slot.FileSlot)
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

// TotalBytes sums the recorded compressed size across all slots.
func (c *Catalog) TotalBytes() (int64, error) {
	slots, err := c.Slots()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, s := range slots {
		total += s.TotalBytes
	}
	return total, nil
}

// NextSlotID allocates the slot ID for a run on the given date: the first run of
// the day is "slot-DATE", later runs get the next free ".N". A leftover open
// (unsealed) slot from a failed attempt is removed and reused.
func (c *Catalog) NextSlotID(date time.Time) (id string, seq int, err error) {
	existing := map[string]bool{}
	ids, err := c.store.ListSlots()
	if err != nil {
		return "", 0, err
	}
	for _, e := range ids {
		existing[e] = true
	}
	day := slot.DateString(date)
	for seq = 1; ; seq++ {
		id = slot.IDFromParts(day, seq)
		if !existing[id] {
			return id, seq, nil
		}
		s, rerr := c.ReadSlot(id)
		if rerr != nil || !s.IsSealed() {
			// Unsealed or unreadable leftover: reclaim it.
			if err := c.store.Remove(id); err != nil {
				return "", 0, err
			}
			return id, seq, nil
		}
	}
}

// WorkdirSnapshotsExist reports whether a snapshot file exists for the level.
func (c *Catalog) SnapshotExists(dleName string, level int) bool {
	_, err := os.Stat(c.SnapshotPath(dleName, level))
	return err == nil
}
