package catalog

import (
	"io"
	"sort"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// scan.go is the catalog's importer: it reads the media (the source of truth) and
// rebuilds the store from it. It speaks the media's vocabulary — bays, mounts,
// archive parts, seals, labels — and ends by handing finished placements back to
// the store through absorb(); it never reaches into the store's fields. The store
// (catalog.go) drives a rebuild via EnsureFresh / Rebuild below.

// EnsureFresh populates an empty cache by scanning one medium's volume the first
// time it is needed (a lost cache, or a catalog created before caching). Copies on
// other media are picked up as operations record them, or via a full Rebuild.
func (c *Catalog) EnsureFresh(medium string, vol media.Volume) error {
	if c.loaded {
		return nil
	}
	idx, err := scanMedium(medium, vol)
	if err != nil {
		return err
	}
	c.absorb(idx)
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
		idx, err := scanMedium(medium, vol)
		if err != nil {
			return 0, err
		}
		c.absorb(idx)
	}
	c.sortEntries()
	c.loaded = true
	if err := c.persist(); err != nil {
		return 0, err
	}
	return len(c.entries), nil
}

// absorb merges one medium's scanned placements and volume labels into the store, without
// persisting (the caller persists once). It is the seam between the importer and the store:
// each scanned archive enters through the in-memory addArchive — the same single write path a
// dump uses — so a slot seen on several media gathers several placements on one entry, and the
// scanned slot's sealed identity creates the entry sealed. Each label upserts the volume registry.
func (c *Catalog) absorb(idx mediumIndex) {
	for _, sp := range idx.placements {
		for _, arch := range sp.slot.Archives {
			if pos, ok := findArchivePos(sp.p.Archives, arch.DLE, arch.Level); ok {
				c.addArchive(arch, sp.p.Medium, pos)
			}
		}
	}
	for _, lbl := range idx.labels {
		c.upsertVolume(lbl)
	}
}

// findArchivePos returns the position of (dle, level) among a placement's archives.
func findArchivePos(aps []ArchivePos, dle string, level int) (ArchivePos, bool) {
	for _, ap := range aps {
		if ap.DLE == dle && ap.Level == level {
			return ap, true
		}
	}
	return ArchivePos{}, false
}

// mediumIndex is the assembled result of scanning one medium: each sealed slot
// with its placement on that medium, plus the labels of the volumes seen.
type mediumIndex struct {
	placements []slotPlacement
	labels     []record.Label
}

// slotPlacement pairs a slot's content with its placement on the scanned
// medium, ready for the store to absorb.
type slotPlacement struct {
	slot *Slot
	p    Placement
}

// scanMedium reads every readable volume of one medium and assembles its sealed
// slots into placements. The shape walk — every non-blank bay of a robotic library,
// or just the loaded reel of a single-drive station — lives in media.WalkReadable,
// so the catalog never type-asserts a Volume's shape itself.
func scanMedium(medium string, vol media.Volume) (mediumIndex, error) {
	acc := newScanMaps()
	var labels []record.Label
	err := media.WalkReadable(vol, func(v media.Volume) error {
		res, err := scanVolume(medium, v)
		if err != nil {
			return err
		}
		acc.add(res)
		if res.label != nil {
			labels = append(labels, *res.label)
		}
		return nil
	})
	if err != nil {
		return mediumIndex{}, err
	}
	return mediumIndex{placements: assemble(medium, acc), labels: labels}, nil
}

