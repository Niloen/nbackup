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

// EstimateRunID is the placeholder run id the sizing prelude reports under — the
// real id is minted only after the estimate, when the run is created.
const EstimateRunID = "estimate"

// Phase is the run's overall lifecycle stage.
type Phase string

const (
	PhaseEstimating Phase = "estimating" // sizing every DLE before any dumping starts
	PhaseRunning    Phase = "running"    // workers are archiving DLEs
	PhaseSealing    Phase = "sealing"    // all dumps done; verifying + writing the seal
	PhaseDone       Phase = "done"       // sealed successfully (terminal)
	PhaseFailed     Phase = "failed"     // a dump or the seal failed (terminal)
	PhaseCanceled   Phase = "canceled"   // the operator interrupted the run (terminal)
)

// Terminal reports whether the run has finished (succeeded, failed, or canceled). The
// estimate phase is non-terminal: it is the prelude to the dump, not its end.
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseFailed || p == PhaseCanceled }

// State is one DLE's progress within the run.
type State string

const (
	StatePending  State = "pending"  // planned, not started
	StateDumping  State = "dumping"  // currently archiving (to the landing, or a holding disk)
	StateFlushing State = "flushing" // committed to a holding disk; the drainer is copying it to the landing
	StateDone     State = "done"     // archived successfully (and drained, in holding-disk mode)
	StateFailed   State = "failed"   // archiving failed
	StateCanceled State = "canceled" // interrupted in flight by a canceled run
)

// DLE is the live progress of a single planned dump.
type DLE struct {
	Name      string   `json:"name"`
	Slug      string   `json:"slug,omitempty"` // internal filesystem-safe DLE id (DLE.Name()), for catalog/URL links
	Level     int      `json:"level"`
	State     State    `json:"state"`
	EstBytes  int64    `json:"est_bytes"`          // planner estimate (uncompressed)
	DoneBytes int64    `json:"done_bytes"`         // uncompressed bytes archived so far
	OutBytes  int64    `json:"out_bytes"`          // compressed bytes produced so far (the size staged on the holding disk)
	Landings  []string `json:"landings,omitempty"` // the DLE's landing route, primary first; its drain copies the staged bytes to every entry
	// DrainBytes sums compressed bytes copied off the holding disk so far across the whole
	// route (a two-landing DLE drains twice, so its total-to-drain is OutBytes per landing);
	// Drained is the per-landing share, keyed by landing name ("" for a report that named none).
	DrainBytes int64            `json:"drain_bytes"`
	Drained    map[string]int64 `json:"drained,omitempty"`
	FileCount  int              `json:"file_count"`
	Holding    string           `json:"holding,omitempty"`    // holding disk it buffered on, set the moment its dump commits there (empty for a direct dump)
	ToHolding  bool             `json:"to_holding,omitempty"` // dump is routed to a holding disk, set when ingestion is acquired there — before it commits; marks the staging window until Holding is set
	Volume     string           `json:"volume,omitempty"`     // the landing volume(s) its archive committed to — the volume's label (or several, comma-joined, when it spanned volumes, drives, or landings); empty for an address-identified landing that carries no label
	StartedAt  time.Time        `json:"started_at,omitempty"`
	EndedAt    time.Time        `json:"ended_at,omitempty"`
	Err        string           `json:"err,omitempty"`
}

// Pct is the DLE's dump completion against its estimate (0..100, capped). Returns 0
// when there is no estimate to measure against.
func (d DLE) Pct() float64 { return pct(d.DoneBytes, d.EstBytes) }

// lanes is how many landings the DLE's drain must reach — the multiplier on its
// staged size for every to-drain total. 1 when no route was recorded (a legacy
// single-landing report).
func (d DLE) lanes() int64 {
	if len(d.Landings) > 1 {
		return int64(len(d.Landings))
	}
	return 1
}

// toDrain is the compressed total the DLE's drain must copy: its staged size, once
// per landing on its route.
func (d DLE) toDrain() int64 { return d.OutBytes * d.lanes() }

// DrainTotal is the compressed total the DLE's drain must copy across its whole route —
// its staged size, once per landing — so callers displaying DrainBytes alongside a total
// (e.g. "X of Y") use this instead of OutBytes, which is only one landing's share.
func (d DLE) DrainTotal() int64 { return d.toDrain() }

