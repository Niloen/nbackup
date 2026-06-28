// Package progress is NBackup's run-monitoring layer. A run (`nb dump`) drives a
// Tracker through the whole cycle — first sizing every DLE (the estimate phase),
// then archiving each one — and the Tracker maintains a single live Snapshot and
// flushes it to a status file. A separate command (`nb status`) loads and renders
// that file — so an operator can watch a detached run from another shell without
// any daemon or socket, only an inspectable file (the same philosophy as the
// catalog: state lives in files).
//
// A default DLE streams source -> compressor -> volume in one pass: one "dumping"
// state, metered by uncompressed bytes against the planner estimate. A holding-disk
// run adds a second phase per DLE — once its dump commits to a holding disk, the
// drainer copies it to the landing — surfaced as the "flushing" state (which holding
// disk it drained from is recorded too, so a multi-disk run shows where each landed).
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
	PhaseEstimating Phase = "estimating" // sizing every DLE before any dumping starts
	PhaseRunning    Phase = "running"    // workers are archiving DLEs
	PhaseSealing    Phase = "sealing"    // all dumps done; verifying + writing the seal
	PhaseDone       Phase = "done"       // sealed successfully (terminal)
	PhaseFailed     Phase = "failed"     // a dump or the seal failed (terminal)
)

// Terminal reports whether the run has finished (succeeded or failed). The
// estimate phase is non-terminal: it is the prelude to the dump, not its end.
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseFailed }

// State is one DLE's progress within the run.
type State string

const (
	StatePending  State = "pending"  // planned, not started
	StateDumping  State = "dumping"  // currently archiving (to the landing, or a holding disk)
	StateFlushing State = "flushing" // committed to a holding disk; the drainer is copying it to the landing
	StateDone     State = "done"     // archived successfully (and drained, in holding-disk mode)
	StateFailed   State = "failed"   // archiving failed
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
	Holding   string    `json:"holding,omitempty"` // holding disk it buffered on, set when draining begins (empty for a direct dump)
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
	Workers   int       `json:"workers"` // configured parallelism
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
		case StateDumping, StateFlushing:
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
