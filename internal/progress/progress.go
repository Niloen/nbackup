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
	// DumpEndedAt freezes the instant the DLE's dump finished. EndedAt cannot serve:
	// a holding-disk DLE's FinishFlush moves it to the drain's end, so a dump duration
	// derived from EndedAt would silently include the queue wait and the flush.
	DumpEndedAt time.Time `json:"dump_ended_at,omitempty"`
	// WriteSeconds accumulates the DLE's landing-write time: each of its landing
	// writes (a drain copy, or a direct dump holding a lane) from the moment it holds
	// a lane writer to the moment it releases it, summed across lanes (Amanda's
	// per-DLE taper time). Waiting for a free lane does not count.
	WriteSeconds float64 `json:"write_seconds,omitempty"`
	Err          string  `json:"err,omitempty"`
	// Reason is the planner's level explanation and Promoted marks a full pulled
	// forward by promotion — carried through the run status so the sealed run's
	// dump report can say why a DLE ran at its level (why tonight was big).
	Reason   string `json:"reason,omitempty"`
	Promoted bool   `json:"promoted,omitempty"`
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
	// DumpEndedAt freezes the moment the last dump finished, so the dump rate is
	// measured over the dumping window instead of the run's whole wall-clock (it must
	// not decay while the drainer works through a tail of flushes).
	DumpEndedAt time.Time `json:"dump_ended_at,omitempty"`
	// Meters is the per-landing write accounting, keyed by landing name: how long each
	// landing lane has actually been writing (drains and direct dumps alike), so
	// write rates are bytes over *busy* time — never diluted by the stretches the
	// drainer sits idle waiting for dumps to stage.
	Meters map[string]LandingMeter `json:"meters,omitempty"`
	// Recent is a short ring of cumulative byte samples (~the last minute), appended
	// by the Tracker as bytes flow. It is what lets any stateless reader of the
	// status file — `nb status`, the web poller — compute a "right now" rate over the
	// trailing window, and lets that rate honestly decay to zero when flow stops.
	Recent []Sample `json:"recent,omitempty"`
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

// LandingMeter is one landing lane's write accounting: how long it has actually been
// writing (busy time), maintained by the Tracker from Begin/EndLandingWrite events.
// Overlapping writers on the lane count once — busy time is the union of the lane's
// occupied intervals, so bytes over it reads as the lane's aggregate throughput.
type LandingMeter struct {
	BusySeconds float64   `json:"busy_seconds,omitempty"` // closed occupied intervals, summed
	ActiveSince time.Time `json:"active_since,omitempty"` // start of the open interval, zero when idle
	Active      int       `json:"active,omitempty"`       // writers on the lane right now
}

// busyAt is the lane's total busy time as of now, including the open interval.
func (m LandingMeter) busyAt(now time.Time) float64 {
	b := m.BusySeconds
	if m.Active > 0 && !m.ActiveSince.IsZero() && now.After(m.ActiveSince) {
		b += now.Sub(m.ActiveSince).Seconds()
	}
	return b
}

// Sample is one instant's cumulative byte counters, kept in Snapshot.Recent so a
// stateless reader can difference two instants into a trailing-window rate.
type Sample struct {
	T      time.Time `json:"t"`
	Dumped int64     `json:"dumped"` // Snapshot.TotalDone at T
	// Written is the compressed bytes that had reached each landing at T
	// (Snapshot.WrittenTo); Dumping is each then-dumping DLE's DoneBytes, so an
	// in-flight card can show its own trailing rate.
	Written map[string]int64 `json:"written,omitempty"`
	Dumping map[string]int64 `json:"dumping,omitempty"`
}

// writtenTo is the compressed bytes of this DLE that have reached the landing: its
// drained share for a holding-routed DLE, and for a direct dump its output bytes on
// every landing of its route (a Tee writes them lockstep). A DLE with no recorded
// route reports under "".
func (d DLE) writtenTo(landing string) int64 {
	if d.Drains() || d.ToHolding {
		return d.Drained[landing]
	}
	if len(d.Landings) == 0 {
		if landing == "" {
			return d.OutBytes
		}
		return 0
	}
	for _, l := range d.Landings {
		if l == landing {
			return d.OutBytes
		}
	}
	return 0
}

// WrittenTo sums the compressed bytes that have reached one landing so far —
// drain copies and direct writes alike.
func (s Snapshot) WrittenTo(landing string) int64 {
	return sum(s.DLEs, func(d DLE) int64 { return d.writtenTo(landing) })
}

// clampNow substitutes the run's end for now once it is terminal, so rates and
// busy time stop moving when the run does.
func (s Snapshot) clampNow(now time.Time) time.Time {
	if s.Phase.Terminal() && !s.EndedAt.IsZero() {
		return s.EndedAt
	}
	return now
}

// WriteBusy is one landing lane's busy time so far: the wall-clock it has actually
// spent writing (any writer occupying it), as opposed to waiting for dumps.
func (s Snapshot) WriteBusy(landing string, now time.Time) float64 {
	return s.Meters[landing].busyAt(s.clampNow(now))
}

// WriteActive reports whether some writer is on the landing lane right now.
func (s Snapshot) WriteActive(landing string) bool { return s.Meters[landing].Active > 0 }

