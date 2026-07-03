package archivefs

import (
	"io"

	"github.com/Niloen/nbackup/internal/archiveio"
)

// ReadItem is one archive in a multi-archive read selection: its logical identity plus the
// physical place of its first part (the copy's medium and the first part's position). It is
// what OrderForOnePass needs to sequence a selection into a one-pass read.
type ReadItem struct {
	Ref      archiveio.Ref     // Run, DLE, Level
	Medium   string            // the copy's medium
	FirstPos archiveio.FilePos // the archive's first part — orders the read within a medium
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

// OpenArchives reads a selection of archives in one ordered pass and drives the read
// itself. It resolves each ref to a copy and positions, orders them physically
// (OrderForOnePass), then reads them one at a time on a shared mounting opener (consecutive
// same-volume archives reuse the mount) — calling fn for each, in read order, with the
// archive's ref and an open func for a Source over its bytes. fn composes the per-archive
// transfer (decode → sink) and may open more than once (verify reads twice). Reading stops at
// the first error fn returns. medium "" copy-selects per ref; a set medium pins to that copy.
// Each open is copy-selecting with fail-over (openRef): the located copy orders the pass, but
// the open re-runs selection over every eligible copy, so a damaged or missing file behind a
// live catalog entry fails over to another copy before the fault surfaces — a set medium still
// confines that to its own placements, so a pinned read is never masked by another medium's copy.
//
// Refs with no available copy are not read; they are returned as missing for the caller to
// handle (a broken chain, a position-missing verdict). A copy whose medium cannot be acquired
// (not in this config, or refused because the open write-window holds it) is failed over like
// any unavailable copy; the fault surfaces only if no copy opens. OpenArchive is the single-ref
// special case of this.
func (fs *FS) OpenArchives(refs []archiveio.Ref, medium string, fn func(ref archiveio.Ref, open func() (io.ReadCloser, error)) error) (missing []archiveio.Ref, err error) {
	readers := map[string]*archiveio.Reader{}
	readerFor := func(m string) (*archiveio.Reader, error) {
		if r, ok := readers[m]; ok {
			return r, nil
		}
		r, e := fs.readerFor(m)
		if e != nil {
			return nil, e
		}
		readers[m] = r
		return r, nil
	}

	items := make([]ReadItem, 0, len(refs))
	for _, ref := range refs {
		m, first, ok, e := fs.planRead(ref, medium, readerFor)
		if e != nil {
			// The ref is catalogued but no copy's medium acquires (not in this config, or
			// a definitive fault): surface it as the pass error, distinct from a missing
			// ref, so a caller (verify) can classify an out-of-scope medium as a skip.
			return missing, e
		}
		if !ok {
			missing = append(missing, ref)
			continue
		}
		items = append(items, ReadItem{Ref: ref, Medium: m, FirstPos: first})
	}

	for _, it := range OrderForOnePass(items) {
		ref := it.Ref
		open := func() (io.ReadCloser, error) { return fs.openRef(ref, medium, readerFor) }
		if e := fn(ref, open); e != nil {
			return missing, e
		}
	}
	return missing, nil
}

// planRead resolves a ref to the copy the pass will order by: a set medium pins to that copy,
// "" takes read-preference order (the engine's own first). It returns the first placement that
// both carries the archive and whose medium acquires — priming the pooled reader so the read
// loop reuses the mount — so a copy whose medium is refused (window-held) or unknown is passed
// over just as a damaged file is failed over at read time. Three outcomes: (ok=true) an
// acquirable copy, ordered by its first part; (ok=false, err=nil) no copy carries the archive —
// the caller records it as missing; (err!=nil) a copy carries it but no carrying copy's medium
// acquires — a definitive read error the caller surfaces rather than a silent skip.
func (fs *FS) planRead(ref archiveio.Ref, medium string, readerFor func(string) (*archiveio.Reader, error)) (m string, first archiveio.FilePos, ok bool, err error) {
	var acqErr error
	for _, p := range fs.cat.PlacementsFor(ref.Run) {
		if medium != "" && p.Medium != medium {
			continue
		}
		parts, has := p.Parts(ref.DLE, ref.Level)
		if !has {
			continue
		}
		if _, e := readerFor(p.Medium); e != nil {
			acqErr = e // carries the archive but the medium won't open — try the next copy
			continue
		}
		return p.Medium, parts[0], true, nil
	}
	return "", archiveio.FilePos{}, false, acqErr
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
