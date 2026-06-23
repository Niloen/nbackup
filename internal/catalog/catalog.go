// Package catalog is NBackup's local cache and bookkeeping layer, analogous to
// Amanda's curinfo/tapelist/catalog databases. Because a media volume may be slow
// or offline (tape, Glacier), the catalog keeps a local index so planning,
// listing, restore-location, pruning, and budget reporting never touch the media.
//
// Its model separates what a slot *is* from where its copies *are*: an Entry pairs
// one medium-independent slot (its content, from the seal record) with the set of
// Placements that hold a copy — each a volume plus the file position of every
// archive on it. The media remain the source of truth (every file self-describing,
// every slot sealed, every labeled volume identified), so the whole cache rebuilds
// by scanning: seals -> slots, labels -> the volume registry.
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
	// CacheFile is the catalog cache stored in the workdir.
	CacheFile = "catalog.json"
	// DirSnapshots holds per-DLE, per-level GNU tar snapshot files.
	DirSnapshots = "snapshots"
)

// Entry is the catalog's per-slot record: one logical slot plus every place a
// copy of it lives.
type Entry struct {
	Slot       *slot.Slot  `json:"slot"`       // medium-independent content (from the seal)
	Placements []Placement `json:"placements"` // one per volume holding a copy
}

// Placement is one copy of a slot on one volume.
type Placement struct {
	Medium   string       `json:"medium"`          // config medium name — how to open it
	Volume   string       `json:"volume"`          // volume label name (== Medium for unlabeled media)
	Epoch    int          `json:"epoch,omitempty"` // label epoch when recorded; staleness check on read
	Archives []ArchivePos `json:"archives"`        // file position of each archive on this volume
}

// Pos returns the file position of an archive on this placement's volume.
func (p Placement) Pos(dle string, level int) (int, bool) {
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return a.Pos, true
		}
	}
	return 0, false
}

