package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// HistoryFile is the per-catalog history file (analogous to Amanda's curinfo).
const HistoryFile = "state.json"

// History records each DLE's backup runs, used by the planner to choose levels.
type History struct {
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

func loadHistory(workdir string) (*History, error) {
	data, err := os.ReadFile(filepath.Join(workdir, HistoryFile))
	if err != nil {
		if os.IsNotExist(err) {
			return &History{DLEs: map[string]*DLEState{}}, nil
		}
		return nil, err
	}
	var h History
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	if h.DLEs == nil {
		h.DLEs = map[string]*DLEState{}
	}
	return &h, nil
}

func (h *History) save(workdir string) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(workdir, HistoryFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(workdir, HistoryFile))
}

// DLE returns the state for a DLE, creating it if absent.
func (h *History) DLE(name string) *DLEState {
	d, ok := h.DLEs[name]
	if !ok {
		d = &DLEState{}
		h.DLEs[name] = d
	}
	return d
}

// DaysSinceFull returns whole days since the last full, or -1 if never.
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

// LastSlot returns the slot ID of the most recent recorded run, or "" if none.
func (d *DLEState) LastSlot() string {
	if len(d.Runs) == 0 {
		return ""
	}
	return d.Runs[len(d.Runs)-1].Slot
}
