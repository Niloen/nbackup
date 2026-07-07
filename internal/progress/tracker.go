package progress

import (
	"strings"
	"sync"
	"time"
)

// Plan is the minimal description of one planned dump the Tracker needs to seed a
// DLE row — supplied by the engine from its planner.Item, without the progress
// package depending on the planner.
type Plan struct {
	Name     string
	Slug     string // internal filesystem-safe DLE id (DLE.Name()), for catalog/URL links
	Level    int
	EstBytes int64
	Landings []string // the DLE's landing route, primary first; empty = a single unnamed landing
}

// Sink receives a snapshot whenever the run state changes. force is true for
// state transitions (start/finish/phase) that must be persisted immediately, and
// false for byte-count updates a sink may throttle. The Tracker calls the sink
// while holding its lock, so calls are serialized and the snapshot is stable.
type Sink func(s Snapshot, force bool)

// Tracker maintains the single live Snapshot of a run and pushes it to a sink as
// workers report progress. Its methods are safe for concurrent use by parallel
// workers. A nil *Tracker is a no-op sink: every reporting method returns early,
// so a caller that may run without progress tracking needs no guard.
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
func NewTracker(runID string, phase Phase, workers int, plan []Plan, now func() time.Time, sink Sink) *Tracker {
	if now == nil {
		now = time.Now
	}
	start := now()
	dles := make([]DLE, len(plan))
	for i, p := range plan {
		dles[i] = DLE{Name: p.Name, Slug: p.Slug, Level: p.Level, State: StatePending, EstBytes: p.EstBytes, Landings: p.Landings}
	}
	byName(dles)
	idx := make(map[string]int, len(dles))
	for i, d := range dles {
		idx[d.Name] = i
	}
	t := &Tracker{
		now:  now,
		sink: sink,
		idx:  idx,
		snap: Snapshot{
			RunID:     runID,
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
	if t == nil {
		return
	}
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
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.DoneBytes = uncompressed
		d.OutBytes = compressed
	}
	t.flush(false)
}

// AddDrainBytes records cumulative compressed bytes copied to one landing for a DLE the
// drainer is flushing off a holding disk — per landing, so a fan-out's copies meter
// independently; DrainBytes stays their sum. Throttle-eligible (force=false), mirroring AddBytes.
func (t *Tracker) AddDrainBytes(name, landing string, copied int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		if d.Drained == nil {
			d.Drained = map[string]int64{}
		}
		d.Drained[landing] = copied
		var total int64
		for _, n := range d.Drained {
			total += n
		}
		d.DrainBytes = total
	}
	t.flush(false)
}

// FinishDLE marks a DLE done (or failed when err != nil) with its final tallies.
func (t *Tracker) FinishDLE(name string, fileCount int, uncompressed, compressed int64, err error) {
	if t == nil {
		return
	}
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
	t.markDumpEndIfDone()
	t.flush(true)
}

// CancelDLE marks a DLE interrupted in flight by a canceled run — distinct from a failure,
// so status shows it as canceled rather than a scary error. A no-op for an unknown DLE.
func (t *Tracker) CancelDLE(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.State = StateCanceled
		d.EndedAt = t.now()
	}
	t.markDumpEndIfDone()
	t.flush(true)
}

// MarkToHolding records that a DLE's dump is routed to a holding disk — set the moment ingestion is
// acquired there, before any bytes commit. It marks the staging window: live status then shows the
// DLE staging to holding (and not yet on the volume) instead of mistaking its in-flight dump for a
// direct write to the landing. A no-op for an unknown DLE.
func (t *Tracker) MarkToHolding(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.ToHolding = true
	}
	t.flush(true)
}

// StageHolding records that a DLE's dump committed to holding disk `holding`, the moment it lands
// there — before it is its turn to drain. This is what distinguishes a buffered DLE queued behind
// another's drain from a direct dump: until the drainer reaches it, only this mark tells the two
// apart (without it, a queued holding DLE renders as "direct"). A no-op for an unknown DLE.
func (t *Tracker) StageHolding(name, holding string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.Holding = holding
	}
	t.flush(true)
}

