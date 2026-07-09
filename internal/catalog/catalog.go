// Package catalog is NBackup's local cache and bookkeeping layer. Because a
// media volume may be slow or offline (tape, Glacier), the catalog keeps a local
// index so planning, listing, restore-location, pruning, and capacity reporting
// never touch the media.
//
// Its model separates what a run *is* from where its copies *are*: an Entry pairs
// one medium-independent run (its content, grouped from the archives' commit footers)
// with the set of Placements that hold a copy — each a volume plus the file position
// of every archive on it. The media remain the source of truth (every file
// self-describing, every archive marked complete by its commit footer, every labeled
// volume identified), so the whole cache rebuilds by scanning: commit footers ->
// runs, labels -> the volume registry.
//
// The package has two faces. This file is the store: an in-memory index of Entries
// and VolumeRecords with queries, insert/update/delete, and JSON persistence — the
// "database" the rest of the system reads and writes. scan.go is the importer that
// rebuilds that store from the media (the source of truth); it hands finished
// placements back through the store's write path and never touches its fields.
//
// Almost everything the catalog holds is derivable from the media; the cache is a
// performance copy, not a system of record. Two deliberate exceptions live beside it.
// Per-DLE operator intent (DLEMeta — today just the `nb reset` force-full directive)
// cannot be scanned back, so it lives in the cache file beside the entries and is
// preserved across a Rebuild. And the usage ledger (usage.go, its own append-only file)
// records each medium's stored bytes over time: a prune's or relabel's *decline* leaves
// no trace on the volume, so it must be recorded when it happens, never derived — the
// bookkeeping half of the package's charter, Amanda's curinfo keeping history in the
// catalog layer. An archiver's incremental state (gnutar's .snar library) is
// non-derivable too, but it is precious and belongs to the archiver, not here (see
// package archiver).
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/fsx"
	"github.com/Niloen/nbackup/internal/record"
)

// CacheFile is the catalog cache stored in the workdir.
const CacheFile = "catalog.json"

// VolumeRecord is the catalog's cached identity of a labeled volume. "Which
// runs are on it" and "is it reusable" are derived from placements + retention,
// not stored here. Barcode is the cartridge the label was last seen on — learned
// whenever a load or label actually reads the tape (Amanda's chg-robot keeps the
// same barcode map in its statefile), so inventory can show which slot holds a
// known volume without loading it. It is a cached observation, stale if cartridges
// are swapped outside NBackup, and "" until the volume has been seen in a drive.
type VolumeRecord struct {
	Label   record.Label `json:"label"`
	Barcode string       `json:"barcode,omitempty"`
	// Used is the volume's stored fill: the on-medium cost of everything recorded
	// on the reel, priced by its medium's own cost rule (media.FileCoster's bounded
	// contract) AT MUTATION TIME — each placement add/remove and the (re)label
	// reset adjust it inside the catalog's own write path (applyFill), and a
	// rebuild scan reconstructs it through that same path, so no query ever needs a
	// cost function. It is what a declared volume_size is spent against, a tape
	// being unable to report its own fill; orphans (a crashed run, a recycled
	// spanning run's leftovers on surviving reels) are recorded nowhere and so not
	// counted — one reason volume_size is declared below native capacity.
	Used int64 `json:"used,omitempty"`
}

// Catalog is a local cache of run entries plus a registry of labeled volumes. It
// holds no long-lived volume reference; volumes are passed in only to (re)scan.
type Catalog struct {
	workdir string
	entries []*Entry
	volumes map[string]*VolumeRecord // by volume label name
	dles    map[string]*DLEMeta      // per-DLE operator/planner metadata, by slug
	loaded  bool
	win     *Window // the open run window, if any — guards one-window-at-a-time; mutators persist per op as always

	resolved  *ResolvedSet     // the latest run's resolved DLE set (resolved.go); nil until a run records one
	now       func() time.Time // injectable clock for the usage ledger's sample stamps; Open sets time.Now
	costFor   CostResolver     // medium name -> file-cost rule, injected by PriceWith; nil = fill not tracked
	usage     []UsageSample    // the usage ledger (usage.go), loaded at Open, appended by persist
	usageLast map[string]int64 // each medium's last recorded stored-bytes, the persist-time diff base
}

