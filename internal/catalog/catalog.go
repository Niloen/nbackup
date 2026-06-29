// Package catalog is NBackup's local cache and bookkeeping layer. Because a
// media volume may be slow or offline (tape, Glacier), the catalog keeps a local
// index so planning, listing, restore-location, pruning, and capacity reporting
// never touch the media.
//
// Its model separates what a slot *is* from where its copies *are*: an Entry pairs
// one medium-independent slot (its content, from the seal record) with the set of
// Placements that hold a copy — each a volume plus the file position of every
// archive on it. The media remain the source of truth (every file self-describing,
// every slot sealed, every labeled volume identified), so the whole cache rebuilds
// by scanning: seals -> slots, labels -> the volume registry.
//
// The package has two faces. This file is the store: an in-memory index of Entries
// and VolumeRecords with queries, insert/update/delete, and JSON persistence — the
// "database" the rest of the system reads and writes. scan.go is the importer that
// rebuilds that store from the media (the source of truth); it hands finished
// placements back through the store's write path and never touches its fields.
//
// Almost everything the catalog holds is derivable from the media; the cache is a
// performance copy, not a system of record. The one exception is per-DLE operator intent
// (DLEMeta — today just the `nb reset` force-full directive): it cannot be scanned back, so
// it lives in the cache file beside the entries and is preserved across a Rebuild. An
// archiver's incremental state (gnutar's .snar library) is non-derivable too, but it is
// precious and belongs to the archiver, not here (see package archiver); the force-full
// directive, by contrast, is small and short-lived — a run consumes it.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Niloen/nbackup/internal/record"
)

// CacheFile is the catalog cache stored in the workdir.
const CacheFile = "catalog.json"

// VolumeRecord is the catalog's cached identity of a labeled volume. "Which
// slots are on it" and "is it reusable" are derived from placements + retention,
// not stored here.
type VolumeRecord struct {
	Label record.Label `json:"label"`
}

// Catalog is a local cache of slot entries plus a registry of labeled volumes. It
// holds no long-lived volume reference; volumes are passed in only to (re)scan.
type Catalog struct {
	workdir string
	entries []*Entry
	volumes map[string]*VolumeRecord // by volume label name
	dles    map[string]*DLEMeta      // per-DLE operator/planner metadata, by slug
	loaded  bool
}

type cacheFile struct {
	Entries []*Entry                 `json:"entries"`
	Volumes map[string]*VolumeRecord `json:"volumes,omitempty"`
	DLEs    map[string]*DLEMeta      `json:"dles,omitempty"`
}

// Open loads the catalog cache from the workdir. If the cache file is absent, the
// catalog is empty and not yet loaded (EnsureFresh will populate it).
func Open(workdir string) (*Catalog, error) {
	c := &Catalog{workdir: workdir, volumes: map[string]*VolumeRecord{}, dles: map[string]*DLEMeta{}}
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
	c.entries = cf.Entries
	if cf.Volumes != nil {
		c.volumes = cf.Volumes
	}
	if cf.DLEs != nil {
		c.dles = cf.DLEs
	}
	c.sortEntries()
	c.loaded = true
	return c, nil
}

// AddArchive merges one archive's content and its placement position into the catalog and
// persists — the catalog's single write path. A slot is only a grouping of committed
// archives, so there is no "add a slot": the entry is created from the archive's slot
// identity the first time one of its archives lands, and archives accrete into it one at a
// time. Both a dump (its Finish records each committed archive), a copy/sync, a rebuild scan,
// and the holding-disk taper write through here; reclaim is the symmetric RemoveArchive.
//
// Every catalog mutation is single-threaded (a run routes all placement writes through one
// goroutine), so no locking is needed.
func (c *Catalog) AddArchive(arch record.Archive, medium string, pos ArchivePos) error {
	c.addArchive(arch, medium, pos)
	c.sortEntries()
	return c.persist()
}

// addArchive is the in-memory merge AddArchive wraps: it creates the slot entry from the
// archive's own slot tag (arch.Slot) on first sight and merges the archive's content +
// placement position, but neither sorts nor persists — for a bulk caller (a rebuild scan)
// that persists once at the end. The catalog cache holds no member lists (they live in the
// member-index cache + the on-medium index), so members are cleared here.
func (c *Catalog) addArchive(arch record.Archive, medium string, pos ArchivePos) {
	e := c.entryByID(arch.Slot)
	if e == nil {
		e = &Entry{Slot: &Slot{ID: arch.Slot}}
		c.entries = append(c.entries, e)
	}
	arch.Members = nil
	e.Slot.addArchive(arch)
	e.addPlacementPos(medium, pos)
	c.loaded = true
}

