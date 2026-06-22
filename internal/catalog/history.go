package catalog

import "time"

// History is a per-DLE view of backup runs, derived from the slot index. The
// planner consumes it to choose levels. It is not persisted on its own — it is
// always computed from the slots (the source of truth).
type History struct {
	DLEs map[string]*DLEState `json:"dles"`
}

// DLEState tracks one DLE's backup history.
type DLEState struct {
	LastFullDate  string      `json:"last_full_date"` // YYYY-MM-DD, empty if never
	LastFullSlot  string      `json:"last_full_slot"`
	LastFullBytes int64       `json:"last_full_bytes"` // compressed size of the last full (for planner balancing)
	Runs          []RunRecord `json:"runs"`
}

// RunRecord is a historical backup of a DLE.
type RunRecord struct {
	Date  string `json:"date"`
	Slot  string `json:"slot"`
	Level int    `json:"level"`
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