type cacheFile struct {
	Entries  []*Entry                 `json:"entries"`
	Volumes  map[string]*VolumeRecord `json:"volumes,omitempty"`
	DLEs     map[string]*DLEMeta      `json:"dles,omitempty"`
	Resolved *ResolvedSet             `json:"resolved,omitempty"` // the latest run's resolved DLE set (resolved.go)
}

// Open loads the catalog cache from the workdir. If the cache file is absent, the
// catalog is empty and not yet loaded (EnsureFresh will populate it).
func Open(workdir string) (*Catalog, error) {
	c := &Catalog{workdir: workdir, volumes: map[string]*VolumeRecord{}, dles: map[string]*DLEMeta{}, now: time.Now}
	c.loadUsage()
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
	c.resolved = cf.Resolved
	c.sortEntries()
	c.loaded = true
	return c, nil
}

// CostResolver resolves a medium name to its file-cost rule (media.Spec.FileCost),
// ok=false for media without finite labeled volumes. The engine injects it via
// PriceWith; the catalog applies it only inside its own mutators (see
// VolumeRecord.Used), never at query time.
type CostResolver func(medium string) (func(kind string, payload int64) int64, bool)

// PriceWith injects the medium cost resolver the stored volume fill is maintained
// with — wired once by the engine (the owner of config + the media registry), the
// same posture as the injected clock. Without it (a bare Open in a test) fill
// tracking is off and Used stays 0.
func (c *Catalog) PriceWith(costFor CostResolver) { c.costFor = costFor }

// chargeFill accumulates the per-label charges of one placed archive being
// recorded (sign +1) or dropped (sign -1) on medium into delta — parts at their
// exact seal sizes, the member index at the footer's IndexSize, the commit footer
// at the medium's bound; a sealless record (an old footer, a partial scan)
// charges its whole Compressed size once, conservatively. Accumulate-then-settle
// (settleFill) keeps a replace (drop old + record new, usually identical) a NET
// zero per label instead of two clamped operations. A no-op without a resolver or
// for an unpriceable medium.
func (c *Catalog) chargeFill(medium string, run *Run, pa PlacedArchive, sign int64, delta map[string]int64) {
	if c.costFor == nil {
		return
	}
	cost, ok := c.costFor(medium)
	if !ok {
		return
	}
	arch, _ := run.Archive(pa.DLE, pa.Level)
	add := func(label, kind string, payload int64) {
		if label != "" {
			delta[label] += sign * cost(kind, payload)
		}
	}
	if len(pa.Seals) == len(pa.Parts) && len(pa.Parts) > 0 {
		for i, pt := range pa.Parts {
			add(pt.Label, record.KindArchive, pa.Seals[i].Size)
		}
	} else {
		for _, pt := range pa.Parts {
			if pt.Label != "" {
				add(pt.Label, record.KindArchive, arch.Compressed)
				break
			}
		}
	}
	add(pa.Commit.Label, record.KindCommit, 0)
	add(pa.Index.Label, record.KindIndex, arch.IndexSize)
}

// settleFill applies accumulated per-label charges to the registered volumes'
// stored fill, flooring at zero. Unregistered labels (a TOC-referenced tape the
// scan has not seen) carry no fill to move and are skipped — their figure starts
// when their tape is scanned.
func (c *Catalog) settleFill(delta map[string]int64) {
	for label, n := range delta {
		if v, ok := c.volumes[label]; ok {
			if v.Used += n; v.Used < 0 {
				v.Used = 0
			}
		}
	}
}

// applyFill is the single-record convenience over chargeFill+settleFill.
func (c *Catalog) applyFill(medium string, run *Run, pa PlacedArchive, sign int64) {
	delta := map[string]int64{}
	c.chargeFill(medium, run, pa, sign, delta)
	c.settleFill(delta)
}

