// Package restore reconstructs a DLE's data from a slot by applying its full
// backup followed by every later incremental in the chain, in run order. GNU
// tar's incremental extraction applies deletions recorded in each archive.
package restore

import (
	"fmt"
	"path/filepath"

	"github.com/Niloen/nbackup/internal/archive"
	"github.com/Niloen/nbackup/internal/slot"
)

// Step is one archive to extract during a restore.
type Step struct {
	SlotID string
	Level  int
	File   string // path relative to the slot directory
	Dir    string // absolute slot directory
}

// Chain computes the ordered list of archives needed to restore a DLE as of the
// target slot: the most recent full at or before the target (in run order),
// plus every later backup for that DLE up to the target (inclusive).
func Chain(catalog, dleName, targetSlotID string) ([]Step, error) {
	slots, err := slot.List(catalog)
	if err != nil {
		return nil, err
	}
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

	// Most recent full for this DLE at or before the target, in run order.
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
			steps = append(steps, Step{
				SlotID: s.ID,
				Level:  a.Level,
				File:   a.File,
				Dir:    filepath.Join(catalog, s.ID),
			})
		}
	}
	return steps, nil
}

// Run restores dleName as of targetSlotID into destDir using the given GNU tar
// binary.
func Run(tarBin, catalog, dleName, targetSlotID, destDir string, logf func(string, ...any)) error {
	steps, err := Chain(catalog, dleName, targetSlotID)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if logf != nil {
			logf("extracting %s L%d -> %s", step.SlotID, step.Level, destDir)
		}
		if err := archive.Extract(tarBin, filepath.Join(step.Dir, step.File), destDir); err != nil {
			return fmt.Errorf("extract %s: %w", step.File, err)
		}
	}
	return nil
}