// LandVolume records which landing volume(s) a DLE's archive committed to — the volume's label (or
// several, comma-joined, when the archive spanned volumes, drives, or landings). This
// is what surfaces the multi-drive spread in `nb status`: each DLE shows the volume its data reached.
// Reports merge: a fan-out lands on several media, each reporting its own label(s), and all distinct
// labels are kept. A no-op for an unknown DLE or an empty volume id (an address-identified landing
// carries no label).
func (t *Tracker) LandVolume(name, volume string) {
	if t == nil || volume == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.Volume = mergeLabels(d.Volume, volume)
	}
	t.flush(true)
}

// mergeLabels folds a comma-joined label report into an existing one, keeping every
// distinct label once, in first-reported order.
func mergeLabels(have, add string) string {
	if have == "" {
		return add
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(have, ",") {
		seen[l] = true
	}
	for _, l := range strings.Split(add, ",") {
		if !seen[l] {
			have += "," + l
			seen[l] = true
		}
	}
	return have
}

// StartFlush marks a DLE as draining from the holding disk it landed on (holding) to the landing
// (the second phase after its dump committed). Recording the disk lets a multi-disk run show where
// each DLE buffered. A no-op for an unknown DLE.
func (t *Tracker) StartFlush(name, holding string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.State = StateFlushing
		d.Holding = holding
	}
	if t.snap.DrainStartedAt.IsZero() {
		t.snap.DrainStartedAt = t.now()
	}
	t.markDumpEndIfDone() // a DLE entering its drain has finished dumping
	t.flush(true)
}

// FinishFlush marks a DLE done once it has landed and been reclaimed from the holding disk.
// A no-op for an unknown DLE.
func (t *Tracker) FinishFlush(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.dle(name); d != nil {
		d.State = StateDone
		d.EndedAt = t.now()
		if d.OutBytes > 0 {
			d.DrainBytes = d.toDrain() // settle the drain bar to 100%
			for _, l := range d.Landings {
				if d.Drained == nil {
					d.Drained = map[string]int64{}
				}
				d.Drained[l] = d.OutBytes
			}
		}
	}
	t.flush(true)
}

// markDumpEndIfDone stamps DumpEndedAt the first time no DLE is still pending or dumping —
// the instant the dumping pipeline finished, which freezes the dump rate so it stops
// decaying while the drainer flushes a remaining tail. Caller holds the lock.
func (t *Tracker) markDumpEndIfDone() {
	if !t.snap.DumpEndedAt.IsZero() {
		return
	}
	for _, d := range t.snap.DLEs {
		if d.State == StatePending || d.State == StateDumping {
			return
		}
	}
	t.snap.DumpEndedAt = t.now()
}

// Fail marks the run failed with a run-level reason — a failure that belongs to
// the run as a whole (a preflight or make-room refusal, a failed drain) rather
// than to any one DLE: SetPhase(PhaseFailed) plus recording why.
func (t *Tracker) Fail(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.snap.Err = err.Error()
	}
	t.snap.Phase = PhaseFailed
	t.snap.EndedAt = t.now()
	t.flush(true)
}

// SetPhase advances the run's overall phase; terminal phases stamp EndedAt.
func (t *Tracker) SetPhase(p Phase) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.snap.Phase = p
	if p.Terminal() {
		t.snap.EndedAt = t.now()
	}
	t.flush(true)
}

// Snapshot returns a copy of the current state (the zero Snapshot on a nil Tracker).
func (t *Tracker) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{}
	}
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

// copy deep-copies the snapshot so callers and sinks never share the live slice
// (or a DLE's live per-landing drain map).
func (t *Tracker) copy() Snapshot {
	s := t.snap
	s.DLEs = append([]DLE(nil), t.snap.DLEs...)
	for i, d := range s.DLEs {
		if d.Drained != nil {
			m := make(map[string]int64, len(d.Drained))
			for k, v := range d.Drained {
				m[k] = v
			}
			s.DLEs[i].Drained = m
		}
	}
	return s
}
