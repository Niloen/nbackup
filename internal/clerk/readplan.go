package clerk

import (
	"io"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/record"
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

// ReadArchives reads a selection of archives in one ordered pass and drives the read
// itself. It resolves each ref to a copy and positions, orders them physically
// (OrderForOnePass), then reads them one at a time on a shared mounting opener (consecutive
// same-volume archives reuse the mount) — calling fn for each, in read order, with the
// archive's ref and an open func for a Source over its bytes. fn composes the per-archive
// transfer (decode → sink) and may open more than once (verify reads twice). Reading stops at
// the first error fn returns. medium "" copy-selects per ref; a set medium pins to that copy.
//
// Refs with no available copy are not read; they are returned as missing for the caller to
// handle (a broken chain, a position-missing verdict). An opener that cannot be acquired
// (medium not in this config) is the error.
func (c *Clerk) ReadArchives(refs []Ref, medium string, fn func(ref Ref, open func() (io.ReadCloser, error)) error) (missing []Ref, err error) {
	type loc struct {
		medium string
		parts  []record.FilePos
	}
	locs := map[Ref]loc{}
	items := make([]ReadItem, 0, len(refs))
	for _, ref := range refs {
		m, parts, ok := c.locate(ref, medium)
		if !ok {
			missing = append(missing, ref)
			continue
		}
		locs[ref] = loc{medium: m, parts: parts}
		first := record.FilePos{}
		if len(parts) > 0 {
			first = parts[0]
		}
		items = append(items, ReadItem{Ref: ref, Medium: m, FirstPos: first})
	}

	openers := map[string]archiveio.PartOpener{}
	opener := func(m string) (archiveio.PartOpener, error) {
		if op, ok := openers[m]; ok {
			return op, nil
		}
		op, e := c.partOpener(m)
		if e != nil {
			return nil, e
		}
		openers[m] = op
		return op, nil
	}

	for _, it := range OrderForOnePass(items) {
		l := locs[it.Ref]
		op, e := opener(l.medium)
		if e != nil {
			return missing, e
		}
		ref := it.Ref
		open := func() (io.ReadCloser, error) {
			want := archiveio.Expect{Slot: ref.Slot, DLE: ref.DLE, Level: ref.Level}
			return c.reader.Open(l.parts, want, op)
		}
		if e := fn(ref, open); e != nil {
			return missing, e
		}
	}
	return missing, nil
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
