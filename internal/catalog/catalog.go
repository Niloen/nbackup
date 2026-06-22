// Package catalog is NBackup's local cache and bookkeeping layer, analogous to
// Amanda's curinfo/tapelist/catalog databases. Because the media volume may be
// slow or offline (tape, Glacier), the catalog keeps a local index of slots — and
// of each archive's volume position — so planning, listing, restore-location,
// pruning, and budget reporting never touch the media. The volume remains the
// source of truth: it is self-describing (every file has a header, every slot a
// seal record), so the cache can be rebuilt by scanning it.
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

// ArchiveKey is the per-slot key identifying an archive by DLE and level.
func ArchiveKey(dle string, level int) string { return fmt.Sprintf("%s/%d", dle, level) }

// Catalog is a local cache of sealed slots plus a map of each archive's volume
// position. It holds no long-lived volume reference; the volume is passed in only
// to refresh.
type Catalog struct {
	workdir string
	index   []*slot.Slot              // cached sealed slots, sorted in run order
	pos     map[string]map[string]int // slotID -> ArchiveKey -> volume position
	loaded  bool
}

type cacheFile struct {
	Slots []*slot.Slot              `json:"slots"`
	Pos   map[string]map[string]int `json:"pos"`
}

// Open loads the catalog cache from the workdir. If the cache file is absent, the
// catalog is empty and not yet loaded (EnsureFresh will populate it).
func Open(workdir string) (*Catalog, error) {
	c := &Catalog{workdir: workdir, pos: map[string]map[string]int{}}
	data, err := os.ReadFile(filepath.Join(workdir, CacheFile))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse catalog cache: %w", err)
	}
	c.setIndex(cf.Slots)
	if cf.Pos != nil {
		c.pos = cf.Pos
	}
	c.loaded = true
	return c, nil
}

// EnsureFresh populates an empty cache from the volume the first time it is needed
// (a lost cache, or a catalog created before caching).
func (c *Catalog) EnsureFresh(vol media.Volume) error {
	if c.loaded {
		return nil
	}
	if err := c.refresh(vol); err != nil {
		return err
	}
	c.loaded = true
	if len(c.index) > 0 {
		return c.persist()
	}
	return nil
}

// Rebuild rescans the volume and replaces the cache, persisting it.
func (c *Catalog) Rebuild(vol media.Volume) (int, error) {
	if err := c.refresh(vol); err != nil {
		return 0, err
	}
	c.loaded = true
	if err := c.persist(); err != nil {
		return 0, err
	}
	return len(c.index), nil
}

// refresh rebuilds the in-memory index from the volume's self-index: it groups
// files by slot, reads each sealed slot's seal record for metadata, and records
// every archive's position.
func (c *Catalog) refresh(vol media.Volume) error {
	files, err := vol.Files()
	if err != nil {
		return err
	}
	bySlot := map[string][]media.FileInfo{}
	for _, f := range files {
		bySlot[f.Header.Slot] = append(bySlot[f.Header.Slot], f)
	}

	var slots []*slot.Slot
	pos := map[string]map[string]int{}
	for slotID, fs := range bySlot {
		sealPos := -1
		for _, f := range fs {
			if f.Header.Kind == media.KindSeal {
				sealPos = f.Pos
			}
		}
		if sealPos < 0 {
			continue // unsealed / incomplete slot
		}
		s, err := readSeal(vol, sealPos)
		if err != nil {
			continue // unreadable seal: skip
		}
		slots = append(slots, s)
		pm := map[string]int{}
		for _, f := range fs {
			if f.Header.Kind == media.KindArchive {
				pm[ArchiveKey(f.Header.DLE, f.Header.Level)] = f.Pos
			}
		}
		pos[slotID] = pm
	}
	c.setIndex(slots)
	c.pos = pos
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

// Position returns the volume position of a slot's archive, or false if unknown.
func (c *Catalog) Position(slotID, dle string, level int) (int, bool) {
	m, ok := c.pos[slotID]
	if !ok {
		return 0, false
	}
	p, ok := m[ArchiveKey(dle, level)]
	return p, ok
}

// TotalBytes sums the recorded compressed size across cached slots.
func (c *Catalog) TotalBytes() int64 {
	var total int64
	for _, s := range c.index {
		total += s.TotalBytes
	}
	return total
}

// Add records a newly sealed slot and its archive positions, then persists.
func (c *Catalog) Add(s *slot.Slot, positions map[string]int) error {
	filtered := c.index[:0:0]
	for _, e := range c.index {
		if e.ID != s.ID {
			filtered = append(filtered, e)
		}
	}
	c.setIndex(append(filtered, s))
	c.pos[s.ID] = positions
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
	delete(c.pos, id)
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
	data, err := json.MarshalIndent(cacheFile{Slots: c.index, Pos: c.pos}, "", "  ")
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

// readSeal reads and parses a slot's seal-record payload from the volume.
func readSeal(vol media.Volume, pos int) (*slot.Slot, error) {
	_, rc, err := vol.ReadFile(pos)
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