// DrainPct is a holding-disk DLE's drain completion: bytes copied off the holding disk
// against the total its route needs — staged size × landings — so 100% means every
// landing holds the archive. 0 when nothing is staged.
func (d DLE) DrainPct() float64 { return pct(d.DrainBytes, d.toDrain()) }

// Drains reports whether the DLE goes through a holding disk, so it has a drain phase.
// Holding is set the moment its dump commits to the holding disk and persists through done,
// so a DLE staged but still queued behind another's drain already reads as draining; a direct
// dump (no holding disk, or an oversized DLE streamed straight to the landing) leaves it empty.
func (d DLE) Drains() bool { return d.Holding != "" }

// OnVolume is the bytes that have landed on the authoritative (primary) volume: 0 while a
// holding-bound DLE is still staging to its disk (its bytes are on holding, not the volume), the
// amount copied so far once it drains — its primary landing's share, so a fan-out never
// double-counts — and for a direct dump the compressed bytes it wrote straight to the volume.
func (d DLE) OnVolume() int64 {
	if d.Drains() || d.ToHolding {
		if len(d.Landings) > 1 {
			return d.Drained[d.Landings[0]]
		}
		return d.DrainBytes
	}
	return d.OutBytes
}

// pct is done/total as a capped 0..100 percentage (0 when there is nothing to measure).
func pct(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	if p := float64(done) / float64(total) * 100; p < 100 {
		return p
	}
	return 100
}

// Snapshot is the whole run's state at one instant — what gets persisted and
// rendered. It is a value type; the Tracker hands out copies.
type Snapshot struct {
	RunID     string    `json:"run_id"`
	Phase     Phase     `json:"phase"`
	Workers   int       `json:"workers"` // configured parallelism
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	// DumpEndedAt freezes the moment the last dump finished, and DrainStartedAt the
	// moment the first drain began — so the two pipelines' rates are each measured over
	// their own window instead of the run's whole wall-clock (dumping and draining run
	// concurrently, and the dump rate must not decay while a tail of drains finishes).
	DumpEndedAt    time.Time `json:"dump_ended_at,omitempty"`
	DrainStartedAt time.Time `json:"drain_started_at,omitempty"`
	// Skipped names the landings this run declared unusable up front (a medium that
	// failed to open, or one that could not make room) — each removed from every DLE's
	// route, so no drain is owed there and nothing reads as drained to it. The archives
	// are MISSING on a skipped landing; the repair is `nb sync --to <landing>`.
	Skipped []SkippedLanding `json:"skipped_landings,omitempty"`
	// Err is the run-level failure reason — a failure that belongs to the run as a
	// whole (a preflight or make-room refusal before any dump started, a failed
	// drain) rather than to any one DLE (those live in DLE.Err).
	Err  string `json:"err,omitempty"`
	DLEs []DLE  `json:"dles"`
}

// SkippedLanding is one landing a run could not serve, and why — the archives it
// was routed are missing there until an `nb sync --to <landing>` backfills them.
// Tripped distinguishes a landing that failed MID-RUN (its first failed write killed
// the lane; copies drained before the failure exist, the rest are missing — the
// landing stays on every DLE's route so the drain totals honestly show the shortfall)
// from one skipped up front (removed from the routes; nothing was ever owed there).
type SkippedLanding struct {
	Landing string `json:"landing"`
	Reason  string `json:"reason"`
	Tripped bool   `json:"tripped,omitempty"`
}

// TotalEst sums the planned estimates (uncompressed).
func (s Snapshot) TotalEst() int64 { return sum(s.DLEs, func(d DLE) int64 { return d.EstBytes }) }

// TotalDone sums uncompressed bytes archived so far.
func (s Snapshot) TotalDone() int64 { return sum(s.DLEs, func(d DLE) int64 { return d.DoneBytes }) }

// TotalToDrain sums the compressed size every drained DLE must copy to its landing(s)
// — staged size × route length per DLE, so a fan-out counts each copy it owes.
func (s Snapshot) TotalToDrain() int64 {
	return sum(s.DLEs, func(d DLE) int64 {
		if d.Drains() {
			return d.toDrain()
		}
		return 0
	})
}

// TotalDrained sums compressed bytes copied to the landings so far (drained DLEs).
func (s Snapshot) TotalDrained() int64 {
	return sum(s.DLEs, func(d DLE) int64 {
		if d.Drains() {
			return d.DrainBytes
		}
		return 0
	})
}