// WriteRate is one landing's throughput in compressed bytes/sec measured over its
// *busy* time — the lane's real speed while writing (the number to trend, and to
// compare against the device or link), never diluted by idle stretches.
func (s Snapshot) WriteRate(landing string, now time.Time) float64 {
	busy := s.WriteBusy(landing, now)
	if busy <= 0 {
		return 0
	}
	return float64(s.WrittenTo(landing)) / busy
}

// WriteUtilization is the share of the run's elapsed time the landing lane spent
// writing (0..1) — low with a fast lane starved by slow dumps, high when the lane
// is the bottleneck.
func (s Snapshot) WriteUtilization(landing string, now time.Time) float64 {
	elapsed := s.Elapsed(now).Seconds()
	if elapsed <= 0 {
		return 0
	}
	if u := s.WriteBusy(landing, now) / elapsed; u < 1 {
		return u
	}
	return 1
}

// nowWindow is the trailing window the "right now" rates measure over.
const nowWindow = 30 * time.Second

// baseSample picks the newest sample old enough to cover the trailing window
// (falling back to the oldest), the baseline a now-rate differences against.
func (s Snapshot) baseSample(now time.Time) (Sample, bool) {
	if len(s.Recent) == 0 {
		return Sample{}, false
	}
	cut := now.Add(-nowWindow)
	base := s.Recent[0]
	for _, smp := range s.Recent[1:] {
		if smp.T.After(cut) {
			break
		}
		base = smp
	}
	return base, true
}

// rateNow differences the current cumulative value cur against the baseline
// sample's value over the trailing window. Because the denominator runs on the
// reader's clock while the counters only move when bytes do, a stalled (or dead)
// writer's rate honestly decays to zero. Terminal runs report 0 — "right now"
// has no meaning once the run is over.
func (s Snapshot) rateNow(now time.Time, cur int64, at func(Sample) int64) float64 {
	if s.Phase.Terminal() {
		return 0
	}
	// Counters only move when the snapshot does: if it last updated before the
	// window opened, nothing flowed inside it — including any bytes that landed
	// after the final sample, which would otherwise linger as a phantom trickle.
	if !s.UpdatedAt.After(now.Add(-nowWindow)) {
		return 0
	}
	base, ok := s.baseSample(now)
	if !ok {
		return 0
	}
	secs := now.Sub(base.T).Seconds()
	delta := cur - at(base)
	if secs < 1 || delta <= 0 {
		return 0
	}
	return float64(delta) / secs
}

// DumpRateNow is the dumping throughput over the trailing window, in uncompressed
// bytes/sec — "is data moving right now, and how fast".
func (s Snapshot) DumpRateNow(now time.Time) float64 {
	return s.rateNow(now, s.TotalDone(), func(smp Sample) int64 { return smp.Dumped })
}

// WriteRateNow is one landing's write throughput over the trailing window, in
// compressed bytes/sec. Zero when the lane is idle — which is information (the
// drainer is waiting for dumps), not a measurement failure.
func (s Snapshot) WriteRateNow(landing string, now time.Time) float64 {
	return s.rateNow(now, s.WrittenTo(landing), func(smp Sample) int64 { return smp.Written[landing] })
}

// DLERateNow is one dumping DLE's throughput over the trailing window, in
// uncompressed bytes/sec. A DLE younger than the window is measured from its own
// start, so a fresh dump is not underestimated against a baseline it predates.
func (s Snapshot) DLERateNow(name string, now time.Time) float64 {
	if s.Phase.Terminal() {
		return 0
	}
	var dle *DLE
	for i := range s.DLEs {
		if s.DLEs[i].Name == name {
			dle = &s.DLEs[i]
			break
		}
	}
	if dle == nil || dle.State != StateDumping {
		return 0
	}
	if !s.UpdatedAt.After(now.Add(-nowWindow)) { // see rateNow: a stale snapshot has no "now"
		return 0
	}
	base, ok := s.baseSample(now)
	if !ok {
		return 0
	}
	baseVal, baseT := base.Dumping[name], base.T
	if baseVal == 0 && dle.StartedAt.After(baseT) {
		baseT = dle.StartedAt
	}
	secs := now.Sub(baseT).Seconds()
	delta := dle.DoneBytes - baseVal
	if secs < 1 || delta <= 0 {
		return 0
	}
	return float64(delta) / secs
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

// Landings names every landing the run writes to, in first-seen route order
// (primaries lead) with metered-but-routeless lanes appended — the keys WriteRate,
// WriteRateNow, and the meters answer for. A run whose DLEs recorded no route
// reports the single unnamed landing "".
func (s Snapshot) Landings() []string {
	var order []string
	seen := map[string]bool{}
	note := func(name string) {
		if !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
	}
	for _, d := range s.DLEs {
		if len(d.Landings) == 0 {
			note("")
			continue
		}
		for _, l := range d.Landings {
			note(l)
		}
	}
	meterNames := make([]string, 0, len(s.Meters))
	for name := range s.Meters {
		meterNames = append(meterNames, name)
	}
	sort.Strings(meterNames)
	for _, name := range meterNames {
		note(name)
	}
	return order
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