// ArchivePos is one archive's identity and its file position on a volume.
type ArchivePos struct {
	DLE   string `json:"dle"`
	Level int    `json:"level"`
	Pos   int    `json:"pos"`
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

// EnsureFresh populates an empty cache by scanning one medium's volume the first
// time it is needed (a lost cache, or a catalog created before caching). Copies on
// other media are picked up as operations record them, or via a full Rebuild.
func (c *Catalog) EnsureFresh(medium string, vol media.Volume) error {
	if c.loaded {
		return nil
	}
	if err := c.ingest(medium, vol); err != nil {
		return err
	}
	c.sortEntries()
	c.loaded = true
	if len(c.entries) > 0 || len(c.volumes) > 0 {
		return c.persist()
	}
	return nil
}

// Rebuild rescans the given media (keyed by medium name) and replaces the cache.
// A slot seen on several volumes yields several placements on one logical entry.
// Returns the number of distinct slots indexed.
func (c *Catalog) Rebuild(volumes map[string]media.Volume) (int, error) {
	c.entries = nil
	c.volumes = map[string]*VolumeRecord{}
	for medium, vol := range volumes {
		if err := c.ingest(medium, vol); err != nil {
			return 0, err
		}
	}
	c.sortEntries()
	c.loaded = true
	if err := c.persist(); err != nil {
		return 0, err
	}
	return len(c.entries), nil
}

// ingest merges a medium's slots and placements into the cache. A robotic Library
// (a tape library) scans every non-blank bay in turn, restoring whatever was
// mounted. A single-drive Station can only read the reel currently in the drive —
// the rest sit offline and cannot be mounted unattended — so it is scanned directly
// as just the loaded reel, or skipped when the drive is empty. A plain volume is
// scanned directly.
func (c *Catalog) ingest(medium string, vol media.Volume) error {
	if st, station := vol.(media.Station); station {
		if _, loaded := st.LoadedVolume(); !loaded {
			return nil
		}
		return c.ingestOne(medium, vol)
	}
	ch, ok := vol.(media.Library)
	if !ok {
		return c.ingestOne(medium, vol)
	}
	prev, hadPrev := ch.Loaded()
	bays, err := ch.Bays()
	if err != nil {
		return err
	}
	for _, b := range bays {
		if b.Blank {
			continue
		}
		if err := ch.Mount(b.ID); err != nil {
			return err
		}
		if err := c.ingestOne(medium, vol); err != nil {
			return err
		}
	}
	if hadPrev {
		return ch.Mount(prev)
	}
	return nil
}

// ingestOne scans one mounted volume and merges its slots and placements.
func (c *Catalog) ingestOne(medium string, vol media.Volume) error {
	slots, placements, label, err := scanVolume(medium, vol)
	if err != nil {
		return err
	}
	for _, s := range slots {
		e := c.entryByID(s.ID)
		if e == nil {
			e = &Entry{Slot: s}
			c.entries = append(c.entries, e)
		} else {
			e.Slot = s
		}
		if p, ok := placements[s.ID]; ok {
			e.setPlacement(p)
		}
	}
	if label != nil {
		c.volumes[label.Name] = &VolumeRecord{Label: *label}
	}
	return nil
}

// ScanSlots reads a volume's sealed slots without touching the cache — used to
// check a volume's current contents (e.g. whether a tape is still active before
// relabel).
func ScanSlots(vol media.Volume) ([]*slot.Slot, error) {
	slots, _, _, err := scanVolume("", vol)
	return slots, err
}

// scanVolume groups a volume's files by slot, reads each sealed slot's seal record
// and the volume's label, and builds a Placement (with archive positions) per slot.
func scanVolume(medium string, vol media.Volume) (slots []*slot.Slot, placements map[string]Placement, label *media.Label, err error) {
	files, err := vol.Files()
	if err != nil {
		return nil, nil, nil, err
	}

	volName, epoch := medium, 0
	if lv, ok := vol.(media.Labeled); ok {
		if lbl, labeled, lerr := lv.ReadLabel(); lerr == nil && labeled {
			label = &lbl
			volName, epoch = lbl.Name, lbl.Epoch
		}
	}

	bySlot := map[string][]media.FileInfo{}
	for _, f := range files {
		bySlot[f.Header.Slot] = append(bySlot[f.Header.Slot], f)
	}
	placements = map[string]Placement{}
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
		s, serr := readSeal(vol, sealPos)
		if serr != nil {
			continue // unreadable seal: skip
		}
		slots = append(slots, s)
		p := Placement{Medium: medium, Volume: volName, Epoch: epoch}
		for _, f := range fs {
			if f.Header.Kind == media.KindArchive {
				p.Archives = append(p.Archives, ArchivePos{DLE: f.Header.DLE, Level: f.Header.Level, Pos: f.Pos})
			}
		}
		placements[slotID] = p
	}
	return slots, placements, label, nil
}

// Record stores a slot's content and adds-or-replaces its placement on
// p.Medium, then persists. Both dump and copy use this — they differ only in
// which medium the placement names.
func (c *Catalog) Record(s *slot.Slot, p Placement) error {
	e := c.entryByID(s.ID)
	if e == nil {
		e = &Entry{Slot: s}
		c.entries = append(c.entries, e)
		c.sortEntries()
	} else {
		e.Slot = s
	}
	e.setPlacement(p)
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
	return nil, fmt.Errorf("slot %s not in catalog (run `nb catalog rebuild` if it exists on media)", id)
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
			if p.Volume == volume {
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

// VolumeForMedium returns any volume tracked for a medium (matched by pool). It
// predates the changer and is only meaningful for media with a single volume;
// changer media hold many, so callers use Volume(name) / SlotsOnVolume instead.
func (c *Catalog) VolumeForMedium(medium string) (media.Label, bool) {
	for _, v := range c.volumes {
		if v.Label.Pool == medium {
			return v.Label, true
		}
	}
	return media.Label{}, false
}

// History derives per-DLE run history from the cached slots (source of truth).
func (c *Catalog) History() *History {
	h := &History{DLEs: map[string]*DLEState{}}
	for _, e := range c.entries { // already in run order
		s := e.Slot
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
