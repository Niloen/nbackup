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

	"github.com/Niloen/nbackup/internal/record"
)

// CacheFile is the catalog cache stored in the workdir.
const CacheFile = "catalog.json"

// Entry is the catalog's per-slot record: one logical slot plus every place a
// copy of it lives.
type Entry struct {
	Slot       *record.Slot `json:"slot"`       // medium-independent content (from the seal)
	Placements []Placement  `json:"placements"` // one per medium holding a copy
}

// Placement is one copy of a slot on one medium. The copy's archives may span
// several of the medium's volumes (tape spanning): each archive names the volumes
// and positions its parts landed on, plus where its per-archive commit footer and
// member index live (the commit footer is the marker, written last).
type Placement struct {
	// Medium is the pool this copy is accounted to (retention, capacity, cost) and
	// the changer to open to reach it. It is NOT a device pin: a labeled volume is
	// located by its label at mount time, so which drive holds a tape is resolved at
	// runtime, not stored here.
	Medium   string       `json:"medium"`
	Archives []ArchivePos `json:"archives"` // each archive and the positions of its parts, commit, and index
}

// FilePos and ArchivePos are the file-location types the catalog persists. They are
// the very types the archiveio writer emits and the reader consumes, defined once in
// package record (the shared on-medium artifact vocabulary) so a writer's recorded
// positions become a placement with no field-by-field conversion. FilePos.Label is the
// volume's global, device-independent identity ("" for address-identified media, which
// carry no label); ArchivePos lists an archive's ordered parts (one unless it spanned).
type (
	FilePos    = record.FilePos
	ArchivePos = record.ArchivePos
)

// Parts returns the ordered part locations of an archive on this placement.
func (p Placement) Parts(dle string, level int) ([]FilePos, bool) {
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return a.Parts, len(a.Parts) > 0
		}
	}
	return nil, false
}

// Labels returns the distinct volume labels this placement occupies — every
// archive part's label plus the seal's — in first-seen order. It is what tells
// which tapes a copy needs mounted. It is empty for address-identified media,
// which carry no labels: a disk/s3 copy spans no tapes, and is reached by its
// medium alone (Placement.Medium), needing no label-based mount.
func (p Placement) Labels() []string {
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
			add(pt.Label)
		}
		add(a.Commit.Label)
		add(a.Index.Label)
	}
	return out
}

// OnLabel reports whether any part of this placement (or its seal) lives on the
// volume with the given label.
func (p Placement) OnLabel(label string) bool {
	for _, v := range p.Labels() {
		if v == label {
			return true
		}
	}
	return false
}

// VolumeRecord is the catalog's cached identity of a labeled volume (Amanda's
// tapelist entry, medium-neutrally named). "Which slots are on it" and "is it
// reusable" are derived from placements + retention, not stored here.
type VolumeRecord struct {
	Label record.Label `json:"label"`
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
func (c *Catalog) Record(s *record.Slot, p Placement) error {
	c.upsert(stripMembers(s), p)
	c.sortEntries()
	c.loaded = true
	return c.persist()
}

// stripMembers returns a shallow copy of s with each archive's member list cleared. The
// catalog cache is the slot index, not the member store: member lists live in the workdir
// member-index cache and the on-medium index, loaded on demand. Keeping them out of the
// cache keeps it small (read on every command) regardless of how many files were backed up.
func stripMembers(s *record.Slot) *record.Slot {
	cp := *s
	cp.Archives = make([]record.Archive, len(s.Archives))
	for i, a := range s.Archives {
		a.Members = nil
		cp.Archives[i] = a
	}
	return &cp
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
func (c *Catalog) RecordVolume(lbl record.Label) error {
	c.volumes[lbl.Name] = &VolumeRecord{Label: lbl}
	c.loaded = true
	return c.persist()
}

// Slots returns the cached slots in run order.
func (c *Catalog) Slots() []*record.Slot {
	out := make([]*record.Slot, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.Slot)
	}
	return out
}

// ReadSlot returns a cached slot by ID.
func (c *Catalog) ReadSlot(id string) (*record.Slot, error) {
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
func (c *Catalog) SlotsOn(medium string) []*record.Slot {
	var out []*record.Slot
	for _, e := range c.entries {
		if e.placedOn(medium) {
			out = append(out, e.Slot)
		}
	}
	return out
}

// SlotsOnLabel returns the slots with a copy on the volume with the given label,
// in run order — used to tell whether a tape already holds a run.
func (c *Catalog) SlotsOnLabel(label string) []*record.Slot {
	var out []*record.Slot
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
func (c *Catalog) upsert(s *record.Slot, p Placement) {
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
	sort.Slice(c.entries, func(i, j int) bool { return record.Less(c.entries[i].Slot, c.entries[j].Slot) })
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
