// Package state persists planner history and the GNU tar snapshot library in
// the catalog. It is operational state, analogous to Amanda's gnutar-lists: the
// snapshot (.snar) files let incremental backups find their base level, and the
// run history lets the planner choose levels. Sealed slots remain
// self-describing without it.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// FileName is the state file stored at the catalog root.
	FileName = "state.json"
	// DirSnapshots holds per-DLE, per-level tar snapshot files.
	DirSnapshots = "snapshots"
)

// State is the catalog-wide planner state.
type State struct {
	DLEs map[string]*DLEState `json:"dles"`
}

// DLEState tracks one DLE's backup history.
type DLEState struct {
	LastFullDate string      `json:"last_full_date"` // YYYY-MM-DD, empty if never
	LastFullSlot string      `json:"last_full_slot"`
	Runs         []RunRecord `json:"runs"`
}

// RunRecord is a historical backup of a DLE.
type RunRecord struct {
	Date  string `json:"date"`
	Slot  string `json:"slot"`
	Level int    `json:"level"`
}

// Load reads catalog state, returning an empty state if none exists.
func Load(catalog string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(catalog, FileName))
	if err != nil {
		if os.IsNotExist(err) {
			return &State{DLEs: map[string]*DLEState{}}, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.DLEs == nil {
		s.DLEs = map[string]*DLEState{}
	}
	return &s, nil
}

// Save writes catalog state atomically.
func (s *State) Save(catalog string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := filepath.Join(catalog, FileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(catalog, FileName))
}

// DLE returns the state for a DLE, creating it if absent.
func (s *State) DLE(name string) *DLEState {
	d, ok := s.DLEs[name]
	if !ok {
		d = &DLEState{}
		s.DLEs[name] = d
	}
	return d
}

// DaysSinceFull returns whole days since the last full for the DLE relative to
// today, or -1 if there has never been a full.
func (d *DLEState) DaysSinceFull(today time.Time) int {
	if d.LastFullDate == "" {
		return -1
	}
	last, err := time.Parse("2006-01-02", d.LastFullDate)
	if err != nil {
		return -1
	}
	return int(today.Sub(last).Hours() / 24)
}

// IncrementalsSinceFull counts runs recorded after the most recent full.
func (d *DLEState) IncrementalsSinceFull() int {
	n := 0
	for i := len(d.Runs) - 1; i >= 0; i-- {
		if d.Runs[i].Level == 0 {
			break
		}
		n++
	}
	return n
}

// LastLevel returns the level of the most recent recorded run, or -1 if none.
func (d *DLEState) LastLevel() int {
	if len(d.Runs) == 0 {
		return -1
	}
	return d.Runs[len(d.Runs)-1].Level
}

// SnapshotPath is the canonical location of a DLE's snapshot for a given level.
func SnapshotPath(catalog, dle string, level int) string {
	return filepath.Join(catalog, DirSnapshots, dle, fmt.Sprintf("L%d.snar", level))
}
