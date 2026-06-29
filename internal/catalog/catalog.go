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

// VolumeRecord is the catalog's cached identity of a labeled volume. "Which
// slots are on it" and "is it reusable" are derived from placements + retention,
// not stored here.
type VolumeRecord struct {
	Label record.Label `json:"label"`
}

// DLEMeta is the catalog's per-DLE operator/planner metadata, keyed by DLE slug. Unlike an
// Entry (which is media-derived and rebuilt by scanning), this is intent that cannot be
// scanned back — so it lives in the cache file and is deliberately preserved across a
// Rebuild. It is a struct, not a bare flag, so further per-DLE state can accrete here
// without reshaping the cache. Today it carries only ForceFull: the operator asked (via
// `nb reset`) that this DLE be fulled on its next run; a run consumes it.
type DLEMeta struct {
	ForceFull bool `json:"force_full,omitempty"`
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
func (c *Catalog) AddArchive(slot *record.Slot, medium string, arch record.Archive, pos ArchivePos) error {
	c.addArchive(slot, medium, arch, pos)
	c.sortEntries()
	return c.persist()
}

// addArchive is the in-memory merge AddArchive wraps: it creates the slot entry from `slot`'s
// identity on first sight and merges the archive's content + placement position, but neither
// sorts nor persists — for a bulk caller (a rebuild scan) that persists once at the end. The
// catalog cache holds no member lists (they live in the member-index cache + the on-medium
// index), so members are cleared here.
func (c *Catalog) addArchive(slot *record.Slot, medium string, arch record.Archive, pos ArchivePos) {
	e := c.entryByID(slot.ID)
	if e == nil {
		ident := *slot
		ident.Archives, ident.TotalBytes = nil, 0
		e = &Entry{Slot: &ident}
		c.entries = append(c.entries, e)
	}
	arch.Members = nil
	mergeSlotArchive(e.Slot, arch)
	e.addPlacementPos(medium, pos)
	c.loaded = true
}

// mergeSlotArchive adds a's content to the slot, replacing any prior archive of the same
// (DLE, level) and keeping TotalBytes in step. The slot content is the union of every archive
// the run produces, independent of which medium currently holds each copy.
func mergeSlotArchive(s *record.Slot, a record.Archive) {
	for i := range s.Archives {
		if s.Archives[i].DLE == a.DLE && s.Archives[i].Level == a.Level {
			s.Archives[i] = a
			s.TotalBytes = 0
			for _, x := range s.Archives {
				s.TotalBytes += x.Compressed
			}
			return
		}
	}
	s.Archives = append(s.Archives, a)
	s.TotalBytes += a.Compressed
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
		e.Slot.DropArchive(dle)
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
	c.volumes[lbl.Name] = &VolumeRecord{Label: lbl}
	c.loaded = true
	return c.persist()
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

// SetForceFull marks a DLE (by slug) to be fulled on its next run and persists. It is the
// store behind `nb reset`: the planner reads ForcedFulls and schedules a mandatory L0.
func (c *Catalog) SetForceFull(slug string) error {
	c.metaFor(slug).ForceFull = true
	return c.persist()
}

// ForcedFulls returns the set of DLE slugs currently flagged for a forced full.
func (c *Catalog) ForcedFulls() map[string]bool {
	out := map[string]bool{}
	for slug, m := range c.dles {
		if m.ForceFull {
			out[slug] = true
		}
	}
	return out
}

// ClearForceFulls drops the force-full flag for the given DLE slugs and persists — called
// once a run seals, having dumped every planned (hence every forced) DLE at L0.
func (c *Catalog) ClearForceFulls(slugs map[string]bool) error {
	changed := false
	for slug := range slugs {
		if m := c.dles[slug]; m != nil && m.ForceFull {
			m.ForceFull = false
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return c.persist()
}

// metaFor returns the DLE's metadata record, creating it on first use.
func (c *Catalog) metaFor(slug string) *DLEMeta {
	if c.dles == nil {
		c.dles = map[string]*DLEMeta{}
	}
	m := c.dles[slug]
	if m == nil {
		m = &DLEMeta{}
		c.dles[slug] = m
	}
	return m
}

// prunedDLEMeta drops zero-value records so the cache file carries only DLEs with live
// metadata (a consumed force-full leaves no residue).
func prunedDLEMeta(dles map[string]*DLEMeta) map[string]*DLEMeta {
	var out map[string]*DLEMeta
	for slug, m := range dles {
		if m == nil || *m == (DLEMeta{}) {
			continue
		}
		if out == nil {
			out = map[string]*DLEMeta{}
		}
		out[slug] = m
	}
	return out
}

func (e *Entry) placedOn(medium string) bool {
	for _, p := range e.Placements {
		if p.Medium == medium {
			return true
		}
	}
	return false
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