// AddArchive merges one archive's content and its placement position into the catalog and
// persists — the catalog's single write path. A run is only a grouping of committed
// archives, so there is no "add a run": the entry is created from the archive's run
// identity the first time one of its archives lands, and archives accrete into it one at a
// time. Both a dump (its Finish records each committed archive), a copy/sync, a rebuild scan,
// and the holding-disk taper write through here; reclaim is the symmetric RemoveArchive.
//
// Every catalog mutation is single-threaded (a run routes all placement writes through one
// goroutine), so no locking is needed.
func (c *Catalog) AddArchive(arch record.Archive, medium string, pos archiveio.ArchivePos) error {
	c.addArchive(arch, medium, pos)
	c.sortEntries()
	return c.persist()
}

// addArchive is the in-memory merge AddArchive wraps: it creates the run entry from the
// archive's own run tag (arch.Run) on first sight and merges the archive's content +
// placement position, but neither sorts nor persists — for a bulk caller (a rebuild scan)
// that persists once at the end. The catalog cache holds no member lists (they live in the
// member-index cache + the on-medium index), so members are cleared here.
func (c *Catalog) addArchive(arch record.Archive, medium string, pos archiveio.ArchivePos) {
	e := c.entryByID(arch.Run)
	if e == nil {
		e = &Entry{Run: &Run{ID: arch.Run}}
		c.entries = append(c.entries, e)
	}
	arch.Members = nil
	arch.Units = nil
	arch.Frames = nil
	// The per-part seals describe this placement's layout, so they land on the placed
	// record (index-aligned with the part positions) and are stripped from the run's
	// medium-independent content — the same split as the positions themselves. A scan
	// that found only some parts (a tape absent) breaks the index alignment, so seals
	// attach only when every part position is present.
	seals := arch.PartSeals
	if len(seals) != len(pos.Parts) {
		seals = nil
	}
	arch.PartSeals = nil
	arch.PartMap = nil               // placement layout too: PlacedArchive.Parts carries the same locations
	arch.IndexPos = record.FilePos{} // ditto: PlacedArchive.Index
	e.Run.addArchive(arch)
	// The archive's key comes from the archive record itself — pos is pure position.
	pa := PlacedArchive{DLE: arch.DLE, Level: arch.Level, Parts: pos.Parts, Seals: seals, Commit: pos.Commit, Index: pos.Index}
	// The stored fill moves with the placement, inside this same mutation: a
	// replaced record (a re-copy or re-scan of the same archive) gives its charge
	// back in the same settlement, so an unchanged record nets zero per label.
	delta := map[string]int64{}
	if p, ok := e.placementOn(medium); ok {
		if old, ok := p.Placed(arch.DLE, arch.Level); ok {
			c.chargeFill(medium, e.Run, old, -1, delta)
		}
	}
	e.addPlaced(medium, pa)
	c.chargeFill(medium, e.Run, pa, +1, delta)
	c.settleFill(delta)
	c.loaded = true
}

// addPlaced records a placed archive on the entry's copy on medium, creating the
// placement if absent and replacing any prior record of the same (DLE, level).
func (e *Entry) addPlaced(medium string, pa PlacedArchive) {
	for i := range e.Placements {
		if e.Placements[i].Medium == medium {
			e.Placements[i].Archives = mergePlaced(e.Placements[i].Archives, pa)
			return
		}
	}
	e.Placements = append(e.Placements, Placement{Medium: medium, Archives: []PlacedArchive{pa}})
}

// mergePlaced returns list with pa added, replacing any entry of the same (DLE, level).
func mergePlaced(list []PlacedArchive, pa PlacedArchive) []PlacedArchive {
	for i := range list {
		if list[i].DLE == pa.DLE && list[i].Level == pa.Level {
			list[i] = pa
			return list
		}
	}
	return append(list, pa)
}

