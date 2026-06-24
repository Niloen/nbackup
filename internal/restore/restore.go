// Package restore computes the ordered chain of archives needed to reconstruct a
// DLE as of a target slot. It is pure: it works over slot metadata and returns
// the steps; the engine performs the I/O and extraction.
package restore

import (
	"fmt"

	"github.com/Niloen/nbackup/internal/format"
)

// Step is one archive to extract during a restore. It identifies the archive
// logically; the engine resolves its volume position via the catalog.
type Step struct {
	SlotID   string
	DLE      string
	Level    int
	Archiver string // archiver type that produced the archive
	Codec    string // compression codec to reverse before extracting
	Encrypt  string // encryption scheme to reverse before decompressing ("" = plaintext)
}

// Chain returns the archives needed to restore a DLE as of the target slot, in
// run order: the most recent full at or before the target, plus every later
// backup for that DLE up to the target (inclusive). The input slots must be
// sorted in run order.
func Chain(slots []*format.Slot, dleName, targetSlotID string) ([]Step, error) {
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

	fullIdx := -1
	for i := 0; i <= targetIdx; i++ {
		for _, a := range slots[i].Archives {
			if a.DLE == dleName && a.Level == 0 {
				fullIdx = i
			}
		}
	}
	if fullIdx < 0 {
		return nil, fmt.Errorf("no full backup found for DLE %q at or before %s", dleName, targetSlotID)
	}

	var steps []Step
	for i := fullIdx; i <= targetIdx; i++ {
		s := slots[i]
		for _, a := range s.Archives {
			if a.DLE != dleName {
				continue
			}
			if i == fullIdx && a.Level != 0 {
				continue // at the full's slot, take only the full
			}
			steps = append(steps, Step{SlotID: s.ID, DLE: a.DLE, Level: a.Level, Archiver: a.Archiver, Codec: a.Codec, Encrypt: a.Encrypt})
		}
	}
	return steps, nil
}
