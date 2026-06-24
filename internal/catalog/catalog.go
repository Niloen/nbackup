// Package catalog is NBackup's local cache and bookkeeping layer, analogous to
// Amanda's curinfo/tapelist/catalog databases. Because a media volume may be slow
// or offline (tape, Glacier), the catalog keeps a local index so planning,
// listing, restore-location, pruning, and capacity reporting never touch the media.
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
// Everything the catalog holds is derivable from the media; it owns no precious
// state. An archiver's incremental state (gnutar's .snar library) is the one piece of
// non-derivable local state, and it belongs to the archiver, not here (see package
// archiver).
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/slot"
)

// CacheFile is the catalog cache stored in the workdir.
const CacheFile = "catalog.json"

// Entry is the catalog's per-slot record: one logical slot plus every place a
// copy of it lives.
type Entry struct {
	Slot       *slot.Slot  `json:"slot"`       // medium-independent content (from the seal)
	Placements []Placement `json:"placements"` // one per medium holding a copy
}

// Placement is one copy of a slot on one medium. The copy's archives may span
// several of the medium's volumes (tape spanning): each archive names the volumes
// and positions its parts landed on. The seal record (the commit marker, written
// last) lives at Seal — on the final volume the copy occupies.
type Placement struct {
	Medium   string       `json:"medium"`   // config medium name — how to open it
	Archives []ArchivePos `json:"archives"` // each archive and the positions of its parts
	Seal     FilePos      `json:"seal"`     // where the seal record lives
}

// FilePos is the location of one file on a volume: a volume (label name, == Medium
// for unlabeled media) plus a file position on it. It locates both an archive part
// and a placement's seal record.
type FilePos struct {
	Volume string `json:"volume"`
	Epoch  int    `json:"epoch,omitempty"` // label epoch when recorded; staleness check on read
	Pos    int    `json:"pos"`
}

// ArchivePos is one archive's identity and the ordered locations of its parts. An
// archive that fits one volume has a single part; a spanned archive has its
// compressed payload split into several parts across volumes, in order.
type ArchivePos struct {
	DLE   string    `json:"dle"`
	Level int       `json:"level"`
	Parts []FilePos `json:"parts"`
}

// Parts returns the ordered part locations of an archive on this placement.
func (p Placement) Parts(dle string, level int) ([]FilePos, bool) {
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return a.Parts, len(a.Parts) > 0
		}
	}
	return nil, false
}

// Volumes returns the distinct volumes this placement occupies — every archive
// part's volume plus the seal's — in first-seen order. It is what tells which tapes
// a copy needs mounted.
func (p Placement) Volumes() []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, a := range p.Archives {
		for _, pt := range a.Parts {
			add(pt.Volume)
		}
	}
	add(p.Seal.Volume)
	return out
}

// OnVolume reports whether any part of this placement (or its seal) lives on the
// named volume.
func (p Placement) OnVolume(volume string) bool {
	for _, v := range p.Volumes() {
		if v == volume {
			return true
		}
	}
	return false
}

// VolumeRecord is the catalog's cached identity of a labeled volume (Amanda's
// tapelist entry, medium-neutrally named). "Which slots are on it" and "is it
// reusable" are derived from placements + retention, not stored here.
type VolumeRecord struct {
	Label media.Label `json:"label"`
}

// Catalog is a local cache of slot entries plus a registry of labeled volumes. It
// holds no long-lived volume reference; volumes are passed in only to (re)scan.
type Catalog struct {
	workdir string
	entries []*Entry
	volumes map[string]*VolumeRecord // by volume label name
	loaded  bool
}

type cacheFile struct {
	Entries []*Entry                 `json:"entries"`
	Volumes map[string]*VolumeRecord `json:"volumes,omitempty"`
}

// Open loads the catalog cache from the workdir. If the cache file is absent, the
// catalog is empty and not yet loaded (EnsureFresh will populate it).
func Open(workdir string) (*Catalog, error) {
	c := &Catalog{workdir: workdir, volumes: map[string]*VolumeRecord{}}
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
	c.sortEntries()
	c.loaded = true
	return c, nil
}

// Record stores a slot's content and adds-or-replaces its placement on
// p.Medium, then persists. Both dump and copy use this — they differ only in
// which medium the placement names.
func (c *Catalog) Record(s *slot.Slot, p Placement) error {
	c.upsert(s, p)
	c.sortEntries()
	c.loaded = true
	return c.persist()
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

// RecordVolume upserts a labeled volume's identity in the registry, so a later run
// can detect a swapped or relabeled volume.
func (c *Catalog) RecordVolume(lbl media.Label) error {
	c.volumes[lbl.Name] = &VolumeRecord{Label: lbl}
	c.loaded = true
	return c.persist()
}

// Slots returns the cached slots in run order.
func (c *Catalog) Slots() []*slot.Slot {
	out := make([]*slot.Slot, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.Slot)
	}
	return out
}

// ReadSlot returns a cached slot by ID.
func (c *Catalog) ReadSlot(id string) (*slot.Slot, error) {
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
func (c *Catalog) SlotsOn(medium string) []*slot.Slot {
	var out []*slot.Slot
	for _, e := range c.entries {
		if e.placedOn(medium) {
			out = append(out, e.Slot)
		}
	}
	return out
}

// SlotsOnVolume returns the slots with a copy on a specific volume (label), in
// run order — used to tell whether a tape already holds a run.
func (c *Catalog) SlotsOnVolume(volume string) []*slot.Slot {
	var out []*slot.Slot
	for _, e := range c.entries {
		for _, p := range e.Placements {
			if p.OnVolume(volume) {
				out = append(out, e.Slot)
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
			total += e.Slot.TotalBytes
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

// History derives per-DLE run history from the cached slots (source of truth).
func (c *Catalog) History() *History {
	h := &History{DLEs: map[string]*DLEState{}}
	for _, e := range c.entries { // already in run order
		s := e.Slot
		for _, a := range s.Archives {
			h.RecordRun(a.DLE, s.ID, s.Date, a.Level)
		}
	}
	return h
}

// upsert sets a slot's content and adds-or-replaces its placement on p.Medium,
// without sorting or persisting. It is the in-memory write shared by Record and by
// the importer's absorb — the single point where a slot+placement enters the store.
func (c *Catalog) upsert(s *slot.Slot, p Placement) {
	e := c.entryByID(s.ID)
	if e == nil {
		e = &Entry{Slot: s}
		c.entries = append(c.entries, e)
	} else {
		e.Slot = s
	}
	e.setPlacement(p)
}

func (e *Entry) placedOn(medium string) bool {
	for _, p := range e.Placements {
		if p.Medium == medium {
			return true
		}
	}
	return false
}

// setPlacement replaces the placement on the same medium, or appends a new one.
func (e *Entry) setPlacement(p Placement) {
	for i, ep := range e.Placements {
		if ep.Medium == p.Medium {
			e.Placements[i] = p
			return
		}
	}
	e.Placements = append(e.Placements, p)
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
	sort.Slice(c.entries, func(i, j int) bool { return slot.Less(c.entries[i].Slot, c.entries[j].Slot) })
}

func (c *Catalog) persist() error {
	if err := os.MkdirAll(c.workdir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cacheFile{Entries: c.entries, Volumes: c.volumes}, "", "  ")
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
