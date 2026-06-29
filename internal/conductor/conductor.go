// Package conductor is NBackup's run lane: it executes one plan into one sealed
// slot — flushing leftovers, pre-flighting tools, opening the landing writer,
// running the producers, and draining onto the landing. It is the dump
// orchestration the engine used to do inline (Run/runOrchestrated), split out
// behind a narrow dependency slice. The methods are stubs in this commit (the
// engine still does the real work); a later lane fills them in.
package conductor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/dumper"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/spool"
)

// PreparedWriter is the folded view of a medium opened for writing: the slot store
// the producers author into, whether the medium can span volumes (so the caller can
// clamp parallelism), and the medium's capacity in bytes. The engine builds it from
// its clerk/librarian machinery so the conductor stays free of those packages.
type PreparedWriter struct {
	Store    archiveio.ArchiveStore
	CanSpan  bool
	Capacity int64
}

// Conductor holds the slice of the orchestrator a single run needs. It is
// per-run (it carries the run's open landing volume and progress sinks), built
// fresh each Run; the closures bind to the engine's own machinery.
type Conductor struct {
	cat               *catalog.Catalog
	dmp               *dumper.Dumper
	plan              func(date time.Time, sink progress.Sink) *planner.Plan
	vol               media.Volume
	openWriter        func(medium string, spec archiveio.SlotSpec, now time.Time, lf logf.Logf) (PreparedWriter, error)
	checkCompress     func() error
	probeReachable    func(host string) error
	preflightDumptype func(dt, host string, checkArchiver bool, checked map[string]bool) error
	flush             func(now time.Time, lf logf.Logf) (int, error)
	holdingMedia      func() []string
	workers           func() int
	newFileSink       func() progress.Sink
	landing           string
	runSink           progress.Sink
	estimateSink      progress.Sink
}

// Deps is the exported mirror of the Conductor's dependency slice.
type Deps struct {
	Cat               *catalog.Catalog
	Dmp               *dumper.Dumper
	Plan              func(date time.Time, sink progress.Sink) *planner.Plan
	Vol               media.Volume
	OpenWriter        func(medium string, spec archiveio.SlotSpec, now time.Time, lf logf.Logf) (PreparedWriter, error)
	CheckCompress     func() error
	ProbeReachable    func(host string) error
	PreflightDumptype func(dt, host string, checkArchiver bool, checked map[string]bool) error
	Flush             func(now time.Time, lf logf.Logf) (int, error)
	HoldingMedia      func() []string
	Workers           func() int
	NewFileSink       func() progress.Sink
	Landing           string
	RunSink           progress.Sink
	EstimateSink      progress.Sink
}

// New constructs a Conductor from its dependencies.
func New(d Deps) *Conductor {
	return &Conductor{
		cat:               d.Cat,
		dmp:               d.Dmp,
		plan:              d.Plan,
		vol:               d.Vol,
		openWriter:        d.OpenWriter,
		checkCompress:     d.CheckCompress,
		probeReachable:    d.ProbeReachable,
		preflightDumptype: d.PreflightDumptype,
		flush:             d.Flush,
		holdingMedia:      d.HoldingMedia,
		workers:           d.Workers,
		newFileSink:       d.NewFileSink,
		landing:           d.Landing,
		runSink:           d.RunSink,
		estimateSink:      d.EstimateSink,
	}
}