// addPlacementPos records archive position pos on the entry's copy on medium, creating the
// placement if absent and replacing any prior position of the same (DLE, level).
func (e *Entry) addPlacementPos(medium string, pos ArchivePos) {
	for i := range e.Placements {
		if e.Placements[i].Medium == medium {
			e.Placements[i].Archives = mergeArchivePos(e.Placements[i].Archives, pos)
			return
		}
	}
	e.Placements = append(e.Placements, Placement{Medium: medium, Archives: []ArchivePos{pos}})
}

// mergeArchivePos returns list with pos added, replacing any entry of the same (DLE, level).
func mergeArchivePos(list []ArchivePos, pos ArchivePos) []ArchivePos {
	for i := range list {
		if list[i].DLE == pos.DLE && list[i].Level == pos.Level {
			list[i] = pos
			return list
		}
	}
	return append(list, pos)
}

// RemovePlacement drops the copy of a slot on one medium. When the last copy is
// gone the whole entry is removed (gone=true) — the slot no longer exists anywhere.
func (c *Catalog) RemovePlacement(slotID, medium string) (gone bool, err error) {
	e := c.entryByID(slotID)
	if e == nil {
		return false, nil
	}
	kept := e.Placements[:0:0]
	for _, p := range e.Placements {
		if p.Medium != medium {
			kept = append(kept, p)
		}
	}
	e.Placements = kept
	if len(e.Placements) == 0 {
		c.removeEntry(slotID)
		gone = true
	}
	return gone, c.persist()
}

// RemoveArchive drops one archive (a DLE's image) from the copy of a slot on one
// medium — the per-archive peer of RemovePlacement. It removes that DLE's ArchivePos
// from the medium's placement; when the placement keeps no archives the whole
// placement goes (placementGone), and when that was the slot's last copy the entry
// goes too (entryGone) — the slot no longer exists anywhere. When no remaining
// placement holds this DLE, the slot's medium-independent content
// (Entry.Slot.Archives) drops it too: the slot stops advertising an image no
// medium holds, even while it keeps other DLEs' images on surviving copies.
func (c *Catalog) RemoveArchive(slotID, medium, dle string) (placementGone, entryGone bool, err error) {
	e := c.entryByID(slotID)
	if e == nil {
		return false, false, nil
	}
	for i := range e.Placements {
		p := &e.Placements[i]
		if p.Medium != medium {
			continue
		}
		kept := p.Archives[:0:0]
		for _, a := range p.Archives {
			if a.DLE != dle {
				kept = append(kept, a)
			}
		}
		p.Archives = kept
		break
	}
	kept := e.Placements[:0:0]
	dleStillHeld := false
	for _, p := range e.Placements {
		if len(p.Archives) > 0 {
			kept = append(kept, p)
		} else {
			placementGone = true
		}
		for _, a := range p.Archives {
			if a.DLE == dle {
				dleStillHeld = true
				break
			}
		}
	}
	e.Placements = kept
	if !dleStillHeld {
		e.Slot.dropArchive(dle)
	}
	if len(e.Placements) == 0 {
		c.removeEntry(slotID)
		entryGone = true
	}
	return placementGone, entryGone, c.persist()
}

// RecordVolume upserts a labeled volume's identity in the registry, so a later run
// can detect a swapped or relabeled volume.
func (c *Catalog) RecordVolume(lbl record.Label) error {
	c.upsertVolume(lbl)
	return c.persist()
}

// upsertVolume records a labeled volume's identity in the registry without persisting —
// the in-memory write path shared by RecordVolume and the importer's absorb (which
// persists once at the end of a scan).
func (c *Catalog) upsertVolume(lbl record.Label) {
	c.volumes[lbl.Name] = &VolumeRecord{Label: lbl}
	c.loaded = true
}

// RemoveVolume drops a labeled volume from the registry. A relabel overwrites a
// tape's identity, so its old name no longer names a live volume and must stop
// counting as one (e.g. in the `nb medium` volume tally). A no-op if absent.
func (c *Catalog) RemoveVolume(name string) error {
	if _, ok := c.volumes[name]; !ok {
		return nil
	}
	delete(c.volumes, name)
	return c.persist()
}

