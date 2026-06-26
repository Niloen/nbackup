package clerk

import (
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// ReadItem is one archive in a multi-archive read selection: its logical identity plus the
// physical place of its first part (the copy's medium and the first part's position). It is
// what OrderForOnePass needs to sequence a selection into a one-pass read.
type ReadItem struct {
	Ref      Ref            // Slot, DLE, Level
	Medium   string         // the copy's medium
	FirstPos record.FilePos // the archive's first part — orders the read within a medium
}

// OrderForOnePass sequences a selection of archives into a one-pass read order: a topological
// sort that keeps each DLE's levels ascending (L0 before L1 before L2 — tar's listed-
// incremental restore must *apply* in level order), using physical layout (medium, then the
// first part's label/epoch/position) as the tiebreak among archives with no pending
// predecessor. So independent archives are read forward to minimize volume mounts, while a
// restore chain is never reordered out of level order. It is a pure function — the resolution
// (refs → ReadItems) and the actual streaming are the caller's.
//
// This is the answer to "can we guarantee L0→L1": yes — an item is emitted only when no
// lower-level item of the same DLE remains, so a chain can never come out of order; the
// physical tiebreak operates only on the free dimensions (independent DLEs). In a rare
// adversarial cross-volume interleaving the forward-read property yields to that ordering and
// a remount happens, but level order is always honored.
func OrderForOnePass(items []ReadItem) []ReadItem {
	remaining := append([]ReadItem(nil), items...)
	out := make([]ReadItem, 0, len(remaining))
	for len(remaining) > 0 {
		// The lowest still-unemitted level per DLE: only those items are "ready" (every
		// lower level of their DLE has already been emitted).
		minLevel := map[string]int{}
		for _, it := range remaining {
			if l, ok := minLevel[it.Ref.DLE]; !ok || it.Ref.Level < l {
				minLevel[it.Ref.DLE] = it.Ref.Level
			}
		}
		// Among the ready items, take the physically-earliest (read forward).
		best := -1
		for i, it := range remaining {
			if it.Ref.Level != minLevel[it.Ref.DLE] {
				continue
			}
			if best == -1 || physicallyBefore(it, remaining[best]) {
				best = i
			}
		}
		out = append(out, remaining[best])
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	return out
}

// ArchiveLoc is one archive's physical location on a copy: the parts to stream and where the
// first one lies (for ordering). The caller supplies it from a catalog placement.
type ArchiveLoc struct {
	Ref   Ref
	Parts []record.FilePos
}

// ReadJob is one archive in a planned read — its identity and a Source over its parts opened
// on the plan's shared opener.
type ReadJob struct {
	Ref    Ref
	parts  []record.FilePos
	opener archiveio.PartOpener
	clerk  *Clerk
}

// Source opens the job's parts on the plan's shared opener (the mounted volume is reused
// across consecutive same-volume jobs — one pass).
func (j ReadJob) Source() (xfer.Source, error) {
	want := archiveio.Expect{Slot: j.Ref.Slot, DLE: j.Ref.DLE, Level: j.Ref.Level}
	return j.clerk.PartsSource(j.parts, want, j.opener)
}

// PlanPinned builds a one-pass read over a set of archives on one medium (verify, copy): it
// orders them physically (OrderForOnePass) and threads one shared mounting opener, so a
// multi-archive read walks the volume forward instead of remounting per archive. The order of
// the returned jobs is the read order.
func (c *Clerk) PlanPinned(medium string, locs []ArchiveLoc) ([]ReadJob, error) {
	opener, err := c.PartOpener(medium)
	if err != nil {
		return nil, err
	}
	items := make([]ReadItem, len(locs))
	parts := make(map[Ref][]record.FilePos, len(locs))
	for i, l := range locs {
		first := record.FilePos{}
		if len(l.Parts) > 0 {
			first = l.Parts[0]
		}
		items[i] = ReadItem{Ref: l.Ref, Medium: medium, FirstPos: first}
		parts[l.Ref] = l.Parts
	}
	jobs := make([]ReadJob, 0, len(items))
	for _, it := range OrderForOnePass(items) {
		jobs = append(jobs, ReadJob{Ref: it.Ref, parts: parts[it.Ref], opener: opener, clerk: c})
	}
	return jobs, nil
}

// physicallyBefore orders two archives by where their first part lies: medium, then the
// volume (label), then its epoch, then the byte position — so reads on one volume stay
// contiguous and move forward.
func physicallyBefore(a, b ReadItem) bool {
	if a.Medium != b.Medium {
		return a.Medium < b.Medium
	}
	if a.FirstPos.Label != b.FirstPos.Label {
		return a.FirstPos.Label < b.FirstPos.Label
	}
	if a.FirstPos.Epoch != b.FirstPos.Epoch {
		return a.FirstPos.Epoch < b.FirstPos.Epoch
	}
	return a.FirstPos.Pos < b.FirstPos.Pos
}