// localDay is the calendar day of instant in loc, at midnight — the operator's
// wall-clock date, which the slot id carries. Taking loc explicitly (rather than
// reading time.Local directly) keeps the day rule unit-testable across zones.
func localDay(instant time.Time, loc *time.Location) time.Time {
	y, m, d := instant.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// Run executes the plan for a date, producing one sealed slot.
func (c *Conductor) Run(now time.Time, logf logf.Logf) (*catalog.Slot, error) {
	// `now` is the run's single time source: the precise instant the slot is stamped
	// committed (CreatedAt) and the moment retention is judged against. The
	// run date — the logical key for the slot id, planning, and restore ordering — is
	// just its day. Keeping the two distinct lets two runs on one day carry distinct
	// commit instants, so a sub-day minimum_age can tell them apart.
	//
	// The day is the run's LOCAL calendar date — the day the operator sees on the wall
	// clock, which is what the slot id carries. The commit instant itself is stamped in
	// UTC (an absolute time for retention/age math); only the day shown in the id is local.
	date := localDay(now, time.Local)
	now = now.UTC()
	// Guard the restore-order invariant: restore replays a DLE's slots in date order,
	// but the archiver's incremental snapshots advance in dump (wall-clock) order. A
	// run dated earlier than a slot already sealed would splice an out-of-order
	// archive into the chain whose snapshot has already moved past it — silently
	// dropping files at restore. Reject it (a same-day rerun, equal date, is fine and
	// takes the next .N). Backdating before today is already caught at the CLI.
	if latest, ok := c.latestSlotDate(); ok && record.DateString(date) < latest {
		return nil, fmt.Errorf("cannot dump for %s: slot(s) dated %s already exist; an earlier-dated run would corrupt the incremental restore order (snapshots have advanced past it) — dump on or after %s", record.DateString(date), latest, latest)
	}
	// Drain any leftover archives a previous holding-disk run crashed before flushing, so the
	// holding disk is clean before this run stages onto it (amflush-on-next-dump). A no-op
	// without a holding disk or when nothing is staged.
	if n, err := c.flush(time.Now().UTC(), logf); err != nil {
		return nil, fmt.Errorf("flush leftover holding-disk archives before dumping: %w", err)
	} else if n > 0 {
		logf.Log("flushed %d leftover holding-disk archive(s) from a previous run", n)
	}
	// Write the run-status file from the first phase — sizing every DLE, which can be
	// slow — so `nb status` reflects the whole dump cycle, not dead air until the first
	// byte is archived. The estimate phase keeps the file non-terminal (the dump is
	// still to come); a live estimate display, when attached, still erases its region
	// when sizing completes.
	fileSink := c.newFileSink()
	estSink := keepEstimating(fileSink)
	if c.estimateSink != nil {
		estSink = progress.MultiSink(estSink, c.estimateSink)
	}
	plan := c.plan(date, estSink)
	forced := c.cat.ForcedFulls() // captured to consume once the run seals (the lock blocks a concurrent reset)
	for _, w := range plan.Warnings {
		logf.Log("WARNING: %s", w)
	}

	// Pre-flight before creating a slot: the compressor binary and every archiver.
	// Resolving every archiver here also populates the archiver cache, so the parallel
	// workers below only read it (no concurrent writes).
	if err := c.checkCompress(); err != nil {
		return nil, err
	}
	checkedEnc := map[string]bool{}
	checkedHost := map[string]bool{}
	for _, item := range plan.Items {
		if !checkedHost[item.DLE.Host] {
			if err := c.probeReachable(item.DLE.Host); err != nil {
				return nil, err
			}
			checkedHost[item.DLE.Host] = true
		}
		if err := c.preflightDumptype(item.DLE.DumpTypeName(), item.DLE.Host, true, checkedEnc); err != nil {
			return nil, err
		}
	}

	slotID, _, err := c.allocSlotID(date)
	if err != nil {
		return nil, err
	}
	spec := archiveio.SlotSpec{ID: slotID, CreatedAt: now}

	// The producers dump every DLE; the drain consumes them — buffering each onto a holding disk
	// (one or more media marked `holding: true`) and copying it to the authoritative landing, or,
	// when no disk fits or none is configured, writing it straight to the landing. Holding disks let
	// the producers run flat out while the landing's drive drains at its own pace.
	holdingNames := c.holdingMedia()
	buffering := len(holdingNames) > 0

	// Open the landing writer here (over the medium's real sink — the spool routes a producer's sink
	// calls to its orchestrator, so no proxy is needed). Opening it now also lets a spanning-capable
	// single drive clamp the workers.
	landPW, err := c.openWriter(c.landing, spec, now, logf)
	if err != nil {
		return nil, err
	}

	// landingSlots is how many landing writes may run at once: one while buffering (the drain copies
	// serially, and a direct write shares that single timeline), and for a direct run one for a
	// spanning-capable single drive (clamping the workers too) or all of them for an unbounded disk.
	workers := c.workers()
	landingSlots := 1
	if !buffering {
		if landPW.CanSpan {
			if workers > 1 {
				logf.Log("medium %q can span volumes; running 1 worker (a single drive writes serially)", c.landing)
				workers = 1
			}
		} else {
			landingSlots = workers
		}
	}

	tr, runLogf := c.progressTracker(slotID, workers, plan.Items, fileSink, logf)
	sealed, err := c.runOrchestrated(plan, workers, landingSlots, spec, holdingNames, landPW, tr, now, runLogf)
	if err != nil {
		return nil, err
	}
	// The run sealed, so every planned DLE — including every forced one, which the planner
	// scheduled at L0 — has been dumped. Consume the force-full directives now; a failed run
	// (returned above) leaves them so the next run retries. The lock `nb dump` holds means no
	// `nb reset` slipped in between planning and here.
	if err := c.cat.ClearForceFulls(forced); err != nil {
		return nil, err
	}
	return sealed, nil
}

// progressTracker builds the run's dump-phase tracker and the log function to use under it. It
// takes over fileSink — the run-status file the estimate phase opened — so `nb status` sees one
// continuous dump cycle, now under the real slot ID. A live terminal sink (when attached) paints
// the same snapshots and suppresses the per-DLE log lines (runLogf becomes nil) so they don't
// scribble over the in-place region. Progress reporting never blocks or fails the backup.
func (c *Conductor) progressTracker(slotID string, workers int, items []planner.Item, fileSink progress.Sink, lf logf.Logf) (*progress.Tracker, logf.Logf) {
	sink := fileSink
	runLogf := lf
	if c.runSink != nil {
		sink = progress.MultiSink(fileSink, c.runSink)
		runLogf = nil
	}
	return progress.NewTracker(slotID, progress.PhaseRunning, workers, planProgress(items), time.Now, sink), runLogf
}

// keepEstimating adapts the estimate phase's status-file sink so the file stays
// non-terminal across the gap between sizing and the first dumped byte. The estimate
// tracker signals completion with a terminal PhaseDone — which a live display uses to
// erase its region — but to the file that would read as a finished run, stopping a
// `nb status --watch` before the dump it is waiting for has even started. Rewriting it
// to PhaseEstimating holds the file open until the dump phase claims it.
func keepEstimating(file progress.Sink) progress.Sink {
	return func(s progress.Snapshot, force bool) {
		if s.Phase.Terminal() {
			s.Phase = progress.PhaseEstimating
		}
		file(s, force)
	}
}

// planProgress projects planner items onto the progress package's seed type,
// keeping progress unaware of the planner.
func planProgress(items []planner.Item) []progress.Plan {
	out := make([]progress.Plan, len(items))
	for i, it := range items {
		out[i] = progress.Plan{Name: it.DLE.ID(), Level: it.Level, EstBytes: it.EstBytes}
	}
	return out
}

// runOrchestrated executes a dump: it opens the holding disks (the buffer the producers stage onto),
// builds the spool over the already-opened landing writer, runs the producers, and seals the
// landing the spool authored. The spool is the run's sole catalog writer.
func (c *Conductor) runOrchestrated(plan *planner.Plan, workers, landingSlots int, spec archiveio.SlotSpec, holdingNames []string, landPW PreparedWriter, tr *progress.Tracker, now time.Time, lf logf.Logf) (*catalog.Slot, error) {
	disks := make([]spool.Disk, len(holdingNames))
	for i, name := range holdingNames {
		pw, err := c.openWriter(name, spec, now, lf)
		if err != nil {
			tr.SetPhase(progress.PhaseFailed)
			return nil, fmt.Errorf("open holding disk %q: %w", name, err)
		}
		disks[i] = spool.Disk{Name: name, Storage: pw.Store, Capacity: pw.Capacity}
	}

	dr := spool.New(spool.Config{
		Holding: spool.NewPool(disks), Tracker: tr, Logf: lf,
		Backing: spool.Backing{Name: c.landing, Storage: landPW.Store, Slots: landingSlots},
	})

	dumpErr := c.dmp.Run(context.Background(), plan.Items, workers, dr, tr, lf)

	tr.SetPhase(progress.PhaseSealing)
	if err := firstErr(dumpErr, dr.Drain()); err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, err
	}
	tr.SetPhase(progress.PhaseDone)
	// The slot is its committed archives, read from the catalog (the cache each archive recorded into
	// as it committed). An empty run committed nothing, so the catalog has no entry — report the empty
	// slot the run authored.
	slot, err := c.cat.ReadSlot(spec.ID)
	if err != nil {
		slot = &catalog.Slot{ID: spec.ID}
	}
	return slot, nil
}

