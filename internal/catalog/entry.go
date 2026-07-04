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

// Labels returns the distinct volume labels this placement occupies — every
// archive part's label plus its commit footer's and index's — in first-seen order. It is what tells
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
