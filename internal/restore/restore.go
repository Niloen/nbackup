// Package restore computes the ordered chain of archives needed to reconstruct a
// DLE as of a target slot. It is pure: it works over archive metadata and returns
// the steps; the engine performs the I/O and extraction.
package restore

import (
	"fmt"
	"sort"

	"github.com/Niloen/nbackup/internal/record"
)

// Step is one archive to extract during a restore. It identifies the archive
// logically; the engine resolves its volume position via the catalog.
type Step struct {
	SlotID   string
	DLE      string
	Level    int
	Archiver string // archiver type that produced the archive
	Compress string // compression scheme to reverse before extracting
	Encrypt  string // encryption scheme to reverse before decompressing ("" = plaintext)
}

// Chain returns the archives needed to restore a DLE as of the target slot, in
// run order: one archive per level along the real base chain — the most recent
// dump for the DLE at or before the target (the "tip"), then the base each
// incremental was built on (its recorded BaseSlot), walked back to the full.
//
// This is the per-level restore: a level-N dump is cumulative since the
// most recent level-(N-1) dump, so only the newest dump of each level is
// replayed and earlier same-level repeats are skipped. Replaying them is not
// merely redundant — GNU tar's directory directives (rename, delete) are not
// idempotent across independent incremental extractions, so a second cumulative
// L1 carrying the same rename aborts the chain. Following BaseSlot is also what
// keeps the chain consistent: each step's base is the exact dump it derives
// from, never an unrelated same-level dump. A missing base is a broken chain and
// is an error (a deliberate failure, not a partial restore). The input is the
// catalog's archives (each carrying its slot tag); a slot is just their grouping.
func Chain(archives []record.Archive, dleName, targetSlotID string) ([]Step, error) {
	targetExists := false
	for _, a := range archives {
		if a.Slot == targetSlotID {
			targetExists = true
			break
		}
	}
	if !targetExists {
		return nil, fmt.Errorf("slot %s not found in catalog", targetSlotID)
	}

	// The DLE's archives in run order (one per slot — a run dumps each DLE once).
	ds := archivesOf(archives, dleName)

	// The tip is the most recent dump of the DLE at or before the target.
	cur := -1
	for i := len(ds) - 1; i >= 0; i-- {
		if !record.SlotIDLess(targetSlotID, ds[i].Slot) { // ds[i].Slot <= target
			cur = i
			break
		}
	}
	if cur < 0 {
		return nil, fmt.Errorf("no backup found for DLE %q at or before %s", dleName, targetSlotID)
	}

	// Walk back along the base chain, newest level first.
	var steps []Step
	for {
		a := ds[cur]
		steps = append(steps, Step{SlotID: a.Slot, DLE: a.DLE, Level: a.Level, Archiver: a.Archiver, Compress: a.Compress, Encrypt: a.Encrypt})
		if a.Level == 0 {
			break
		}
		baseIdx, err := baseIndex(ds, cur, dleName)
		if err != nil {
			return nil, err
		}
		cur = baseIdx
	}

	// Reverse into run order: the full first, then each level up to the tip.
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	return steps, nil
}

// baseIndex locates the archive that an incremental builds on, within ds (the DLE's archives
// in run order). It honors the recorded BaseSlot strictly — if it names a slot that no longer
// holds a backup for the DLE (pruned away), that is a broken chain and an error, never a
// silent substitution. When BaseSlot was not recorded it derives the base as the most recent
// dump one level down before curIdx (what the planner would have recorded). A missing base
// either way is an error, not a partial restore.
func baseIndex(ds []record.Archive, curIdx int, dleName string) (int, error) {
	a := ds[curIdx]
	if a.BaseSlot != "" {
		for i := curIdx - 1; i >= 0; i-- {
			if ds[i].Slot == a.BaseSlot {
				return i, nil
			}
		}
		return 0, fmt.Errorf("broken incremental chain for DLE %q: slot %s (level %d) builds on slot %q, which holds no backup for it", dleName, a.Slot, a.Level, a.BaseSlot)
	}
	for i := curIdx - 1; i >= 0; i-- {
		if ds[i].Level == a.Level-1 {
			return i, nil
		}
	}
	return 0, fmt.Errorf("broken incremental chain for DLE %q: slot %s (level %d) has no level-%d base at or before it", dleName, a.Slot, a.Level, a.Level-1)
}

// archivesOf returns the archives of dle, in run order (by slot id).
func archivesOf(archives []record.Archive, dleName string) []record.Archive {
	var out []record.Archive
	for _, a := range archives {
		if a.DLE == dleName {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return record.SlotIDLess(out[i].Slot, out[j].Slot) })
	return out
}