// firstErr returns the first non-nil error, in order.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// latestSlotDate returns the most recent slot date (YYYY-MM-DD) across the whole
// catalog, or ("", false) when no slots exist. Dates are lexically comparable.
func (c *Conductor) latestSlotDate() (string, bool) {
	latest := ""
	for _, s := range c.cat.Slots() {
		if d := s.Date(); d > latest {
			latest = d
		}
	}
	return latest, latest != ""
}

// PlannedSlotID returns the slot id a real dump on date would seal next: the next
// free same-day sequence given the sealed slots already in the catalog. It is the
// preview peer of allocSlotID (which additionally reclaims an unsealed orphan on the
// loaded volume) and exists so `nb dump --dry-run` names the slot a real run would
// produce — not always `.1` — when the date is already sealed.
func (c *Conductor) PlannedSlotID(date time.Time) string {
	have := map[string]bool{}
	for _, s := range c.cat.Slots() {
		have[s.ID] = true
	}
	ds := record.DateString(date)
	for seq := 1; ; seq++ {
		id := record.IDFromParts(ds, seq)
		if !have[id] {
			return id
		}
	}
}

// allocSlotID picks the slot ID for a run on the given date: the first run of
// the day is "slot-DATE", later runs get the next free ".N". A leftover unsealed
// slot from a failed attempt is reclaimed. This consults the volume (the write
// path may touch media) so it is robust to a stale cache.
func (c *Conductor) allocSlotID(date time.Time) (id string, seq int, err error) {
	files, err := c.vol.Files()
	if err != nil {
		// A changer with nothing loaded yet (a fresh library before its first mount,
		// e.g. auto_label on a blank pool) has no files to scan for orphans. The
		// catalog still seeds every known slot id pool-globally below, so treat an
		// empty drive as "no extra files" rather than a hard failure — letting a
		// first dump proceed to PrepareWrite, which mounts and auto-labels a bay.
		if !errors.Is(err, media.ErrNoVolume) {
			return "", 0, err
		}
		files = nil
	}
	present := map[string]bool{} // slot id -> exists (catalog or loaded volume)
	sealed := map[string]bool{}  // slot id -> sealed (immutable; never reuse the id)
	// Seed from the catalog, which indexes every sealed slot across the whole pool.
	// A slot id is pool-global, so a same-day rerun must take the next free .N even
	// when an earlier run sealed onto a different volume (or medium) than the one now
	// loaded — scanning only the loaded volume's Files() would miss it and reuse the
	// id, shadowing that earlier run in the catalog. Catalog slots are sealed by
	// construction (Record runs only after Seal).
	for _, s := range c.cat.Slots() {
		present[s.ID] = true
		sealed[s.ID] = true
	}
	// The loaded volume may also carry an orphan from a failed attempt that the catalog
	// never recorded; note it so its id can be reclaimed below. A slot with any committed
	// archive (a commit footer) is a real recovery point — its id is never reused; one with
	// only uncommitted parts is a reclaimable orphan.
	for _, f := range files {
		present[f.Header.Slot] = true
		if f.Header.Kind == record.KindCommit {
			sealed[f.Header.Slot] = true
		}
	}
	day := record.DateString(date)
	for seq = 1; ; seq++ {
		id = record.IDFromParts(day, seq)
		if !present[id] {
			return id, seq, nil
		}
		if sealed[id] {
			continue // a sealed slot occupies this id; try the next sequence
		}
		// Unsealed leftover from a failed attempt: reclaim its files. A medium that
		// cannot remove individual files (tape — space is reclaimed by relabeling the
		// whole volume) leaves the orphan in place; a scan ignores it (it has no seal),
		// and it is reclaimed on the next relabel. Take the next id rather than failing.
		removed := true
		for _, f := range files {
			if f.Header.Slot != id {
				continue
			}
			if err := c.vol.RemoveFile(f.Pos); err != nil {
				if errors.Is(err, media.ErrNoFileRemoval) {
					removed = false
					break
				}
				return "", 0, err
			}
		}
		if !removed {
			continue
		}
		return id, seq, nil
	}
}
