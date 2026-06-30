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
	// (one or more media marked `holding: true`) and copying it to the authoritative landing, or,
	// when no disk fits or none is configured, writing it straight to the landing. Holding disks let
	// the producers run flat out while the landing's drive drains at its own pace.
	holdingNames := c.d.HoldingMedia
	buffering := len(holdingNames) > 0

	// Open the landing writer here (over the medium's real sink — the spool routes a producer's sink
	// calls to its orchestrator, so no proxy is needed). Opening it now also lets a serial single
	// drive clamp the workers.
	landPW, err := c.d.OpenWriter(c.d.Landing, spec, now, logf)
	if err != nil {
		return nil, err
	}

	// landingSlots is how many landing writes may run at once: one while buffering (the drain copies
	// serially, and a direct write shares that single timeline), one for a serial single-drive medium
	// (tape — a spanning serial drive also clamps the workers, since it cannot interleave two archives'
	// parts), and all of them for a concurrent-write medium (disk or cloud, which split each archive
	// into independent objects/files — fully parallel even with part_size set).
	workers := c.d.Workers
	landingSlots := 1
	if !buffering {
		if landPW.Serial {
			if landPW.CanSpan && workers > 1 {
				logf.Log("medium %q writes serially and can span volumes; running 1 worker (a single drive cannot interleave archives)", c.d.Landing)
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
	if err := c.d.Cat.ClearForceFulls(forced); err != nil {
		return nil, err
	}
	return sealed, nil
}

// runOrchestrated executes a dump: it opens the holding disks (the buffer the producers stage onto),
// builds the spool over the already-opened landing writer, runs the producers, and seals the
// landing the spool authored. The spool is the run's sole catalog writer.
func (c *Conductor) runOrchestrated(plan *planner.Plan, workers, landingSlots int, spec archiveio.SlotSpec, holdingNames []string, landPW PreparedWriter, tr *progress.Tracker, now time.Time, lf logf.Logf) (*catalog.Slot, error) {
	disks := make([]spool.Disk, len(holdingNames))
	for i, name := range holdingNames {
		pw, err := c.d.OpenWriter(name, spec, now, lf)
		if err != nil {
			tr.SetPhase(progress.PhaseFailed)
			return nil, fmt.Errorf("open holding disk %q: %w", name, err)
		}
		disks[i] = spool.Disk{Name: name, Storage: pw.Store, Capacity: pw.Capacity}
	}

	sp := spool.New(spool.Config{
		Holding: spool.NewPool(disks), Tracker: tr, Logf: lf,
		Backing: spool.Backing{Name: c.d.Landing, Storage: landPW.Store, Slots: landingSlots},
	})

	dumpErr := c.d.Dmp.Run(context.Background(), plan.Items, workers, sp, tr, lf)

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

// firstErr returns the first non-nil error, in order.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
