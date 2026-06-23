// Package progress is NBackup's run-monitoring layer, analogous to Amanda's
// amdump status file + amstatus reader. A run (`nb dump`) drives a Tracker as its
// dumpers archive each DLE; the Tracker maintains a single live Snapshot and
// flushes it to a status file. A separate command (`nb status`) loads and renders
// that file — so an operator can watch a detached run from another shell without
// any daemon or socket, only an inspectable file (the same philosophy as the
// catalog: state lives in files).
//
// NBackup has no holding disk: each DLE streams source -> compressor -> volume in
// one pass, so unlike Amanda there is no separate dumper/taper split — one DLE has
// one "dumping" state, metered by uncompressed bytes against the planner estimate.
package progress

import (
	"sort"
	"time"
)

// StatusFileName is the run-status file the Tracker writes and `nb status` reads,
// relative to the catalog workdir.
const StatusFileName = "run-status.json"

// Phase is the run's overall lifecycle stage.
type Phase string

const (
	PhaseRunning Phase = "running" // dumpers are archiving DLEs
	PhaseSealing Phase = "sealing" // all dumps done; verifying + writing the seal
	PhaseDone    Phase = "done"    // sealed successfully (terminal)
	PhaseFailed  Phase = "failed"  // a dump or the seal failed (terminal)
)

// Terminal reports whether the run has finished (succeeded or failed).
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseFailed }

// State is one DLE's progress within the run.
type State string

const (
	StatePending State = "pending" // planned, not started
	StateDumping State = "dumping" // currently archiving
	StateDone    State = "done"    // archived successfully
	StateFailed  State = "failed"  // archiving failed
)

// DLE is the live progress of a single planned dump.
type DLE struct {
	Name      string    `json:"name"`
	Level     int       `json:"level"`
	State     State     `json:"state"`
	EstBytes  int64     `json:"est_bytes"`  // planner estimate (uncompressed)
	DoneBytes int64     `json:"done_bytes"` // uncompressed bytes archived so far
	OutBytes  int64     `json:"out_bytes"`  // compressed bytes written so far
	FileCount int       `json:"file_count"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Err       string    `json:"err,omitempty"`
}

// Pct is the DLE's completion against its estimate (0..100, capped). Returns 0
// when there is no estimate to measure against.
func (d DLE) Pct() float64 {
	if d.EstBytes <= 0 {
		return 0
	}
	p := float64(d.DoneBytes) / float64(d.EstBytes) * 100
	if p > 100 {
		return 100
	}
	return p
}

// Snapshot is the whole run's state at one instant — what gets persisted and
// rendered. It is a value type; the Tracker hands out copies.
type Snapshot struct {
	SlotID    string    `json:"slot_id"`
	Phase     Phase     `json:"phase"`
	Dumpers   int       `json:"dumpers"` // configured parallelism
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	DLEs      []DLE     `json:"dles"`
}

// TotalEst sums the planned estimates (uncompressed).
func (s Snapshot) TotalEst() int64 { return sum(s.DLEs, func(d DLE) int64 { return d.EstBytes }) }

// TotalDone sums uncompressed bytes archived so far.
func (s Snapshot) TotalDone() int64 { return sum(s.DLEs, func(d DLE) int64 { return d.DoneBytes }) }

// TotalOut sums compressed bytes written so far.
func (s Snapshot) TotalOut() int64 { return sum(s.DLEs, func(d DLE) int64 { return d.OutBytes }) }

// Counts tallies DLEs by state.
func (s Snapshot) Counts() (active, done, failed, pending int) {
	for _, d := range s.DLEs {
		switch d.State {
		case StateDumping:
			active++
		case StateDone:
			done++
		case StateFailed:
			failed++
		default:
			pending++
		}
	}
	return
}

// Elapsed is the wall time from start to the reference instant (UpdatedAt for a
// finished run, otherwise now).
func (s Snapshot) Elapsed(now time.Time) time.Duration {
	end := now
	if s.Phase.Terminal() && !s.EndedAt.IsZero() {
		end = s.EndedAt
	}
	if end.Before(s.StartedAt) {
		return 0
	}
	return end.Sub(s.StartedAt)
}

// Rate is the overall archived throughput in uncompressed bytes/sec (0 until any
// measurable time has passed).
func (s Snapshot) Rate(now time.Time) float64 {
	secs := s.Elapsed(now).Seconds()
	if secs <= 0 {
		return 0
	}
	return float64(s.TotalDone()) / secs
}

// Pct is the run's overall completion against the total estimate (0..100).
func (s Snapshot) Pct() float64 {
	est := s.TotalEst()
	if est <= 0 {
		return 0
	}
	p := float64(s.TotalDone()) / float64(est) * 100
	if p > 100 {
		return 100
	}
	return p
}

// ETA estimates remaining time from the current rate and the unfinished
// estimate. Returns ok=false while no rate is known or the run is terminal.
func (s Snapshot) ETA(now time.Time) (d time.Duration, ok bool) {
	if s.Phase.Terminal() {
		return 0, false
	}
	rate := s.Rate(now)
	if rate <= 0 {
		return 0, false
	}
	remaining := s.TotalEst() - s.TotalDone()
	if remaining <= 0 {
		return 0, false
	}
	return time.Duration(float64(remaining) / rate * float64(time.Second)), true
}

func sum(ds []DLE, f func(DLE) int64) int64 {
	var t int64
	for _, d := range ds {
		t += f(d)
	}
	return t
}

// byName orders DLEs deterministically for stable rendering and file output.
func byName(ds []DLE) { sort.Slice(ds, func(i, j int) bool { return ds[i].Name < ds[j].Name }) }
