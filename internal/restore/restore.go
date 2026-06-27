// Package restore computes the ordered chain of archives needed to reconstruct a
// DLE as of a target slot. It is pure: it works over slot metadata and returns
// the steps; the engine performs the I/O and extraction.
package restore

import (
	"fmt"

	"github.com/Niloen/nbackup/internal/record"
)

// Step is one archive to extract during a restore. It identifies the archive
// logically; the engine resolves its volume position via the catalog.
type Step struct {
	SlotID   string
	DLE      string
	Level    int
	Archiver string // archiver type that produced the archive
	Compress string // compression codec to reverse before extracting
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
// is an error (a deliberate failure, not a partial restore). The input slots
// must be sorted in run order.
func Chain(slots []*record.Slot, dleName, targetSlotID string) ([]Step, error) {
	targetIdx := -1
	for i, s := range slots {
		if s.ID == targetSlotID {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return nil, fmt.Errorf("slot %s not found in catalog", targetSlotID)
	}

	// The tip is the most recent dump of the DLE at or before the target.
	curIdx := -1
	for i := targetIdx; i >= 0; i-- {
		if archiveFor(slots[i], dleName) != nil {
			curIdx = i
			break
		}
	}
	if curIdx < 0 {
		return nil, fmt.Errorf("no backup found for DLE %q at or before %s", dleName, targetSlotID)
	}

	// Walk back along the base chain, newest level first.
	var steps []Step
	for {
		s, a := slots[curIdx], archiveFor(slots[curIdx], dleName)
		steps = append(steps, Step{SlotID: s.ID, DLE: a.DLE, Level: a.Level, Archiver: a.Archiver, Compress: a.Compress, Encrypt: a.Encrypt})
		if a.Level == 0 {
			break
		}
		baseIdx, err := baseIndex(slots, curIdx, dleName, *a)
		if err != nil {
			return nil, err
		}
		curIdx = baseIdx
	}

	// Reverse into run order: the full first, then each level up to the tip.
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	return steps, nil
}

// baseIndex locates the archive that an incremental builds on. It honors the
// recorded BaseSlot strictly — if it names a slot that is no longer in the
// catalog (pruned away), that is a broken chain and an error, never a silent
// substitution. When BaseSlot was not recorded it derives the base as the most
// recent dump one level down before curIdx (what the planner would have
// recorded). A missing base either way is an error, not a partial restore.
func baseIndex(slots []*record.Slot, curIdx int, dleName string, a record.Archive) (int, error) {
	if a.BaseSlot != "" {
		for i := curIdx - 1; i >= 0; i-- {
			if slots[i].ID == a.BaseSlot {
				if archiveFor(slots[i], dleName) != nil {
					return i, nil
				}
				break
			}
		}
		return 0, fmt.Errorf("broken incremental chain for DLE %q: slot %s (level %d) builds on slot %q, which holds no backup for it", dleName, slots[curIdx].ID, a.Level, a.BaseSlot)
	}
	for i := curIdx - 1; i >= 0; i-- {
		if b := archiveFor(slots[i], dleName); b != nil && b.Level == a.Level-1 {
			return i, nil
		}
	}
	return 0, fmt.Errorf("broken incremental chain for DLE %q: slot %s (level %d) has no level-%d base at or before it", dleName, slots[curIdx].ID, a.Level, a.Level-1)
}

// archiveFor returns the slot's archive for the named DLE, or nil if absent.
func archiveFor(s *record.Slot, dleName string) *record.Archive {
	for i := range s.Archives {
		if s.Archives[i].DLE == dleName {
			return &s.Archives[i]
		}
	}
	return nil
}
