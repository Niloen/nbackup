package catalog

import (
	"io"

	"github.com/Niloen/nbackup/internal/format"
	"github.com/Niloen/nbackup/internal/media"
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

// absorb merges one medium's scanned placements and volume labels into the store,
// without persisting. It is the seam between the importer and the store: each
// placement enters through upsert (so a slot seen on several media gathers several
// placements on one entry), and each label upserts the volume registry.
func (c *Catalog) absorb(idx mediumIndex) {
	for _, sp := range idx.placements {
		c.upsert(sp.slot, sp.p)
	}
	for _, lbl := range idx.labels {
		c.volumes[lbl.Name] = &VolumeRecord{Label: lbl}
	}
}

// mediumIndex is the assembled result of scanning one medium: each sealed slot
// with its placement on that medium, plus the labels of the volumes seen.
type mediumIndex struct {
	placements []slotPlacement
	labels     []format.Label
}

// slotPlacement pairs a sealed slot's content with its placement on the scanned
// medium, ready for the store to absorb.
type slotPlacement struct {
	slot *format.Slot
	p    Placement
}

// scanMedium reads every readable volume of one medium and assembles its sealed
// slots into placements. The shape walk — every non-blank bay of a robotic library,
// or just the loaded reel of a single-drive station — lives in media.WalkReadable,
// so the catalog never type-asserts a Volume's shape itself.
func scanMedium(medium string, vol media.Volume) (mediumIndex, error) {
	acc := newMediumScan()
	var labels []format.Label
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

// assemble turns one medium's accumulated part files and seals into placements: each
// sealed slot becomes one placement whose archives gather their parts (ordered by
// part index) from across the medium's volumes. A part missing from the scan (a tape
// not present) leaves a short part list — verify/restore reports the gap and fails
// over to another copy.
func assemble(medium string, acc *mediumScan) []slotPlacement {
	var out []slotPlacement
	for slotID, sl := range acc.seals {
		p := Placement{Medium: medium, Seal: sl.loc}
		for _, a := range sl.meta.Archives {
			n := a.Parts
			if n < 1 {
				n = 1 // a single whole archive records Parts as 0 or 1
			}
			ap := ArchivePos{DLE: a.DLE, Level: a.Level}
			for part := 0; part < n; part++ {
				if loc, ok := acc.parts[partKey{slot: slotID, dle: a.DLE, level: a.Level, part: part}]; ok {
					ap.Parts = append(ap.Parts, loc)
				}
			}
			p.Archives = append(p.Archives, ap)
		}
		out = append(out, slotPlacement{slot: sl.meta, p: p})
	}
	return out
}

// ScanSlots reads a volume's sealed slots without touching the cache — used to
// check a volume's current contents (e.g. whether a tape is still active before
// relabel).
func ScanSlots(vol media.Volume) ([]*format.Slot, error) {
	res, err := scanVolume("", vol)
	if err != nil {
		return nil, err
	}
	slots := make([]*format.Slot, 0, len(res.seals))
	for _, s := range res.seals {
		slots = append(slots, s.meta)
	}
	return slots, nil
}

// partKey identifies one archive part within a slot across a medium's volumes.
type partKey struct {
	slot, dle   string
	level, part int
}

// scannedSeal is a seal record found during a scan: the slot it commits and where it
// lives.
type scannedSeal struct {
	meta *format.Slot
	loc  FilePos
}

// scanResult is one volume's contribution to a medium scan: its archive part files,
// its seals, and its label (if any).
type scanResult struct {
	parts map[partKey]FilePos
	seals map[string]scannedSeal
	label *format.Label
}

// mediumScan accumulates a whole medium's parts and seals across its volumes before
// placements are assembled (a slot's parts may straddle several volumes, and the seal
// committing them lives on only one).
type mediumScan struct {
	parts map[partKey]FilePos
	seals map[string]scannedSeal
}

func newMediumScan() *mediumScan {
	return &mediumScan{parts: map[partKey]FilePos{}, seals: map[string]scannedSeal{}}
}

func (m *mediumScan) add(res scanResult) {
	for k, loc := range res.parts {
		m.parts[k] = loc // last-seen wins (an orphaned re-copy is harmless to reads)
	}
	for slotID, s := range res.seals {
		m.seals[slotID] = s
	}
}

// scanVolume reads one volume's files into raw part-file and seal records, plus the
// volume's label. It does not assemble placements — that happens per medium, after
// every volume is scanned, because a slot's parts (and its committing seal) may sit
// on different volumes.
func scanVolume(medium string, vol media.Volume) (scanResult, error) {
	files, err := vol.Files()
	if err != nil {
		return scanResult{}, err
	}

	// Address-identified media (disk, s3) carry no label: the medium is its own sole
	// volume, so the part's label stays empty. Labeled (tape) media record the label
	// name + epoch read off the cartridge.
	labelName, epoch := "", 0
	var label *format.Label
	if lv, ok := vol.(media.Labeled); ok {
		if lbl, labeled, lerr := lv.ReadLabel(); lerr == nil && labeled {
			label = &lbl
			labelName, epoch = lbl.Name, lbl.Epoch
		}
	}

	res := scanResult{parts: map[partKey]FilePos{}, seals: map[string]scannedSeal{}, label: label}
	for _, f := range files {
		switch f.Header.Kind {
		case format.KindArchive:
			res.parts[partKey{slot: f.Header.Slot, dle: f.Header.DLE, level: f.Header.Level, part: f.Header.Part}] =
				FilePos{Label: labelName, Epoch: epoch, Pos: f.Pos}
		case format.KindSeal:
			s, serr := readSeal(vol, f.Pos)
			if serr != nil {
				continue // unreadable seal: skip
			}
			res.seals[f.Header.Slot] = scannedSeal{meta: s, loc: FilePos{Label: labelName, Epoch: epoch, Pos: f.Pos}}
		}
	}
	return res, nil
}

// readSeal reads and parses a slot's seal-record payload from the volume.
func readSeal(vol media.Volume, pos int) (*format.Slot, error) {
	_, rc, err := vol.ReadFile(pos)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return format.ParseSlot(data)
}
