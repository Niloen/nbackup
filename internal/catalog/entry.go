package catalog

import (
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/record"
)

// Entry is the catalog's per-run record: one logical run plus every place a
// copy of it lives.
type Entry struct {
	Run        *Run        `json:"run"`        // medium-independent content, grouped from the archives' commit footers
	Placements []Placement `json:"placements"` // one per medium holding a copy
}

// Placement is one copy of a run on one medium. The copy's archives may span
// several of the medium's volumes (tape spanning): each archive names the volumes
// and positions its parts landed on, plus where its per-archive commit footer and
// member index live (the commit footer is the marker, written last).
type Placement struct {
	// Medium is the pool this copy is accounted to (retention, capacity, cost) and
	// the changer to open to reach it. It is NOT a device pin: a labeled volume is
	// located by its label at mount time, so which drive holds a tape is resolved at
	// runtime, not stored here.
	Medium string `json:"medium"`
	// Archives lists each archive held here and the positions of its parts, commit,
	// and index — see PlacedArchive.
	Archives []PlacedArchive `json:"archives"`
}

// PlacedArchive is the catalog's persisted record of one archive on one placement: the
// archive's key within the run's copy (DLE, level — the run itself is the entry's) and
// where its files landed. The location fields mirror archiveio.ArchivePos (the writer's
// commit output; Pos() converts back for positional read-back/reclaim), but the
// serialized shape is the catalog's own: the cache file's layout must not shift under a
// refactor of the block layer's call vocabulary. archiveio.FilePos is the shared atom —
// its Label is the volume's global, device-independent identity ("" for
// address-identified media, which carry no label); Parts is ordered (one part unless
// the archive spanned).
type PlacedArchive struct {
	DLE    string              `json:"dle"`
	Level  int                 `json:"level"`
	Parts  []archiveio.FilePos `json:"parts"`
	Seals  []record.PartSeal   `json:"seals,omitempty"` // per-part seals, index-aligned with Parts (from this placement's commit footer); empty when the footer predates seals or a scan found only some parts
	Commit archiveio.FilePos   `json:"commit"`          // the commit footer's location (the archive's marker)
	Index  archiveio.FilePos   `json:"index,omitempty"` // the member index's location (zero = no members)
}

// Pos returns the archive's location as the block layer's position value — what
// positional read-back and reclaim (WriteStore.OpenArchiveAt/ReclaimAt) take.
func (a PlacedArchive) Pos() archiveio.ArchivePos {
	return archiveio.ArchivePos{Parts: a.Parts, Commit: a.Commit, Index: a.Index}
}

// IOParts returns the archive's parts as the block layer's read vocabulary: each
// part's position zipped with its seal. When the recorded seals do not align 1:1 with
// the parts (a sealless footer, a scan that saw only some parts) the parts come back
// unsealed — misalignment is unrepresentable in []archiveio.Part, so the pairing rule
// lives here, at the persisted record's edge, once.
func (a PlacedArchive) IOParts() []archiveio.Part {
	parts := archiveio.BareParts(a.Parts)
	if len(a.Seals) == len(a.Parts) {
		for i := range parts {
			parts[i].Seal = a.Seals[i]
		}
	}
	return parts
}

// Parts returns the ordered part locations of an archive on this placement.
func (p Placement) Parts(dle string, level int) ([]archiveio.FilePos, bool) {
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return a.Parts, len(a.Parts) > 0
		}
	}
	return nil, false
}

// Placed returns the full placed-archive record of (dle, level) on this copy — parts,
// seals, commit, index — for callers that need more than the part positions (a sampling
// check reads the per-part seals).
func (p Placement) Placed(dle string, level int) (PlacedArchive, bool) {
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return a, true
		}
	}
	return PlacedArchive{}, false
}

// Holds reports whether this copy records the archive at all. A copy is
// archive-granular: a per-archive prune can reclaim one DLE's image from one
// medium while the run's other archives (and other media's copies) survive, so
// a placement may hold only a subset of the run's content. (Unlike Parts, a
// recorded archive whose parts were not all seen — a tape absent from a scan —
// still counts as held: the copy claims it, so verify must judge it.)
func (p Placement) Holds(dle string, level int) bool {
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return true
		}
	}
	return false
}

// Labeled reports whether this copy lives on labeled volumes (tape) rather than at
// addresses (disk, cloud) — the placement-side face of the media.Labeled capability,
// read from the recorded positions alone (an address-identified medium records empty
// labels; see record.FilePos), so no medium is opened to ask. The distinction is
// load-bearing for reclamation: a labeled copy is out of per-archive reach by
// construction — nothing can be deleted from the middle of a reel — so it dies only
// with its whole volume, at relabel; a copy trusted by address is prune's to delete.
func (p Placement) Labeled() bool { return len(p.Labels()) > 0 }

// Labels returns the distinct volume labels this placement occupies — every
// archive's labels (see PlacedArchive.Labels), merged in first-seen order. It is
// what tells which tapes a copy needs mounted. It is empty for address-identified
// media, which carry no labels: a disk/s3 copy spans no tapes, and is reached by
// its medium alone (Placement.Medium), needing no label-based mount.
func (p Placement) Labels() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range p.Archives {
		for _, v := range a.Labels() {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// Labels returns the distinct volume labels this one archive occupies — its
// parts' labels plus its commit footer's and index's — in first-seen order.
// Placement.Labels aggregates this across a copy's whole archive list; per-volume
// accounting needs the per-archive view instead, to attribute a spanned archive's
// bytes to one volume without double-counting it against every volume it touches.
func (a PlacedArchive) Labels() []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, pt := range a.Parts {
		add(pt.Label)
	}
	add(a.Commit.Label)
	add(a.Index.Label)
	return out
}

// Missing returns the subset of the given archives this copy does not hold — the
// gap a copy or sync must carry to make this placement whole. A zero Placement
// holds nothing, so Missing returns them all. It is the one coverage question
// behind copy's resume set, sync's backlog, and auto source resolution.
func (p Placement) Missing(archives []record.Archive) []record.Archive {
	var out []record.Archive
	for _, a := range archives {
		if !p.Holds(a.DLE, a.Level) {
			out = append(out, a)
		}
	}
	return out
}

// OnLabel reports whether any file of this placement (a part, its commit footer, or
// its index) lives on the volume with the given label.
func (p Placement) OnLabel(label string) bool {
	for _, v := range p.Labels() {
		if v == label {
			return true
		}
	}
	return false
}

func (e *Entry) placedOn(medium string) bool {
	for _, p := range e.Placements {
		if p.Medium == medium {
			return true
		}
	}
	return false
}

// anyPlacementHolds reports whether any of the entry's copies still holds an archive
// of the DLE (at any level).
func (e *Entry) anyPlacementHolds(dle string) bool {
	for _, p := range e.Placements {
		for _, a := range p.Archives {
			if a.DLE == dle {
				return true
			}
		}
	}
	return false
}

// placementOn returns the entry's copy on the named medium, if any.
func (e *Entry) placementOn(medium string) (Placement, bool) {
	for _, p := range e.Placements {
		if p.Medium == medium {
			return p, true
		}
	}
	return Placement{}, false
}
