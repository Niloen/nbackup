package catalog

import "github.com/Niloen/nbackup/internal/record"

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

func (e *Entry) placedOn(medium string) bool {
	for _, p := range e.Placements {
		if p.Medium == medium {
			return true
		}
	}
	return false
}
