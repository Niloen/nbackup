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

// OpenArchives is the clerk's "open a set of archives" door: given logical refs, it locates
// each one's copy and positions itself (the caller supplies no positions), orders them into a
// one-pass read (OrderForOnePass), and threads one shared mounting opener per medium. medium
// "" copy-selects per ref (prefer the engine's own copy); a set medium pins to that copy
// (verify a specific copy, read a copy source). Refs with no available copy are skipped — the
// caller compares the returned jobs to its refs to detect them (a broken chain, a
// position-missing verdict). An opener that cannot be acquired (medium not in this config) is
// returned as the error.
func (c *Clerk) OpenArchives(refs []Ref, medium string) ([]ReadJob, error) {
	type loc struct {
		medium string
		parts  []record.FilePos
	}
	locs := map[Ref]loc{}
	items := make([]ReadItem, 0, len(refs))
	for _, ref := range refs {
		m, parts, ok := c.locate(ref, medium)
		if !ok {
			continue // no copy of this archive here; the caller detects the gap
		}
		locs[ref] = loc{medium: m, parts: parts}
		first := record.FilePos{}
		if len(parts) > 0 {
			first = parts[0]
		}
		items = append(items, ReadItem{Ref: ref, Medium: m, FirstPos: first})
	}

	openers := map[string]archiveio.PartOpener{}
	jobs := make([]ReadJob, 0, len(items))
	for _, it := range OrderForOnePass(items) {
		l := locs[it.Ref]
		op, ok := openers[l.medium]
		if !ok {
			var err error
			if op, err = c.PartOpener(l.medium); err != nil {
				return nil, err
			}
			openers[l.medium] = op
		}
		jobs = append(jobs, ReadJob{Ref: it.Ref, parts: l.parts, opener: op, clerk: c})
	}
	return jobs, nil
}

// locate resolves a ref to the copy that holds it: a set medium pins to that copy; "" takes
// the first placement in read-preference order (the engine's own copy first) that has the
// archive's parts.
func (c *Clerk) locate(ref Ref, medium string) (string, []record.FilePos, bool) {
	for _, p := range c.cat.PlacementsFor(ref.Slot) {
		if medium != "" && p.Medium != medium {
			continue
		}
		if parts, ok := p.Parts(ref.DLE, ref.Level); ok {
			return p.Medium, parts, true
		}
	}
	return "", nil, false
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