// LandingDrain is one landing's flush backlog across the run: bytes copied to it so
// far against the staged bytes routed to it.
type LandingDrain struct {
	Landing string
	Done    int64
	Total   int64
}

// LandingDrains splits the run's flush totals per landing, in first-seen route order
// (primaries lead since routes list them first). One entry — the classic single-landing
// run — means the aggregate Flush line already tells the whole story; more than one is
// a fan-out worth itemizing. DLEs whose reports named no landing land under "".
func (s Snapshot) LandingDrains() []LandingDrain {
	var order []string
	totals := map[string]*LandingDrain{}
	note := func(name string, done, total int64) {
		ld, ok := totals[name]
		if !ok {
			ld = &LandingDrain{Landing: name}
			totals[name] = ld
			order = append(order, name)
		}
		ld.Done += done
		ld.Total += total
	}
	for _, d := range s.DLEs {
		if !d.Drains() {
			continue
		}
		if len(d.Landings) == 0 {
			note("", d.DrainBytes, d.OutBytes)
			continue
		}
		for _, name := range d.Landings {
			note(name, d.Drained[name], d.OutBytes)
		}
	}
	out := make([]LandingDrain, len(order))
	for i, name := range order {
		out[i] = *totals[name]
	}
	return out
}

// LandingDrainRate is one landing's draining throughput in compressed bytes/sec,
// over the same window as DrainRate.
func (s Snapshot) LandingDrainRate(done int64, now time.Time) float64 {
	if s.DrainStartedAt.IsZero() {
		return 0
	}
	end := now
	if s.Phase.Terminal() && !s.EndedAt.IsZero() {
		end = s.EndedAt
	}
	secs := end.Sub(s.DrainStartedAt).Seconds()
	if secs <= 0 {
		return 0
	}
	return float64(done) / secs
}

// TotalOnVolume sums the bytes that have landed on the authoritative volume.
func (s Snapshot) TotalOnVolume() int64 {
	return sum(s.DLEs, func(d DLE) int64 { return d.OnVolume() })
}

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
		case StateCanceled:
			// Counted separately (see Snapshot.Canceled); excluded from the live buckets so
			// an interrupted DLE is not miscounted as still pending.
		default:
			pending++
		}
	}
	return
}

// Canceled tallies DLEs interrupted in flight by a canceled run.
func (s Snapshot) Canceled() int {
	var n int
	for _, d := range s.DLEs {
		if d.State == StateCanceled {
			n++
		}
	}
	return n
}

// Elapsed is the wall time from start to the reference instant (EndedAt for a
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

// Rate is the dumping throughput in uncompressed bytes/sec, measured over the dump
// window (run start to the last dump's finish). Scoping it to that window means it
// reflects dumping speed and does not decay while the drainer works through a tail of
// flushes after dumping is done. 0 until measurable time has passed.
func (s Snapshot) Rate(now time.Time) float64 {
	end := now
	if !s.DumpEndedAt.IsZero() {
		end = s.DumpEndedAt
	} else if s.Phase.Terminal() && !s.EndedAt.IsZero() {
		end = s.EndedAt
	}
	secs := end.Sub(s.StartedAt).Seconds()
	if secs <= 0 {
		return 0
	}
	return float64(s.TotalDone()) / secs
}

// DrainRate is the draining throughput in compressed bytes/sec, measured from the first
// drain to now (or the run's end). 0 before any drain has started or until time passes.
func (s Snapshot) DrainRate(now time.Time) float64 {
	if s.DrainStartedAt.IsZero() {
		return 0
	}
	end := now
	if s.Phase.Terminal() && !s.EndedAt.IsZero() {
		end = s.EndedAt
	}
	secs := end.Sub(s.DrainStartedAt).Seconds()
	if secs <= 0 {
		return 0
	}
	return float64(s.TotalDrained()) / secs
}

// Pct is the run's overall dump completion against the total estimate (0..100).
func (s Snapshot) Pct() float64 { return pct(s.TotalDone(), s.TotalEst()) }

// DrainPct is the run's overall drain completion: bytes copied to the landing against
// the compressed total to drain (0..100). 0 when nothing is staged for draining.
func (s Snapshot) DrainPct() float64 { return pct(s.TotalDrained(), s.TotalToDrain()) }

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
