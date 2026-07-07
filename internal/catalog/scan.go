package catalog

import (
	"io"
	"sort"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// scan.go is the catalog's importer: it reads the media (the source of truth) and
// rebuilds the store from it. It speaks the media's vocabulary — slots, mounts,
// archive parts, commit footers, labels — and ends by handing finished placements back to
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

// Rebuild rescans the given media (keyed by medium name). A run seen on several
// volumes yields several placements on one logical entry.
//
// Additive by default (full=false): the scan MERGES into the existing catalog —
// what a scanned volume shows replaces the catalog's prior beliefs about that
// volume (a relabeled reel's stale records are dropped, an unchanged archive
// re-records as a net no-op), while volumes the scan never reached keep their
// records. That is the disaster-recovery flow: feed tapes one at a time, running
// Rebuild after each, until the report lists nothing missing — the commit
// footers' part maps name the tapes still needed. full=true wipes first and
// rebuilds only from what is reachable now (the reconciliation of last resort;
// it also forgets ghost registry entries a rename left behind).
//
// The report carries the run count and the gaps a caller should surface: runs
// whose parts were seen but whose commit footer was not (more tapes exist).
func (c *Catalog) Rebuild(volumes map[string]media.Volume, full bool) (RebuildReport, error) {
	if full {
		c.entries = nil
		c.volumes = map[string]*VolumeRecord{}
	}
	var rep RebuildReport
	for medium, vol := range volumes {
		idx, err := scanMedium(medium, vol)
		if err != nil {
			return RebuildReport{}, err
		}
		// A scanned volume is whole-volume truth: records referencing its label at
		// another epoch describe a reel that has been physically wiped since —
		// drop them (giving their charges back) before absorbing the fresh scan.
		for _, lbl := range idx.labels {
			c.dropStaleOn(lbl)
		}
		c.absorb(idx)
		rep.OrphanRuns = append(rep.OrphanRuns, idx.orphans...)
	}
	c.sortEntries()
	c.loaded = true
	if err := c.persist(); err != nil {
		return RebuildReport{}, err
	}
	rep.Runs = len(c.entries)
	// A part-only tape of a run already indexed (its footer absorbed from another
	// tape, possibly a previous additive pass) is resolved, not orphaned.
	unresolved := rep.OrphanRuns[:0:0]
	for _, o := range rep.OrphanRuns {
		if c.entryByID(o.Run) == nil {
			unresolved = append(unresolved, o)
		}
	}
	rep.OrphanRuns = unresolved
	sort.Slice(rep.OrphanRuns, func(i, j int) bool { return rep.OrphanRuns[i].Run < rep.OrphanRuns[j].Run })
	return rep, nil
}

// RebuildReport is what a rebuild pass found and what it could not resolve.
type RebuildReport struct {
	Runs int // distinct runs now indexed
	// OrphanRuns lists runs whose part files were scanned but whose commit footer
	// was not found on any scanned volume: the run cannot be indexed yet, and the
	// tape holding its footer is presumably among the ones not fed in — the cue to
	// keep inserting tapes and re-running an additive rebuild.
	OrphanRuns []OrphanRun
}

// OrphanRun is one such unresolved run: its id and the scanned labels holding its
// footerless parts.
type OrphanRun struct {
	Run    string
	Labels []string
}

// dropStaleOn removes placed archives referencing the given label at a different
// epoch than the reel now carries: the reel was physically wiped (relabeled)
// since they were recorded, so those files are provably gone. Spanning siblings
// on other reels become orphans, exactly as a live recycle leaves them; each
// dropped record gives its fill charges back.
func (c *Catalog) dropStaleOn(lbl record.Label) {
	staleRef := func(fp archiveio.FilePos) bool {
		return fp.Label == lbl.Name && fp.Epoch != lbl.Epoch
	}
	stale := func(pa PlacedArchive) bool {
		for _, pt := range pa.Parts {
			if staleRef(pt) {
				return true
			}
		}
		return staleRef(pa.Commit) || (pa.Index.Label != "" && staleRef(pa.Index))
	}
	entries := c.entries[:0:0]
	for _, e := range c.entries {
		placements := e.Placements[:0:0]
		for _, p := range e.Placements {
			kept := p.Archives[:0:0]
			for _, pa := range p.Archives {
				if stale(pa) {
					c.applyFill(p.Medium, e.Run, pa, -1)
					continue
				}
				kept = append(kept, pa)
			}
			p.Archives = kept
			if len(p.Archives) > 0 {
				placements = append(placements, p)
			}
		}
		e.Placements = placements
		if len(e.Placements) > 0 {
			entries = append(entries, e)
		}
	}
	c.entries = entries
}

// absorb merges one medium's scanned placements and volume labels into the store, without
// persisting (the caller persists once). It is the seam between the importer and the store:
// each scanned archive enters through the in-memory addArchive — the same single write path a
// dump uses — so a run seen on several media gathers several placements on one entry, its
// content taken from the archives' commit footers. Each label upserts the volume registry.
func (c *Catalog) absorb(idx mediumIndex) {
	// Labels first: the upsert starts each reel's stored fill at its label file,
	// then each absorbed placement adds its charge through the same addArchive
	// pricing the live write path uses — a rebuild reconstructs Used for free.
	for _, lbl := range idx.labels {
		c.upsertVolume(lbl)
	}
	for _, sp := range idx.placements {
		for _, arch := range sp.run.Archives {
			if pa, ok := findPlaced(sp.p.Archives, arch.DLE, arch.Level); ok {
				c.addArchive(arch, sp.p.Medium, pa.Pos())
			}
		}
	}
}

// findPlaced returns the placed archive of (dle, level) among a placement's archives.
func findPlaced(pas []PlacedArchive, dle string, level int) (PlacedArchive, bool) {
	for _, pa := range pas {
		if pa.DLE == dle && pa.Level == level {
			return pa, true
		}
	}
	return PlacedArchive{}, false
}

// mediumIndex is the assembled result of scanning one medium: each run assembled from its
// committed archives, with its placement on that medium, the labels of the volumes seen,
// and the runs whose parts were seen without a commit footer (see OrphanRun).
type mediumIndex struct {
	placements []runPlacement
	labels     []record.Label
	orphans    []OrphanRun
}

// runPlacement pairs a run's content with its placement on the scanned
// medium, ready for the store to absorb.
type runPlacement struct {
	run *Run
	p   Placement
}

// scanMedium reads every readable volume of one medium and assembles its committed
// archives into placements. The shape walk — every non-blank slot of a robotic library,
// or just the loaded reel of a single-drive station — lives in media.WalkReadable,
// so the catalog never type-asserts a Volume's shape itself.
func scanMedium(medium string, vol media.Volume) (mediumIndex, error) {
	acc := newScanMaps()
	var labels []record.Label
	err := media.WalkReadable(vol, func(v media.Volume) error {
		res, err := scanVolume(v)
		if err != nil {
			return err
		}
		// A labeled volume belongs to its label's pool — the medium of the same
		// name. A cartridge from ANOTHER pool sharing this physical changer is not
		// this medium's to absorb: skipping it keeps two pools on one library from
		// bleeding into each other's placements and aggregates (that pool's own
		// rebuild scans it under its own medium name).
		if res.label != nil && res.label.Pool != "" && res.label.Pool != medium {
			return nil
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
	placements, orphans := assemble(medium, acc)
	return mediumIndex{placements: placements, labels: labels, orphans: orphans}, nil
}

// assemble turns one medium's accumulated parts, commit footers, and member indexes into
// placements: each committed archive (one with a commit footer) gathers its parts (ordered by
// part index) from across the medium's volumes, and the committed archives are grouped by
// run id into the in-memory run. Parts with no commit footer are orphans (a crashed run) —
// skipped. A part missing from the scan (a tape not present) leaves a short part list —
// verify/restore reports the gap and fails over to another copy. Archives are ordered by
// (dle, level) so a rebuild is deterministic.
//
// Parts left over — sighted on a scanned volume but matched to no commit footer —
// come back as OrphanRuns: either a crashed run's true leftovers, or (the case a
// partial rebuild must surface) a committed run whose footer tape simply was not
// among the volumes fed in yet.
func assemble(medium string, acc *scanMaps) ([]runPlacement, []OrphanRun) {
	keys := make([]archiveKey, 0, len(acc.commits))
	for k := range acc.commits {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].run != keys[j].run {
			return keys[i].run < keys[j].run
		}
		if keys[i].dle != keys[j].dle {
			return keys[i].dle < keys[j].dle
		}
		return keys[i].level < keys[j].level
	})

	runs := map[string]*runPlacement{}
	var order []string // run ids in first-seen order
	consumed := map[partKey]bool{}
	for _, key := range keys {
		sc := acc.commits[key]
		sa := runs[key.run]
		if sa == nil {
			sa = &runPlacement{
				run: &Run{ID: key.run},
				p:   Placement{Medium: medium},
			}
			runs[key.run] = sa
			order = append(order, key.run)
		}
		n := sc.arch.Parts
		if n < 1 {
			n = 1 // a single whole archive records Parts as 0 or 1
		}
		ap := PlacedArchive{DLE: key.dle, Level: key.level, Commit: sc.loc}
		for part := 0; part < n; part++ {
			pk := partKey{run: key.run, dle: key.dle, level: key.level, part: part}
			if loc, ok := acc.parts[pk]; ok {
				consumed[pk] = true
				ap.Parts = append(ap.Parts, loc) // a physically sighted part is the strongest truth
				continue
			}
			if part < len(sc.arch.PartMap) {
				// The scan never saw this part's volume, but the footer's part map —
				// the archive's TOC — names it: record the location so the placement
				// stays complete (aligned seals, correct part indexes) and a restore
				// can prompt for the absent reel by label.
				pm := sc.arch.PartMap[part]
				ap.Parts = append(ap.Parts, archiveio.FilePos{Label: pm.Label, Epoch: pm.Epoch, Pos: pm.Pos})
			}
		}
		if ixLoc, ok := acc.indexes[key]; ok {
			ap.Index = ixLoc // note where the member index lives; members load lazily (browse/verify)
		}
		arch := *sc.arch
		arch.Run = key.run // the header's run tag is authoritative for grouping
		sa.run.addArchive(arch)
		sa.p.Archives = append(sa.p.Archives, ap)
	}

	out := make([]runPlacement, 0, len(order))
	for _, id := range order {
		out = append(out, *runs[id])
	}

	// Group the unmatched parts by run: labels holding files of runs this scan
	// could not index (no commit footer among the scanned volumes).
	orphanLabels := map[string]map[string]bool{}
	for pk, loc := range acc.parts {
		if consumed[pk] || loc.Label == "" {
			continue
		}
		if orphanLabels[pk.run] == nil {
			orphanLabels[pk.run] = map[string]bool{}
		}
		orphanLabels[pk.run][loc.Label] = true
	}
	var orphans []OrphanRun
	for run, labels := range orphanLabels {
		names := make([]string, 0, len(labels))
		for l := range labels {
			names = append(names, l)
		}
		sort.Strings(names)
		orphans = append(orphans, OrphanRun{Run: run, Labels: names})
	}
	return out, orphans
}

// OrphanFiles returns the files on a volume that belong to no committed archive: parts and
// member indexes a crashed run left behind without ever writing their commit footer (and any
// file whose footer is present but unreadable, since its archive then never assembles). These
// are invisible to retention — assemble discards them, so the catalog never records them —
// yet they still consume the medium.
//
// It reads only the SURPRISES. known is the set of positions the caller (the prune sweep,
// from the catalog's own placements) already accounts for on this medium; those files are
// skipped without a read, and only the leftover positions are opened and classified. On a
// cloud store that is the difference between a couple of list calls and a network round trip
// per object, so a healthy bucket is diffed almost for free. When known is empty (a lost or
// empty cache) nothing is excluded and this degrades to a full medium scan.
//
// Detection stays MEDIUM-TRUTH for every candidate: a surprise is reported as an orphan only
// after this reads the medium's own commit footers among the surprises and finds none
// referencing it. So a committed-but-uncatalogued archive (the stale-cache danger) still
// shows its footer here and is kept — excluding the known set only ever narrows what is read
// and deleted, never the reverse, so it can never make a committed archive look orphaned.
// Volume labels are never orphans. On any read error nothing is returned, so a caller deletes
// nothing on a partial read.
//
// A fully-committed but superseded copy (a rare forced-re-copy leftover, all parts + footer
// present at other positions) assembles from its own footer among the surprises and is thus
// kept rather than swept; those are prevented proactively by ReclaimCopy, so this is a benign
// capacity edge, not the crash-leftover case this targets.
//
// It sees only files committed at the medium layer; a torn append (a payload whose header
// sidecar never landed) is not surfaced here — that fragment is enumerated separately via
// media.IncompleteEnumerator.
func OrphanFiles(vol media.Volume, known map[int]bool) ([]record.FileInfo, error) {
	files, err := surpriseFiles(vol, known)
	if err != nil {
		return nil, err
	}
	// Assemble any committed archives hiding among the surprises (a stale cache the known
	// set did not cover), so their files are recognized as referenced and never swept. On a
	// healthy store the surprise set holds no commit footers, so this reads nothing more.
	acc := newScanMaps()
	for _, f := range files {
		loc := archiveio.FilePos{Pos: f.Pos} // address-identified medium: no label/epoch
		switch f.Header.Kind {
		case record.KindArchive:
			acc.parts[partKey{run: f.Header.Run, dle: f.Header.DLE, level: f.Header.Level, part: f.Header.Part}] = loc
		case record.KindCommit:
			a, cerr := readCommit(vol, f.Pos)
			if cerr != nil {
				continue // unreadable footer: its archive reads as uncommitted (a real orphan)
			}
			acc.commits[archiveKey{run: f.Header.Run, dle: f.Header.DLE, level: f.Header.Level}] =
				scannedCommit{arch: a, loc: loc}
		case record.KindIndex:
			acc.indexes[archiveKey{run: f.Header.Run, dle: f.Header.DLE, level: f.Header.Level}] = loc
		}
	}
	referenced := map[int]bool{}
	assembled, _ := assemble("", acc)
	for _, sp := range assembled {
		for _, ap := range sp.p.Archives {
			for _, pt := range ap.Parts {
				referenced[pt.Pos] = true
			}
			referenced[ap.Commit.Pos] = true
			if ap.Index != (archiveio.FilePos{}) {
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

// surpriseFiles lists a volume's files while skipping the known positions. A per-file medium
// (fslike: disk, cloud) enumerates only the unknown files' headers via KnownExcluder — the
// whole point, so a large store is not re-read object by object. Any other medium (tape never
// reaches the orphan sweep) falls back to a full Files() pass filtered by known.
func surpriseFiles(vol media.Volume, known map[int]bool) ([]record.FileInfo, error) {
	if ex, ok := vol.(media.KnownExcluder); ok {
		return ex.FilesExcept(known)
	}
	files, err := vol.Files()
	if err != nil {
		return nil, err
	}
	out := files[:0]
	for _, f := range files {
		if !known[f.Pos] {
			out = append(out, f)
		}
	}
	return out, nil
}

// ScanRuns reads a volume's committed runs without touching the cache — used to check a
// volume's current contents (e.g. whether a tape is still active before relabel).
func ScanRuns(vol media.Volume) ([]*Run, error) {
	res, err := scanVolume(vol)
	if err != nil {
		return nil, err
	}
	acc := newScanMaps()
	acc.add(res)
	sps, _ := assemble("", acc)
	runs := make([]*Run, 0, len(sps))
	for _, sp := range sps {
		runs = append(runs, sp.run)
	}
	return runs, nil
}

// partKey identifies one archive part within a run across a medium's volumes.
type partKey struct {
	run, dle    string
	level, part int
}

// archiveKey identifies one committed archive within a run across a medium's volumes.
type archiveKey struct {
	run, dle string
	level    int
}

// scannedCommit is a committed archive found during a scan: its footer metadata (without
// members) and where the footer landed.
type scannedCommit struct {
	arch *record.Archive
	loc  archiveio.FilePos
}

// scanMaps holds the file locations a scan collects, keyed for assembly: each archive part's
// position, each committed archive's footer, and each member index. It serves two roles — one
// volume's contribution (embedded in scanResult) and the whole-medium accumulator that gathers
// them, since an archive's parts (and its commit/index) may straddle several of the medium's
// volumes.
type scanMaps struct {
	parts   map[partKey]archiveio.FilePos
	commits map[archiveKey]scannedCommit
	indexes map[archiveKey]archiveio.FilePos
}

func newScanMaps() *scanMaps {
	return &scanMaps{
		parts:   map[partKey]archiveio.FilePos{},
		commits: map[archiveKey]scannedCommit{},
		indexes: map[archiveKey]archiveio.FilePos{},
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
func scanVolume(vol media.Volume) (scanResult, error) {
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
		loc := archiveio.FilePos{Label: labelName, Epoch: epoch, Pos: f.Pos}
		switch f.Header.Kind {
		case record.KindArchive:
			res.parts[partKey{run: f.Header.Run, dle: f.Header.DLE, level: f.Header.Level, part: f.Header.Part}] = loc
		case record.KindCommit:
			a, cerr := readCommit(vol, f.Pos)
			if cerr != nil {
				continue // unreadable footer: skip (the archive reads as uncommitted)
			}
			res.commits[archiveKey{run: f.Header.Run, dle: f.Header.DLE, level: f.Header.Level}] =
				scannedCommit{arch: a, loc: loc}
		case record.KindIndex:
			// Note where the member index lives, but do NOT read it — members load lazily
			// (browse / structural verify), so a rebuild reads only small commit footers.
			res.indexes[archiveKey{run: f.Header.Run, dle: f.Header.DLE, level: f.Header.Level}] = loc
		}
	}
	return res, nil
}

// readCommit reads and parses an archive's commit footer payload from the volume.
func readCommit(vol media.Volume, pos int) (*record.Archive, error) {
	_, rc, err := vol.ReadFile(pos, media.Range{})
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