// assemble turns one medium's accumulated parts, commit footers, and member indexes into
// placements: each committed archive (one with a commit footer) gathers its parts (ordered by
// part index) from across the medium's volumes, and the committed archives are grouped by
// slot id into the in-memory slot. Parts with no commit footer are orphans (a crashed run) —
// skipped. A part missing from the scan (a tape not present) leaves a short part list —
// verify/restore reports the gap and fails over to another copy. Archives are ordered by
// (dle, level) so a rebuild is deterministic.
func assemble(medium string, acc *scanMaps) []slotPlacement {
	keys := make([]archiveKey, 0, len(acc.commits))
	for k := range acc.commits {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].slot != keys[j].slot {
			return keys[i].slot < keys[j].slot
		}
		if keys[i].dle != keys[j].dle {
			return keys[i].dle < keys[j].dle
		}
		return keys[i].level < keys[j].level
	})

	slots := map[string]*slotPlacement{}
	var order []string // slot ids in first-seen order
	for _, key := range keys {
		sc := acc.commits[key]
		sa := slots[key.slot]
		if sa == nil {
			sa = &slotPlacement{
				slot: &Slot{ID: key.slot},
				p:    Placement{Medium: medium},
			}
			slots[key.slot] = sa
			order = append(order, key.slot)
		}
		n := sc.arch.Parts
		if n < 1 {
			n = 1 // a single whole archive records Parts as 0 or 1
		}
		ap := ArchivePos{DLE: key.dle, Level: key.level, Commit: sc.loc}
		for part := 0; part < n; part++ {
			if loc, ok := acc.parts[partKey{slot: key.slot, dle: key.dle, level: key.level, part: part}]; ok {
				ap.Parts = append(ap.Parts, loc)
			}
		}
		if ixLoc, ok := acc.indexes[key]; ok {
			ap.Index = ixLoc // note where the member index lives; members load lazily (browse/verify)
		}
		arch := *sc.arch
		arch.Slot = key.slot // the header's slot tag is authoritative for grouping
		sa.slot.addArchive(arch)
		sa.p.Archives = append(sa.p.Archives, ap)
	}

	out := make([]slotPlacement, 0, len(order))
	for _, id := range order {
		out = append(out, *slots[id])
	}
	return out
}

// OrphanFiles scans one volume and returns the files that belong to no committed archive:
// parts and member indexes a crashed run left behind without ever writing their commit
// footer (and any file whose footer is present but unreadable, since its archive then never
// assembles). These are invisible to retention — assemble discards them, so the catalog never
// records them — yet they still consume the medium.
//
// Detection reads the volume's OWN commit footers (the on-medium ground truth), never the
// catalog cache: orphans are the readable files (vol.Files()) minus the positions of the
// archives a fresh scan assembles. This is the safety-critical property — a stale or empty
// cache can never make a committed archive look orphaned, because the same footer that proves
// an archive is good is what marks its files referenced here. Volume labels are never orphans.
// On any scan error nothing is returned, so a caller deletes nothing on a partial read.
//
// It sees only files committed at the medium layer; a torn append (a payload whose header
// sidecar never landed) is not surfaced here — that fragment is enumerated separately via
// media.IncompleteEnumerator.
func OrphanFiles(vol media.Volume) ([]record.FileInfo, error) {
	files, err := vol.Files()
	if err != nil {
		return nil, err
	}
	idx, err := scanMedium("", vol)
	if err != nil {
		return nil, err
	}
	referenced := map[int]bool{}
	for _, sp := range idx.placements {
		for _, ap := range sp.p.Archives {
			for _, pt := range ap.Parts {
				referenced[pt.Pos] = true
			}
			referenced[ap.Commit.Pos] = true
			if ap.Index != (record.FilePos{}) {
				referenced[ap.Index.Pos] = true
			}
		}
	}
	var orphans []record.FileInfo
	for _, f := range files {
		if f.Header.Kind == record.KindLabel || referenced[f.Pos] {
			continue
		}
		orphans = append(orphans, f)
	}
	return orphans, nil
}

// ScanSlots reads a volume's committed slots without touching the cache — used to check a
// volume's current contents (e.g. whether a tape is still active before relabel).
func ScanSlots(vol media.Volume) ([]*Slot, error) {
	res, err := scanVolume("", vol)
	if err != nil {
		return nil, err
	}
	acc := newScanMaps()
	acc.add(res)
	sps := assemble("", acc)
	slots := make([]*Slot, 0, len(sps))
	for _, sp := range sps {
		slots = append(slots, sp.slot)
	}
	return slots, nil
}