// Slots returns the cached slots in run order.
func (c *Catalog) Slots() []*Slot {
	out := make([]*Slot, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.Slot)
	}
	return out
}

// ReadSlot returns a cached slot by ID.
func (c *Catalog) ReadSlot(id string) (*Slot, error) {
	if e := c.entryByID(id); e != nil {
		return e.Slot, nil
	}
	return nil, fmt.Errorf("slot %s not in catalog (run `nb rebuild` if it exists on media)", id)
}

// Placements returns the copies of a slot, for a reader to choose among.
func (c *Catalog) Placements(slotID string) []Placement {
	if e := c.entryByID(slotID); e != nil {
		return e.Placements
	}
	return nil
}

// SlotsOn returns the slots with a copy on the named medium, in run order.
func (c *Catalog) SlotsOn(medium string) []*Slot {
	var out []*Slot
	for _, e := range c.entries {
		if e.placedOn(medium) {
			out = append(out, e.Slot)
		}
	}
	return out
}

// SlotsOnLabel returns the slots with a copy on the volume with the given label,
// in run order — used to tell whether a tape already holds a run.
func (c *Catalog) SlotsOnLabel(label string) []*Slot {
	var out []*Slot
	for _, e := range c.entries {
		for _, p := range e.Placements {
			if p.OnLabel(label) {
				out = append(out, e.Slot)
				break
			}
		}
	}
	return out
}

// Archives returns every cached archive (each carrying its slot tag), across all slots, in
// run order — the corpus the policy layer (restore, recovery, drill) reasons over.
func (c *Catalog) Archives() []record.Archive {
	var out []record.Archive
	for _, e := range c.entries {
		out = append(out, e.Slot.Archives...)
	}
	return out
}

// ArchivesOn returns the archives of every slot with a copy on the named medium — the
// per-medium corpus retention and reclamation reason over. The slot's content is its archives
// across media, scoped here to slots present on the medium (matching SlotsOn).
func (c *Catalog) ArchivesOn(medium string) []record.Archive {
	var out []record.Archive
	for _, e := range c.entries {
		if e.placedOn(medium) {
			out = append(out, e.Slot.Archives...)
		}
	}
	return out
}

// SlotIDsOnLabel returns the ids of the slots with a copy on the volume with the given label,
// in run order — what a volume's reusability check (retention.Floor.First) consults.
func (c *Catalog) SlotIDsOnLabel(label string) []string {
	var out []string
	for _, e := range c.entries {
		for _, p := range e.Placements {
			if p.OnLabel(label) {
				out = append(out, e.Slot.ID)
				break
			}
		}
	}
	return out
}

// MediumBytes sums the stored bytes of slots with a copy on the named medium.
func (c *Catalog) MediumBytes(medium string) int64 {
	var total int64
	for _, e := range c.entries {
		if e.placedOn(medium) {
			total += e.Slot.TotalBytes()
		}
	}
	return total
}

// Volumes returns the volume registry, sorted by name.
func (c *Catalog) Volumes() []VolumeRecord {
	out := make([]VolumeRecord, 0, len(c.volumes))
	for _, v := range c.volumes {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label.Name < out[j].Label.Name })
	return out
}

// Volume returns a labeled volume's record by name.
func (c *Catalog) Volume(name string) (VolumeRecord, bool) {
	if v, ok := c.volumes[name]; ok {
		return *v, true
	}
	return VolumeRecord{}, false
}

func (c *Catalog) entryByID(id string) *Entry {
	for _, e := range c.entries {
		if e.Slot.ID == id {
			return e
		}
	}
	return nil
}

func (c *Catalog) removeEntry(id string) {
	kept := c.entries[:0:0]
	for _, e := range c.entries {
		if e.Slot.ID != id {
			kept = append(kept, e)
		}
	}
	c.entries = kept
}

func (c *Catalog) sortEntries() {
	sort.Slice(c.entries, func(i, j int) bool { return record.SlotIDLess(c.entries[i].Slot.ID, c.entries[j].Slot.ID) })
}

func (c *Catalog) persist() error {
	if err := os.MkdirAll(c.workdir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cacheFile{Entries: c.entries, Volumes: c.volumes, DLEs: prunedDLEMeta(c.dles)}, "", "  ")
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