// RemovePlacement drops the copy of a run on one medium. When the last copy is
// gone the whole entry is removed (gone=true) — the run no longer exists anywhere.
func (c *Catalog) RemovePlacement(runID, medium string) (gone bool, err error) {
	e := c.entryByID(runID)
	if e == nil {
		return false, nil
	}
	kept := e.Placements[:0:0]
	for _, p := range e.Placements {
		if p.Medium != medium {
			kept = append(kept, p)
			continue
		}
		for _, pa := range p.Archives {
			c.applyFill(medium, e.Run, pa, -1) // the dropped copy's charge leaves its reels
		}
	}
	e.Placements = kept
	if len(e.Placements) == 0 {
		c.removeEntry(runID)
		gone = true
	}
	return gone, c.persist()
}

// RemoveArchive drops one archive (a DLE's image) from the copy of a run on one
// medium — the per-archive peer of RemovePlacement. It removes that DLE's PlacedArchive
// from the medium's placement; when the placement keeps no archives the whole
// placement goes (placementGone), and when that was the run's last copy the entry
// goes too (entryGone) — the run no longer exists anywhere. When no remaining
// placement holds this DLE, the run's medium-independent content
// (Entry.Run.Archives) drops it too: the run stops advertising an image no
// medium holds, even while it keeps other DLEs' images on surviving copies.
func (c *Catalog) RemoveArchive(runID, medium, dle string) (placementGone, entryGone bool, err error) {
	e := c.entryByID(runID)
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
				continue
			}
			c.applyFill(medium, e.Run, a, -1) // the pruned image's charge leaves its reels
		}
		p.Archives = kept
		break
	}
	kept := e.Placements[:0:0]
	for _, p := range e.Placements {
		if len(p.Archives) > 0 {
			kept = append(kept, p)
		} else {
			placementGone = true
		}
	}
	e.Placements = kept
	if !e.anyPlacementHolds(dle) {
		e.Run.dropArchive(dle)
	}
	if len(e.Placements) == 0 {
		c.removeEntry(runID)
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
// persists once at the end of a scan). The stored fill follows the epoch: a
// same-epoch re-record keeps it, while a (re)label — which physically wipes the
// reel — restarts it at the label file's own charge.
func (c *Catalog) upsertVolume(lbl record.Label) {
	rec := &VolumeRecord{Label: lbl, Used: c.labelFill(lbl)}
	if old, ok := c.volumes[lbl.Name]; ok {
		rec.Barcode = old.Barcode // identity update; the learned cartridge stays
		if old.Label.Epoch == lbl.Epoch {
			rec.Used = old.Used
		}
	} else {
		// A label registered for the first time may already be referenced by
		// placements — a reel known only from other tapes' commit-footer part maps
		// until now. Their charges were skipped while the reel had no record
		// (settleFill applies only to registered volumes), so registration
		// settles them retroactively.
		rec.Used += c.retroFill(lbl.Name)
	}
	c.volumes[lbl.Name] = rec
	c.loaded = true
}

// retroFill sums the charges existing placements hold against a label that is
// only now entering the registry (see upsertVolume).
func (c *Catalog) retroFill(label string) int64 {
	delta := map[string]int64{}
	for _, e := range c.entries {
		for _, p := range e.Placements {
			for _, pa := range p.Archives {
				c.chargeFill(p.Medium, e.Run, pa, +1, delta)
			}
		}
	}
	return delta[label]
}

// labelFill is a freshly labeled reel's starting fill: its file-0 label record,
// priced by its pool's medium (a label's Pool IS the owning medium's name).
func (c *Catalog) labelFill(lbl record.Label) int64 {
	if c.costFor == nil {
		return 0
	}
	cost, ok := c.costFor(lbl.Pool)
	if !ok {
		return 0
	}
	return cost(record.KindLabel, 0)
}

// SetVolumeBarcode records which cartridge (barcode) a volume's label was last
// read from — the learned barcode↔label map behind slot-inventory display. A
// cartridge holds one volume, so the barcode is dropped from any other record
// first. A no-op for an unknown volume or an empty barcode (no scanner).
func (c *Catalog) SetVolumeBarcode(name, barcode string) error {
	rec, ok := c.volumes[name]
	if !ok || barcode == "" || rec.Barcode == barcode {
		return nil
	}
	for _, other := range c.volumes {
		if other.Barcode == barcode {
			other.Barcode = ""
		}
	}
	rec.Barcode = barcode
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

// Runs returns the cached runs in run order.
func (c *Catalog) Runs() []*Run {
	out := make([]*Run, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.Run)
	}
	return out
}

// ReadRun returns a cached run by ID.
func (c *Catalog) ReadRun(id string) (*Run, error) {
	if e := c.entryByID(id); e != nil {
		return e.Run, nil
	}
	return nil, fmt.Errorf("run %s not in catalog (run `nb rebuild` if it exists on media)", id)
}

// Placements returns the copies of a run, for a reader to choose among.
func (c *Catalog) Placements(runID string) []Placement {
	if e := c.entryByID(runID); e != nil {
		return e.Placements
	}
	return nil
}

// snapshotPlacements deep-copies every run's placements, keyed by run ID — the View's
// point-in-time data, taken at OpenWindow. The copy goes down to each archive's part
// list, so a writer merging new positions into a live entry never shares an array with it.
func (c *Catalog) snapshotPlacements() map[string][]Placement {
	snap := make(map[string][]Placement, len(c.entries))
	for _, e := range c.entries {
		ps := make([]Placement, len(e.Placements))
		for i, p := range e.Placements {
			archives := make([]PlacedArchive, len(p.Archives))
			for j, a := range p.Archives {
				a.Parts = append([]archiveio.FilePos(nil), a.Parts...)
				archives[j] = a
			}
			ps[i] = Placement{Medium: p.Medium, Archives: archives}
		}
		snap[e.Run.ID] = ps
	}
	return snap
}

// RunsOn returns the runs with a copy on the named medium, in run order.
func (c *Catalog) RunsOn(medium string) []*Run {
	var out []*Run
	for _, e := range c.entries {
		if e.placedOn(medium) {
			out = append(out, e.Run)
		}
	}
	return out
}

// RunsOnLabel returns the runs with a copy on the volume with the given label,
// in run order — used to tell whether a tape already holds a run.
func (c *Catalog) RunsOnLabel(label string) []*Run {
	var out []*Run
	for _, e := range c.entries {
		for _, p := range e.Placements {
			if p.OnLabel(label) {
				out = append(out, e.Run)
				break
			}
		}
	}
	return out
}

// Archives returns every cached archive (each carrying its run tag), across all runs, in
// run order — the corpus the policy layer (restore, recovery, drill) reasons over.
func (c *Catalog) Archives() []record.Archive {
	var out []record.Archive
	for _, e := range c.entries {
		out = append(out, e.Run.Archives...)
	}
	return out
}

// ArchivesOn returns the archives whose copy actually lives on the named medium — the
// per-medium corpus retention, reclamation, and usage accounting reason over. It is
// archive-granular, matching the placement record: a per-archive prune leaves a run's
// copy holding only some of its archives, and the pruned ones no longer count against
// (or get re-pruned from) this medium even while the run keeps them on other media.
func (c *Catalog) ArchivesOn(medium string) []record.Archive {
	var out []record.Archive
	for _, e := range c.entries {
		p, ok := e.placementOn(medium)
		if !ok {
			continue
		}
		for _, a := range e.Run.Archives {
			if p.Holds(a.DLE, a.Level) {
				out = append(out, a)
			}
		}
	}
	return out
}

// PlacedArchiveInfo pairs one archive held on a medium (its record.Archive — bytes,
// level, commit time) with its placement there (parts/labels) — the join
// ArchivesOn already does, extended with per-archive location for a caller that
// must attribute bytes to a specific volume label rather than just the medium as
// a whole (per-volume accounting for a labeled pool).
type PlacedArchiveInfo struct {
	Run     string
	Archive record.Archive
	Placed  PlacedArchive
}

// PlacedArchivesOn returns, for the named medium, every archive currently held
// there together with its placement record — ArchivesOn's archive-granular join,
// carrying each archive's part/label positions alongside it.
func (c *Catalog) PlacedArchivesOn(medium string) []PlacedArchiveInfo {
	var out []PlacedArchiveInfo
	for _, e := range c.entries {
		p, ok := e.placementOn(medium)
		if !ok {
			continue
		}
		for _, a := range e.Run.Archives {
			if pa, ok := p.Placed(a.DLE, a.Level); ok {
				out = append(out, PlacedArchiveInfo{Run: e.Run.ID, Archive: a, Placed: pa})
			}
		}
	}
	return out
}

// RunIDsOnLabel returns the ids of the runs with a copy on the volume with the given label,
// in run order — what a volume's reusability check (retention.Floor.First) consults.
func (c *Catalog) RunIDsOnLabel(label string) []string {
	var out []string
	for _, e := range c.entries {
		for _, p := range e.Placements {
			if p.OnLabel(label) {
				out = append(out, e.Run.ID)
				break
			}
		}
	}
	return out
}

// MediumBytes sums the stored bytes of the archives with a copy on the named medium.
// Archive-granular like ArchivesOn: an archive pruned from this medium stops counting
// here even while its run's other archives (or its own copies elsewhere) remain.
func (c *Catalog) MediumBytes(medium string) int64 {
	var total int64
	for _, a := range c.ArchivesOn(medium) {
		total += a.Compressed
	}
	return total
}

// MissingVolume names a tape the catalog's placements reference but no scan has
// ever seen: a label learned from commit footers' part maps (the TOC) whose reel
// was not among the volumes fed to any rebuild — the worklist entry telling the
// operator which tape to insert next.
type MissingVolume struct {
	Label string
	Runs  []string // runs with files on it, sorted
}

// MissingVolumes reports the labels placements reference that are absent from the
// volume registry, sorted by label. Empty means every referenced tape has been
// scanned (or recorded live) — the catalog is complete for what it knows about.
func (c *Catalog) MissingVolumes() []MissingVolume {
	runsByLabel := map[string]map[string]bool{}
	for _, e := range c.entries {
		for _, p := range e.Placements {
			for _, label := range p.Labels() {
				if _, known := c.volumes[label]; known {
					continue
				}
				if runsByLabel[label] == nil {
					runsByLabel[label] = map[string]bool{}
				}
				runsByLabel[label][e.Run.ID] = true
			}
		}
	}
	out := make([]MissingVolume, 0, len(runsByLabel))
	for label, runs := range runsByLabel {
		ids := make([]string, 0, len(runs))
		for id := range runs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out = append(out, MissingVolume{Label: label, Runs: ids})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
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
		if e.Run.ID == id {
			return e
		}
	}
	return nil
}

func (c *Catalog) removeEntry(id string) {
	kept := c.entries[:0:0]
	for _, e := range c.entries {
		if e.Run.ID != id {
			kept = append(kept, e)
		}
	}
	c.entries = kept
}

func (c *Catalog) sortEntries() {
	sort.Slice(c.entries, func(i, j int) bool { return record.RunIDLess(c.entries[i].Run.ID, c.entries[j].Run.ID) })
}

func (c *Catalog) persist() error {
	if err := os.MkdirAll(c.workdir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cacheFile{Entries: c.entries, Volumes: c.volumes, DLEs: prunedDLEMeta(c.dles), Resolved: c.resolved}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := fsx.WriteFileAtomic(filepath.Join(c.workdir, CacheFile), data, 0o644); err != nil {
		return err
	}
	// The cache is durable; record what changed in the usage ledger (usage.go).
	// persist is the one choke point every mutation flows through, so recording here
	// covers every byte-changing path by construction; recordUsage itself is
	// best-effort and diffs to a no-op for non-byte mutations.
	c.recordUsage()
	return nil
}
