package conductor

import (
	"context"
	"errors"
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

// Run executes the plan for a date, producing one sealed run. Canceling ctx interrupts the
// run: in-flight dumps are killed, the status is marked canceled, and it returns ErrCanceled.
func (c *Conductor) Run(ctx context.Context, now time.Time, logf logf.Logf) (*catalog.Run, error) {
	// `now` is the run's single time source: the precise instant the run is stamped
	// committed (CreatedAt) and the moment retention is judged against. The
	// run date — the logical key for the run id, planning, and restore ordering — is
	// just its day. Keeping the two distinct lets two runs on one day carry distinct
	// commit instants, so a sub-day minimum_age can tell them apart.
	//
	// The day is the run's LOCAL calendar date — the day the operator sees on the wall
	// clock, which is what the run id carries. The commit instant itself is stamped in
	// UTC (an absolute time for retention/age math); only the day shown in the id is local.
	date := localDay(now, time.Local)
	now = now.UTC()
	// Guard the restore-order invariant: restore replays a DLE's writers in date order,
	// but the archiver's incremental snapshots advance in dump (wall-clock) order. A
	// run dated earlier than a run already sealed would splice an out-of-order
	// archive into the chain whose snapshot has already moved past it — silently
	// dropping files at restore. Reject it (a same-day rerun, equal date, is fine and
	// takes the next .N). Backdating before today is already caught at the CLI.
	if latest, ok := c.latestRunDate(); ok && record.DateString(date) < latest {
		return nil, fmt.Errorf("cannot dump for %s: run(s) dated %s already exist; an earlier-dated run would corrupt the incremental restore order (snapshots have advanced past it) — dump on or after %s", record.DateString(date), latest, latest)
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

	// Pre-flight before creating a run: the compressor binary and every archiver.
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

	runID, _, err := c.allocRunID(date)
	if err != nil {
		return nil, err
	}
	spec := archiveio.RunSpec{ID: runID, CreatedAt: now}

	// The producers dump every DLE; the drain consumes them — buffering each onto a holding disk
	// (one or more media marked `holding: true`) and copying it to its landing, or, when no disk fits
	// or none is configured, writing it straight to its landing. Holding disks let the producers run
	// flat out while a landing's drive drains at its own pace. A run may write several landings at once
	// (per-dumptype routing): the spool opens a backing per distinct landing — see runOrchestrated.
	holdingNames := c.d.HoldingMedia

	// No global worker clamp: per-backing Writers in the spool serialize a serial landing (a single drive
	// writes one archive at a time), so a worker dumping a DLE bound for a serial tape parks on its
	// backing's permit without blocking a worker dumping a DLE bound for cloud. Acquiring the target
	// happens off the dumper's gate, so a parked producer holds no worker permit.
	workers := c.d.Workers

	tr, runLogf := c.progressTracker(runID, workers, plan.Items, fileSink, logf)
	// Caught here it covers a cancel during the estimate/preflight prelude (above), before any
	// dump starts; runOrchestrated catches one during the dump itself.
	if ctx.Err() != nil {
		tr.SetPhase(progress.PhaseCanceled)
		return nil, ErrCanceled
	}
	sealed, err := c.runOrchestrated(ctx, plan, workers, spec, holdingNames, tr, now, runLogf)
	if err != nil {
		// A failed run may still have committed archives (a partial dump, or one DLE
		// failing while its run-mates landed) — pass the committed run through so the
		// caller's failure record carries the run id and per-DLE stats, not a blank.
		return sealed, err
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

// runOrchestrated executes a dump: it runs the dumper as the producer over a spool spanning every
// distinct landing the plan routes to (each DLE routed to its landing), and reports the sealed run. The
// spool wiring — holding disks, per-landing backings, the drain lifecycle — is withSpool, shared with
// copy/sync so that machinery lives in one place.
func (c *Conductor) runOrchestrated(ctx context.Context, plan *planner.Plan, workers int, spec archiveio.RunSpec, holdingNames []string, tr *progress.Tracker, now time.Time, lf logf.Logf) (*catalog.Run, error) {
	landings := distinctLandings(plan.Items, c.d.LandingFor)
	err := c.withSpool(ctx, landings, holdingNames, spec, workers, tr, now, lf, func(sp *spool.Spool, _ archiveio.ReadStore) error {
		route := func(it planner.Item) archiveio.Ingest { return sp.Ingest(c.d.LandingFor(it)) }
		return c.d.Dmp.Run(ctx, plan.Items, workers, route, tr, lf) // a dump keeps no media: it only writes, and reads no medium
	})
	if err != nil {
		// Even a failed run keeps every archive that committed (the archive is the commit
		// unit; there is no run-level rollback). Return what the catalog holds alongside the
		// error — a partial dump commits a valid archive, so its run id and stats belong in
		// the failure record. A cancel seals nothing on purpose, so it stays bare.
		if !errors.Is(err, ErrCanceled) {
			if run, rerr := c.d.Cat.ReadRun(spec.ID); rerr == nil {
				return run, err
			}
		}
		return nil, err
	}
	// The run is its committed archives, read from the catalog (the cache each archive recorded into
	// as it committed). An empty run committed nothing, so the catalog has no entry — report the empty
	// run the run authored.
	run, rerr := c.d.Cat.ReadRun(spec.ID)
	if rerr != nil {
		run = &catalog.Run{ID: spec.ID}
	}
	return run, nil
}

// withSpool wires a spool from a set of landing media plus the holding disks, runs the producer (run)
// over it, and drains — the one place holding-disk gathering, per-landing backing/writer construction, and
// the drain/cancel/seal lifecycle live. Dump passes the plan's landings and a dumper producer; copy and
// sync pass a single target landing and a copier producer. run builds its own route from sp (so this
// helper never sees the producer's item type). It returns nil on a sealed run, ErrCanceled on a cancel,
// or the first producer/drain error.
//
// withSpool is also the window's ownership handover: at window-open every medium the run writes
// gets exactly one owner. The written media (landings + holding disks) are write-claimed for the
// window (Deps.ClaimWrites) — a read-mount onto one is refused for the window's duration, so a
// producer that reads (copy/sync opening its source archives) can only reach media the window
// does not write, failing over past a written copy like any unavailable one. The catalog splits
// the same way: the run mutates the live catalog while the closure reads the window's View copy
// (Deps.OpenReader; sound because a session never reads its own writes). The window closes
// unconditionally when withSpool returns — every archive recorded before then is already
// persisted (the archive is the commit unit).
func (c *Conductor) withSpool(ctx context.Context, landings, holdingNames []string, spec archiveio.RunSpec, workers int, tr *progress.Tracker, now time.Time, lf logf.Logf, run func(sp *spool.Spool, ro archiveio.ReadStore) error) (err error) {
	// The ownership claim: the written media belong to this window until it ends.
	release, err := c.d.ClaimWrites(append(append([]string(nil), landings...), holdingNames...))
	if err != nil {
		return err
	}
	defer release()
	// The window opens here — before the spool (and its orchestrator and drains) exists,
	// while this goroutine is the only one touching the catalog.
	view, win, err := c.d.Cat.OpenWindow()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := win.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	ro := c.d.OpenReader(view)
	// A copy/sync run has no progress tracker (it reports through its own report), so phase
	// transitions are a no-op there; a dump always has one.
	setPhase := func(p progress.Phase) {
		if tr != nil {
			tr.SetPhase(p)
		}
	}
	disks := make([]spool.Disk, len(holdingNames))
	for i, name := range holdingNames {
		pw, err := c.d.OpenWriter(name, spec, now, lf)
		if err != nil {
			setPhase(progress.PhaseFailed)
			return fmt.Errorf("open holding disk %q: %w", name, err)
		}
		disks[i] = spool.Disk{Name: name, Storage: pw.Stores[0], Capacity: pw.Capacity, Lim: pw.Lim, Writers: pw.Writers}
	}

	// One backing per landing; landingWriters decides how many writes to it may run at once.
	backings := make([]spool.Backing, 0, len(landings))
	for _, name := range landings {
		pw, err := c.d.OpenWriter(name, spec, now, lf)
		if err != nil {
			setPhase(progress.PhaseFailed)
			return fmt.Errorf("open landing %q: %w", name, err)
		}
		writers := landingWriters(pw, workers)
		stores := make([]archiveio.WriteStore, len(pw.Stores))
		for i, s := range pw.Stores {
			stores[i] = s // a Store is a WriteStore; the spool only writes backings
		}
		backings = append(backings, spool.Backing{Name: name, Stores: stores, Writers: writers, Lim: pw.Lim})
	}

	sp := spool.New(ctx, spool.Config{
		Backings: backings, Holding: spool.NewPool(disks),
		Spec: spec, Now: func() time.Time { return now },
		Tracker: tr, Logf: lf,
	})

	runErr := run(sp, ro)

	// A canceled run is not a failure to seal: stop the spool (it aborted on the same ctx, so Drain just
	// joins it without flushing the queued copies — those flush on the next run), mark the status
	// canceled, and seal nothing. Leftover holding archives are reclaimed by the amflush-on-next path.
	if ctx.Err() != nil {
		_ = sp.Drain()
		setPhase(progress.PhaseCanceled)
		lf.Log("run canceled — nothing sealed; any buffered archives flush on the next run")
		return ErrCanceled
	}

	setPhase(progress.PhaseSealing)
	if err := firstErr(runErr, sp.Drain()); err != nil {
		setPhase(progress.PhaseFailed)
		return err
	}
	setPhase(progress.PhaseDone)
	return nil
}

// CopyRun runs a copier producer over a spool that writes to a single target landing (no holding
// buffer), for nb copy / nb sync — the same spool wiring a dump uses, so the target's drives are leased
// one per concurrent copy. run drives the transfers (it builds its route from sp); there is no progress
// tracker (the caller reports through its own report). spec.ID tags the member index each copied archive
// records under, and spec.CreatedAt stamps the run's authoring time. source is the medium the copier
// reads; the window's write claim on target would refuse those reads anyway (see withSpool), but the
// same-medium case is rejected here so the operator gets the direct message, not a failed-over read.
func (c *Conductor) CopyRun(ctx context.Context, target, source string, spec archiveio.RunSpec, workers int, now time.Time, lf logf.Logf, run func(sp *spool.Spool, ro archiveio.ReadStore) error) error {
	if source == target {
		return fmt.Errorf("medium %q cannot be both read and written in one run", target)
	}
	return c.withSpool(ctx, []string{target}, nil, spec, workers, nil, now, lf, run)
}

// landingWriters is how many writes to one landing may run at once — the medium's `writers`
// cap, whichever path the write takes (a dumper's direct dump and a drain copying a staged
// archive lease the same permits). Unset, the medium's natural width applies: one archive per
// drive on a serial multi-drive library, else the worker count (a concurrent-write disk/cloud
// absorbs independent files). A serial medium never exceeds its drives either way — two
// archives cannot interleave on one rolling volume.
func landingWriters(pw PreparedWriter, workers int) int {
	writers := pw.Writers
	if writers == 0 {
		if pw.Serial {
			writers = len(pw.Stores)
		} else {
			writers = workers
		}
	}
	if pw.Serial && writers > len(pw.Stores) {
		writers = len(pw.Stores)
	}
	if writers < 1 {
		writers = 1
	}
	return writers
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
