package conductor

import (
	"context"
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/spool"
)

// ErrCanceled is the error a run returns when the operator interrupts it (SIGINT/SIGTERM):
// the dump is stopped, its status marked canceled, and nothing sealed. Its message is the
// plain operator-facing reason; it unwraps to context.Canceled so callers can still classify
// it with errors.Is (against either ErrCanceled or context.Canceled) without the raw
// "context canceled" leaking into what the operator reads.
var ErrCanceled error = canceledError{}

type canceledError struct{}

func (canceledError) Error() string { return "canceled by operator (Ctrl-C)" }
func (canceledError) Unwrap() error { return context.Canceled }

// Run executes the plan for a date, producing one sealed slot. Canceling ctx interrupts the
// run: in-flight dumps are killed, the status is marked canceled, and it returns ErrCanceled.
func (c *Conductor) Run(ctx context.Context, now time.Time, logf logf.Logf) (*catalog.Slot, error) {
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
	if n, err := c.d.Flush(time.Now().UTC(), logf); err != nil {
		return nil, fmt.Errorf("flush leftover holding-disk archives before dumping: %w", err)
	} else if n > 0 {
		logf.Log("flushed %d leftover holding-disk archive(s) from a previous run", n)
	}
	// Write the run-status file from the first phase — sizing every DLE, which can be
	// slow — so `nb status` reflects the whole dump cycle, not dead air until the first
	// byte is archived. The estimate phase keeps the file non-terminal (the dump is
	// still to come); a live estimate display, when attached, still erases its region
	// when sizing completes.
	fileSink := c.d.NewFileSink()
	estSink := keepEstimating(fileSink)
	if c.d.EstimateSink != nil {
		estSink = progress.MultiSink(estSink, c.d.EstimateSink)
	}
	plan := c.d.Plan(date, estSink)
	forced := c.d.Cat.ForcedFulls() // captured to consume once the run seals (the lock blocks a concurrent reset)
	for _, w := range plan.Warnings {
		logf.Log("WARNING: %s", w)
	}

	// Pre-flight before creating a slot: the compressor binary and every archiver.
	// Resolving every archiver here also populates the archiver cache, so the parallel
	// workers below only read it (no concurrent writes).
	if err := c.d.CheckCompress(); err != nil {
		return nil, err
	}
	checkedEnc := map[string]bool{}
	checkedHost := map[string]bool{}
	for _, item := range plan.Items {
		if !checkedHost[item.DLE.Host] {
			if err := c.d.ProbeReachable(item.DLE.Host); err != nil {
				return nil, err
			}
			checkedHost[item.DLE.Host] = true
		}
		if err := c.d.PreflightDumptype(item.DLE.DumpTypeName(), item.DLE.Host, true, checkedEnc); err != nil {
			return nil, err
		}
	}

	slotID, _, err := c.allocSlotID(date)
	if err != nil {
		return nil, err
	}
	spec := archiveio.SlotSpec{ID: slotID, CreatedAt: now}

	// The producers dump every DLE; the drain consumes them — buffering each onto a holding disk
	// (one or more media marked `holding: true`) and copying it to its landing, or, when no disk fits
	// or none is configured, writing it straight to its landing. Holding disks let the producers run
	// flat out while a landing's drive drains at its own pace. A run may write several landings at once
	// (per-dumptype routing): the spool opens a backing per distinct landing — see runOrchestrated.
	holdingNames := c.d.HoldingMedia

	// No global worker clamp: per-backing Slots in the spool serialize a serial landing (a single drive
	// writes one archive at a time), so a worker dumping a DLE bound for a serial tape parks on its
	// backing's permit without blocking a worker dumping a DLE bound for cloud. Acquiring the target
	// happens off the dumper's gate, so a parked producer holds no worker slot.
	workers := c.d.Workers

	tr, runLogf := c.progressTracker(slotID, workers, plan.Items, fileSink, logf)
	// Caught here it covers a cancel during the estimate/preflight prelude (above), before any
	// dump starts; runOrchestrated catches one during the dump itself.
	if ctx.Err() != nil {
		tr.SetPhase(progress.PhaseCanceled)
		return nil, ErrCanceled
	}
	sealed, err := c.runOrchestrated(ctx, plan, workers, spec, holdingNames, tr, now, runLogf)
	if err != nil {
		return nil, err
	}
	// The run sealed, so every planned DLE — including every forced one, which the planner
	// scheduled at L0 — has been dumped. Consume the force-full directives now; a failed run
	// (returned above) leaves them so the next run retries. The lock `nb dump` holds means no
	// `nb reset` slipped in between planning and here.
	if err := c.d.Cat.ClearForceFulls(forced); err != nil {
		return nil, err
	}
	return sealed, nil
}

