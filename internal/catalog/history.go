package catalog

import "time"

// History is a per-DLE view of backup runs, derived from the slot index. The
// planner consumes it to choose levels. It is not persisted on its own — it is
// always computed from the slots (the source of truth).
type History struct {
	DLEs map[string]*DLEState `json:"dles"`
}

// DLEState tracks one DLE's backup history. Sizes are not stored: estimates are
// computed fresh from the dump method each run (Amanda's client estimate).
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

// DLE returns the state for a DLE, creating it if absent.
func (h *History) DLE(name string) *DLEState {
	d, ok := h.DLEs[name]
	if !ok {
		d = &DLEState{}
		h.DLEs[name] = d
	}
	return d
}

// RecordRun advances a DLE's state as if a run of the given level had been sealed:
// it appends the run and, for a full (level 0), moves the last-full markers. This
// is the single in-memory rule for how a sealed run changes history — the catalog's
// derivation from slots and the planner's forward simulation both go through it, so
// a simulated day advances exactly as a real one would.
func (h *History) RecordRun(dle, slotID, date string, level int) {
	d := h.DLE(dle)
	d.Runs = append(d.Runs, RunRecord{Date: date, Slot: slotID, Level: level})
	if level == 0 {
		d.LastFullDate = date
		d.LastFullSlot = slotID
	}
}

// Clone returns a deep copy whose DLEStates and run slices are independent of the
// original, so a caller can advance it (RecordRun) without mutating the
// catalog-derived history. Used by the planner's forward simulation.
func (h *History) Clone() *History {
	out := &History{DLEs: make(map[string]*DLEState, len(h.DLEs))}
	for name, d := range h.DLEs {
		cp := *d
		cp.Runs = append([]RunRecord(nil), d.Runs...)
		out.DLEs[name] = &cp
	}
	return out
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

// LastSlot returns the slot ID of the most recent recorded run, or "" if none.
func (d *DLEState) LastSlot() string {
	if len(d.Runs) == 0 {
		return ""
	}
	return d.Runs[len(d.Runs)-1].Slot
}
