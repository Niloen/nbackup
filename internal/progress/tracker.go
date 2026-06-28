package progress

import (
	"sync"
	"time"
)

// Plan is the minimal description of one planned dump the Tracker needs to seed a
// DLE row — supplied by the engine from its planner.Item, without the progress
// package depending on the planner.
type Plan struct {
	Name     string
	Level    int
	EstBytes int64
}

// Sink receives a snapshot whenever the run state changes. force is true for
// state transitions (start/finish/phase) that must be persisted immediately, and
// false for byte-count updates a sink may throttle. The Tracker calls the sink
// while holding its lock, so calls are serialized and the snapshot is stable.
type Sink func(s Snapshot, force bool)

// Tracker maintains the single live Snapshot of a run and pushes it to a sink as
// workers report progress. Its methods are safe for concurrent use by parallel
// workers.
type Tracker struct {
	now  func() time.Time
	sink Sink

	mu   sync.Mutex
	idx  map[string]int // DLE name -> position in snap.DLEs
	snap Snapshot
}

// NewTracker seeds a snapshot in phase from the plan and pushes the initial state
// to the sink. phase is the run's starting stage (PhaseEstimating for the sizing
// prelude, PhaseRunning once workers archive). now supplies timestamps (injectable
// for tests); sink may be nil.
func NewTracker(slotID string, phase Phase, workers int, plan []Plan, now func() time.Time, sink Sink) *Tracker {
	if now == nil {
		now = time.Now
	}
	start := now()
	dles := make([]DLE, len(plan))
	idx := make(map[string]int, len(plan))
	for i, p := range plan {
		dles[i] = DLE{Name: p.Name, Level: p.Level, State: StatePending, EstBytes: p.EstBytes}
		idx[p.Name] = i
	}
	byName(dles)
	for i, d := range dles { // reindex after sort
		idx[d.Name] = i
	}
	t := &Tracker{
		now:  now,
		sink: sink,
		idx:  idx,
		snap: Snapshot{
			SlotID:    slotID,
			Phase:     phase,
			Workers:   workers,
			StartedAt: start,
			UpdatedAt: start,
			DLEs:      dles,
		},
	}
	t.flush(true)
	return t
}

// StartDLE marks a DLE as actively dumping.
func (t *Tracker) StartDLE(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.State = StateDumping
		d.StartedAt = t.now()
	}
	t.flush(true)
}

// AddBytes records cumulative progress for a DLE: uncompressed bytes archived and
// compressed bytes written so far. Throttle-eligible (force=false).
func (t *Tracker) AddBytes(name string, uncompressed, compressed int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.DoneBytes = uncompressed
		d.OutBytes = compressed
	}
	t.flush(false)
}

// FinishDLE marks a DLE done (or failed when err != nil) with its final tallies.
func (t *Tracker) FinishDLE(name string, fileCount int, uncompressed, compressed int64, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.EndedAt = t.now()
		d.FileCount = fileCount
		if uncompressed > 0 {
			d.DoneBytes = uncompressed
		}
		if compressed > 0 {
			d.OutBytes = compressed
		}
		if err != nil {
			d.State = StateFailed
			d.Err = err.Error()
		} else {
			d.State = StateDone
			if uncompressed == 0 {
				d.DoneBytes = d.EstBytes // no measured bytes: settle the bar to 100%
			}
		}
	}
	t.flush(true)
}

// StartFlush marks a DLE as draining from the holding disk it landed on (holding) to the landing
// (the second phase after its dump committed). Recording the disk lets a multi-disk run show where
// each DLE buffered. A no-op for an unknown DLE.
func (t *Tracker) StartFlush(name, holding string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.State = StateFlushing
		d.Holding = holding
	}
	t.flush(true)
}

// FinishFlush marks a DLE done once it has landed and been reclaimed from the holding disk.
// A no-op for an unknown DLE.
func (t *Tracker) FinishFlush(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.State = StateDone
		d.EndedAt = t.now()
	}
	t.flush(true)
}

// SetPhase advances the run's overall phase; terminal phases stamp EndedAt.
func (t *Tracker) SetPhase(p Phase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.snap.Phase = p
	if p.Terminal() {
		t.snap.EndedAt = t.now()
	}
	t.flush(true)
}

// Snapshot returns a copy of the current state.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.copy()
}

// dle returns a pointer to the named DLE row, or nil. Caller holds the lock.
func (t *Tracker) dle(name string) *DLE {
	if i, ok := t.idx[name]; ok {
		return &t.snap.DLEs[i]
	}
	return nil
}

// flush stamps UpdatedAt and pushes a snapshot copy to the sink. Caller holds the
// lock; the sink is invoked under it so updates are serialized and ordered.
func (t *Tracker) flush(force bool) {
	t.snap.UpdatedAt = t.now()
	if t.sink != nil {
		t.sink(t.copy(), force)
	}
}

// copy deep-copies the snapshot so callers and sinks never share the live slice.
func (t *Tracker) copy() Snapshot {
	s := t.snap
	s.DLEs = append([]DLE(nil), t.snap.DLEs...)
	return s
}