// partKey identifies one archive part within a slot across a medium's volumes.
type partKey struct {
	slot, dle   string
	level, part int
}

// archiveKey identifies one committed archive within a slot across a medium's volumes.
type archiveKey struct {
	slot, dle string
	level     int
}

// scannedCommit is a committed archive found during a scan: its footer metadata (without
// members) and where the footer landed.
type scannedCommit struct {
	arch *record.Archive
	loc  FilePos
}

// scanMaps holds the file locations a scan collects, keyed for assembly: each archive part's
// position, each committed archive's footer, and each member index. It serves two roles — one
// volume's contribution (embedded in scanResult) and the whole-medium accumulator that gathers
// them, since an archive's parts (and its commit/index) may straddle several of the medium's
// volumes.
type scanMaps struct {
	parts   map[partKey]FilePos
	commits map[archiveKey]scannedCommit
	indexes map[archiveKey]FilePos
}

func newScanMaps() *scanMaps {
	return &scanMaps{
		parts:   map[partKey]FilePos{},
		commits: map[archiveKey]scannedCommit{},
		indexes: map[archiveKey]FilePos{},
	}
}

// add merges one volume's scanned locations into the accumulator.
func (m *scanMaps) add(res scanResult) {
	for k, loc := range res.parts {
		m.parts[k] = loc // last-seen wins (an orphaned re-copy is harmless to reads)
	}
	for k, c := range res.commits {
		m.commits[k] = c
	}
	for k, loc := range res.indexes {
		m.indexes[k] = loc
	}
}

// scanResult is one volume's contribution to a medium scan: its location maps (parts, commit
// footers, member-index locations — the members are not read, that is lazy) plus the volume's
// label.
type scanResult struct {
	scanMaps
	label *record.Label
}

// scanVolume reads one volume's files into raw parts, commit footers, and member indexes,
// plus the volume's label. It does not assemble placements — that happens per medium, after
// every volume is scanned, because an archive's parts (and its commit/index) may sit on
// different volumes.
func scanVolume(medium string, vol media.Volume) (scanResult, error) {
	files, err := vol.Files()
	if err != nil {
		return scanResult{}, err
	}

	// Address-identified media (disk, s3) carry no label: the medium is its own sole
	// volume, so the part's label stays empty. Labeled (tape) media record the label
	// name + epoch read off the cartridge.
	labelName, epoch := "", 0
	var label *record.Label
	if lv, ok := vol.(media.Labeled); ok {
		if lbl, labeled, lerr := lv.ReadLabel(); lerr == nil && labeled {
			label = &lbl
			labelName, epoch = lbl.Name, lbl.Epoch
		}
	}

	res := scanResult{scanMaps: *newScanMaps(), label: label}
	for _, f := range files {
		loc := FilePos{Label: labelName, Epoch: epoch, Pos: f.Pos}
		switch f.Header.Kind {
		case record.KindArchive:
			res.parts[partKey{slot: f.Header.Slot, dle: f.Header.DLE, level: f.Header.Level, part: f.Header.Part}] = loc
		case record.KindCommit:
			a, cerr := readCommit(vol, f.Pos)
			if cerr != nil {
				continue // unreadable footer: skip (the archive reads as uncommitted)
			}
			res.commits[archiveKey{slot: f.Header.Slot, dle: f.Header.DLE, level: f.Header.Level}] =
				scannedCommit{arch: a, loc: loc}
		case record.KindIndex:
			// Note where the member index lives, but do NOT read it — members load lazily
			// (browse / structural verify), so a rebuild reads only small commit footers.
			res.indexes[archiveKey{slot: f.Header.Slot, dle: f.Header.DLE, level: f.Header.Level}] = loc
		}
	}
	return res, nil
}

// readCommit reads and parses an archive's commit footer payload from the volume.
func readCommit(vol media.Volume, pos int) (*record.Archive, error) {
	_, rc, err := vol.ReadFile(pos)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return record.ParseCommit(data)
}
