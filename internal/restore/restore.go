// Package restore reconstructs a DLE's data from a slot by applying its full
// backup followed by any later incrementals, in order.
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
// target slot: the most recent full at or before the target, plus every
// incremental for that DLE between the full and the target (inclusive).
func Chain(catalog, dleName, targetSlotID string) ([]Step, error) {
	slots, err := slot.List(catalog)
	if err != nil {
		return nil, err
	}
	var targetDate string
	for _, s := range slots {
		if s.ID == targetSlotID {
			targetDate = s.Date
		}
	}
	if targetDate == "" {
		return nil, fmt.Errorf("slot %s not found in catalog", targetSlotID)
	}

	// Find the index of the most recent full for this DLE at or before target.
	fullIdx := -1
	for i, s := range slots {
		if s.Date > targetDate {
			break
		}
		for _, a := range s.Archives {
			if a.DLE == dleName && a.Level == 0 {
				fullIdx = i
			}
		}
	}
	if fullIdx < 0 {
		return nil, fmt.Errorf("no full backup found for DLE %q at or before %s", dleName, targetSlotID)
	}

	var steps []Step
	for i := fullIdx; i < len(slots); i++ {
		s := slots[i]
		if s.Date > targetDate {
			break
		}
		for _, a := range s.Archives {
			if a.DLE != dleName {
				continue
			}
			// Take the full at fullIdx, and incrementals afterwards.
			if i == fullIdx && a.Level != 0 {
				continue
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

// Run restores dleName as of targetSlotID into destDir.
func Run(catalog, dleName, targetSlotID, destDir string, logf func(string, ...any)) error {
	steps, err := Chain(catalog, dleName, targetSlotID)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if logf != nil {
			logf("extracting %s L%d -> %s", step.SlotID, step.Level, destDir)
		}
		if err := archive.Extract(filepath.Join(step.Dir, step.File), destDir); err != nil {
			return fmt.Errorf("extract %s: %w", step.File, err)
		}
	}
	return nil
}