// runOrchestrated executes a dump: it opens the holding disks (the buffer the producers stage onto)
// and a writer per distinct landing the plan routes to, builds the spool over them, runs the
// producers (each routed to its DLE's landing), and drains onto those landings. The spool is the run's
// sole catalog writer.
func (c *Conductor) runOrchestrated(ctx context.Context, plan *planner.Plan, workers int, spec archiveio.SlotSpec, holdingNames []string, tr *progress.Tracker, now time.Time, lf logf.Logf) (*catalog.Slot, error) {
	disks := make([]spool.Disk, len(holdingNames))
	for i, name := range holdingNames {
		pw, err := c.d.OpenWriter(name, spec, now, lf)
		if err != nil {
			tr.SetPhase(progress.PhaseFailed)
			return nil, fmt.Errorf("open holding disk %q: %w", name, err)
		}
		disks[i] = spool.Disk{Name: name, Storage: pw.Store, Capacity: pw.Capacity, Lim: pw.Lim}
	}

	// One backing per distinct landing the plan routes to. Slots is how many writes to it may run at
	// once: one while buffering (the drain copies serially) or for a serial single-drive medium (tape),
	// else the worker count for a concurrent-write medium (disk/cloud — independent objects/files).
	buffering := len(holdingNames) > 0
	landings := distinctLandings(plan.Items, c.d.LandingFor)
	backings := make([]spool.Backing, 0, len(landings))
	for _, name := range landings {
		pw, err := c.d.OpenWriter(name, spec, now, lf)
		if err != nil {
			tr.SetPhase(progress.PhaseFailed)
			return nil, fmt.Errorf("open landing %q: %w", name, err)
		}
		slots := 1
		if !buffering && !pw.Serial {
			slots = workers
		}
		backings = append(backings, spool.Backing{Name: name, Storage: pw.Store, Slots: slots, Lim: pw.Lim})
	}

	sp := spool.New(ctx, spool.Config{
		Backings: backings, Holding: spool.NewPool(disks),
		Spec: spec, Now: func() time.Time { return now },
		Tracker: tr, Logf: lf,
	})
	route := func(it planner.Item) archiveio.Ingest { return sp.Ingest(c.d.LandingFor(it)) }

	dumpErr := c.d.Dmp.Run(ctx, plan.Items, workers, route, tr, lf)

	// A canceled run is not a failure to seal: stop the spool (it aborted on the same ctx, so
	// Drain just joins it without flushing the queued copies — those flush on the next run),
	// mark the status canceled, and seal nothing. Leftover holding archives are reclaimed by
	// the amflush-on-next-dump path.
	if ctx.Err() != nil {
		_ = sp.Drain()
		tr.SetPhase(progress.PhaseCanceled)
		lf.Log("run canceled — nothing sealed; any buffered archives flush on the next run")
		return nil, ErrCanceled
	}

	tr.SetPhase(progress.PhaseSealing)
	if err := firstErr(dumpErr, sp.Drain()); err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, err
	}
	tr.SetPhase(progress.PhaseDone)
	// The slot is its committed archives, read from the catalog (the cache each archive recorded into
	// as it committed). An empty run committed nothing, so the catalog has no entry — report the empty
	// slot the run authored.
	slot, err := c.d.Cat.ReadSlot(spec.ID)
	if err != nil {
		slot = &catalog.Slot{ID: spec.ID}
	}
	return slot, nil
}

// distinctLandings returns the distinct landing media the plan's items route to, in first-seen order
// (so the open order is stable). An empty plan yields none — an empty run opens no landing writer.
func distinctLandings(items []planner.Item, landingFor func(planner.Item) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		l := landingFor(it)
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
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
