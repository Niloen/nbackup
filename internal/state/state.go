// Package state persists planner history and incremental base snapshots in the
// catalog. It is an operational cache: slots remain fully self-describing
// without it, but it lets the planner pick levels and incrementals find a base.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/Niloen/nbackup/internal/archive"
)

// FileName is the state file stored at the catalog root.
const FileName = "state.json"

// State is the catalog-wide planner state.
type State struct {
	DLEs map[string]*DLEState `json:"dles"`
}

// DLEState tracks one DLE's backup history.
type DLEState struct {
	LastFullDate string           `json:"last_full_date"` // YYYY-MM-DD, empty if never
	LastFullSlot string           `json:"last_full_slot"`
	BaseSnapshot archive.Snapshot `json:"base_snapshot"` // snapshot captured at the last full
	Runs         []RunRecord      `json:"runs"`
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
