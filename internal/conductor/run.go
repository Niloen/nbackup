package conductor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/scheduler"
	"github.com/Niloen/nbackup/internal/sizeutil"
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
	// but the archiver's incremental state advances in dump (wall-clock) order. A
	// run dated earlier than a run already sealed would splice an out-of-order
	// archive into the chain whose incremental state has already moved past it —
	// silently dropping files at restore. Reject it (a same-day rerun, equal date, is fine
	// and mints a later time suffix). Backdating before today is already caught at the CLI.
	if latest, ok := c.latestRunDate(); ok && record.DateString(date) < latest {
		return nil, fmt.Errorf("cannot dump for %s: run(s) dated %s already exist; an earlier-dated run would corrupt the incremental restore order (incremental state has advanced past it) — dump on or after %s", record.DateString(date), latest, latest)
	}
	// Drain any leftover archives a previous holding-disk run crashed before flushing, so the
	// holding disk is clean before this run stages onto it (amflush-on-next-dump). A no-op
	// without a holding disk or when nothing is staged. Deliberately time.Now, not `now`:
	// a --date backdate stamps this run, not the leftover flush of a previous one.
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
	plan, err := c.d.Plan(date, estSink)
	if err != nil {
		// Planning failed before a run existed — most often a source enumeration that
		// could not run (fail loud, never guess a partition's match set).
		return nil, err
	}
	forced := c.d.Cat.ForcedFulls() // captured to consume once the run seals (the lock blocks a concurrent reset)
	for _, w := range plan.Warnings {
		logf.Log("WARNING: %s", w)
	}

	// Pre-flight before creating a run: every source host, the compressor binary, and
	// every archiver — the strict mode of the same check `nb plan` previews with.
	// Resolving every archiver here also populates the archiver cache, so the parallel
	// workers below only read it (no concurrent writes). Strict preflight reads only
	// host+dumptype, so the resolved items convert losslessly.
	dles := make([]config.DLE, len(plan.Items))
	for i, item := range plan.Items {
		dles[i] = config.DLE{Host: item.DLE.Host, Path: item.DLE.Source, DumpType: item.DLE.DumpType}
	}
	if _, err := scheduler.Preflight(c.d.Preflight, dles, true); err != nil {
		c.failEstimated(fileSink, plan, err)
		return nil, err
	}

	// Make room on each bounded landing for tonight's estimated bytes BEFORE any
	// byte lands (fslike media; a labeled pool's rotation reclaims at the write
	// itself). An unknown estimate (a first dump) contributes 0 — the reactive
	// no-space path stays the backstop for estimate misses.
	//
	// Any-lane-suffices holds here just like at window-open: a landing that cannot
	// make room (capacity vs. retention is a per-medium promise) is skipped for this
	// run with a warning, and the dump is fatal only when some DLE's whole route is
	// unusable — that DLE could land nowhere. Unlike a down medium, a refusal is not
	// self-healing: the repair `nb sync --to <landing>` re-runs the same math, so it
	// works only once capacity is increased or retention trimmed — the warning says so.
	roomFailed := map[string]error{}
	if c.d.MakeRoom != nil {
		incoming := map[string]int64{}
		var order []string
		for _, item := range plan.Items {
			for _, landing := range c.d.LandingsFor(item) {
				if _, seen := incoming[landing]; !seen {
					order = append(order, landing)
				}
				incoming[landing] += item.EstBytes
			}
		}
		for _, landing := range order {
			freed, err := c.d.MakeRoom(landing, incoming[landing], now, logf)
			if err != nil {
				roomFailed[landing] = fmt.Errorf("make room on %q for tonight's ~%s: %w", landing, sizeutil.FormatBytes(incoming[landing]), err)
				logf.Log("WARNING landing %q cannot make room for tonight's ~%s and is skipped for this run: %v — then repair: %s", landing, sizeutil.FormatBytes(incoming[landing]), err, progress.RepairSync("", landing))
				continue
			}
			if freed > 0 {
				logf.Log("made room on %q: reclaimed %s to fit tonight's ~%s", landing, sizeutil.FormatBytes(freed), sizeutil.FormatBytes(incoming[landing]))
			}
		}
		if err := c.routeFatal(plan.Items, roomFailed); err != nil {
			c.failEstimated(fileSink, plan, err)
			return nil, err
		}
	}

	runID := c.mintRunID(now, time.Local)
	spec := archiveio.RunSpec{ID: runID, CreatedAt: now}

	// The producers dump every DLE; the drain consumes them — buffering each onto a holding disk
	// (one or more media marked `holding: true`) and copying it to every landing on its route, or,
	// when no disk fits or none is configured, writing it straight to its landing(s). Holding disks
	// let the producers run flat out while a landing's drive drains at its own pace. A run may write
	// several landings at once (per-dumptype routing, or a fan-out route like `landing: [s3, gdrive]`):
	// the spool opens a backing per distinct landing — see runOrchestrated.
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
	sealed, err := c.runOrchestrated(ctx, plan, workers, spec, holdingNames, roomFailed, tr, now, runLogf)
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
// copy/sync so that machinery lives in one place. roomFailed names landings the make-room prelude
// already declared unusable (warned there): they get no backing — the spool treats them as tripped
// from the start — and they count as down in the whole-route judgment below.
func (c *Conductor) runOrchestrated(ctx context.Context, plan *planner.Plan, workers int, spec archiveio.RunSpec, holdingNames []string, roomFailed map[string]error, tr *progress.Tracker, now time.Time, lf logf.Logf) (*catalog.Run, error) {
	var landings []string
	for _, l := range distinctLandings(plan.Items, c.d.LandingsFor) {
		if _, down := roomFailed[l]; !down {
			landings = append(landings, l)
		}
	}
	if len(landings) > 1 {
		lf.Log("run writes landings: %s", strings.Join(landings, ", "))
	}
	// Any-lane-suffices holds at window-open too: a landing that fails to OPEN (medium
	// down before the run starts) is skipped with a warning, exactly like one that
	// fails mid-run — fatal only if some item's whole route is unusable (failed to open,
	// or already out at make-room), because that item could land nowhere.
	fatalOpen := func(openFailed map[string]error) error {
		failed := make(map[string]error, len(roomFailed)+len(openFailed))
		for l, err := range roomFailed {
			failed[l] = err
		}
		for l, err := range openFailed {
			failed[l] = err
		}
		return c.routeFatal(plan.Items, failed)
	}
	// The tracker learns each skipped landing so the status file tells the truth: the
	// landing leaves every DLE's route (no drain is owed there — nothing may read as
	// drained to it) and the skip is surfaced with its reason and repair.
	skipLandings(tr, roomFailed)
	err := c.withSpool(ctx, landings, holdingNames, spec, workers, tr, now, lf, fatalOpen, func(sp *spool.Spool, _ archivefs.ReadStore) error {
		route := func(it planner.Item) archivefs.Ingest { return sp.Ingest(c.d.LandingsFor(it)...) }
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
// gets exactly one owner. Opening a medium's writer (Deps.OpenWriter) takes its write claim; the
// deferred PreparedWriter.Release calls below give the claims back at window end. A read-mount
// onto a claimed medium is refused for the window's duration, so a producer that reads
// (copy/sync opening its source archives) can only reach media the window does not write,
// failing over past a written copy like any unavailable one. The catalog splits the same way:
// the run mutates the live catalog while the closure reads the window's View copy
// (Deps.OpenReader; sound because a session never reads its own writes). The window closes
// unconditionally when withSpool returns — every archive recorded before then is already
// persisted (the archive is the commit unit).
// fatalOpen, when non-nil, lets a run tolerate landings that fail to OPEN: each failure is
// warned and its backing skipped (the spool treats the name as tripped from the start), and
// fatalOpen judges the collected failures — returning an error when some producer's whole
// route is down. nil means any open failure is fatal (copy/sync, whose one target is the job).
func (c *Conductor) withSpool(ctx context.Context, landings, holdingNames []string, spec archiveio.RunSpec, workers int, tr *progress.Tracker, now time.Time, lf logf.Logf, fatalOpen func(failed map[string]error) error, run func(sp *spool.Spool, ro archivefs.ReadStore) error) (err error) {
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
	// fail marks the run failed with the run-level reason, so a failure no DLE owns
	// (an open refusal, a drain error) still names itself in the status file.
	fail := func(err error) {
		if tr != nil {
			tr.Fail(err)
		}
	}
	// Each OpenWriter takes its medium's write claim; the deferred Release returns it at
	// window end — after the drain has joined, so the claim spans every write.
	releaseWriter := func(pw PreparedWriter) {
		if pw.Release != nil {
			pw.Release()
		}
	}
	disks := make([]spool.Disk, len(holdingNames))
	for i, name := range holdingNames {
		pw, err := c.d.OpenWriter(name, spec, now, lf)
		if err != nil {
			err = fmt.Errorf("open holding disk %q: %w", name, err)
			fail(err)
			return err
		}
		defer releaseWriter(pw)
		disks[i] = spool.Disk{Name: name, Alloc: pw.Allocs[0], Storage: pw.Store, Capacity: pw.Capacity, Lim: pw.Lim, Writers: pw.Writers}
	}

	// One backing per landing; landingWriters decides how many writes to it may run at once.
	backings := make([]spool.Backing, 0, len(landings))
	openFailed := map[string]error{}
	for _, name := range landings {
		pw, err := c.d.OpenWriter(name, spec, now, lf)
		if err != nil {
			if fatalOpen == nil {
				err = fmt.Errorf("open landing %q: %w", name, err)
				fail(err)
				return err
			}
			openFailed[name] = err
			lf.Log("WARNING landing %q failed to open and is skipped for this run: %v — repair: %s", name, err, progress.RepairSync(spec.ID, name))
			continue
		}
		defer releaseWriter(pw)
		writers := landingWriters(pw, workers)
		backings = append(backings, spool.Backing{Name: name, Allocs: pw.Allocs, Rec: pw.Store, Writers: writers, Lim: pw.Lim})
	}
	if len(openFailed) > 0 {
		if err := fatalOpen(openFailed); err != nil {
			fail(err)
			return err
		}
		skipLandings(tr, openFailed) // survived the judgment: the run proceeds without them
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
	drainErr := sp.Drain()
	// A tripped landing is a warning, not a failure: every archive still landed
	// somewhere (a route with NO survivor aborts instead), and the catalog's
	// placements record exactly what is missing — so the repair is one `nb sync`,
	// named here loud enough to act on.
	for _, w := range sp.Warnings() {
		lf.Log("WARNING %s", w)
	}
	if err := firstErr(runErr, drainErr); err != nil {
		fail(err)
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
func (c *Conductor) CopyRun(ctx context.Context, target, source string, spec archiveio.RunSpec, workers int, now time.Time, lf logf.Logf, run func(sp *spool.Spool, ro archivefs.ReadStore) error) error {
	if source == target {
		return fmt.Errorf("medium %q cannot be both read and written in one run", target)
	}
	return c.withSpool(ctx, []string{target}, nil, spec, workers, nil, now, lf, nil, run)
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
			writers = len(pw.Allocs)
		} else {
			writers = workers
		}
	}
	if pw.Serial && writers > len(pw.Allocs) {
		writers = len(pw.Allocs)
	}
	if writers < 1 {
		writers = 1
	}
	return writers
}

// routeFatal is the any-lane-suffices judgment: given the landings declared unusable
// this run (failed to open, or refused at make-room), it is fatal exactly when some
// item's whole route is down — that item could land nowhere; every landing it names
// was already warned about individually. nil when failed is empty.
func (c *Conductor) routeFatal(items []planner.Item, failed map[string]error) error {
	if len(failed) == 0 {
		return nil
	}
	for _, it := range items {
		route := c.d.LandingsFor(it)
		alive := 0
		for _, l := range route {
			if _, down := failed[l]; !down {
				alive++
			}
		}
		if alive == 0 {
			return fmt.Errorf("dump %s: no landing on its route is usable: %w", it.DLE.ID(), failed[route[0]])
		}
	}
	return nil
}

// skipLandings records each unusable landing on the run's tracker (removed from every DLE's
// route, kept on the snapshot with its reason) — in name order, so the status file is stable.
// A no-op with no failures or no tracker (copy/sync report through their own report).
func skipLandings(tr *progress.Tracker, failed map[string]error) {
	names := make([]string, 0, len(failed))
	for name := range failed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tr.SkipLanding(name, failed[name].Error())
	}
}

// distinctLandings returns the distinct landing media the plan's items route to, in first-seen order
// (so the open order is stable) — the union across every item's route, since a fan-out item writes
// several. An empty plan yields none — an empty run opens no landing writer.
func distinctLandings(items []planner.Item, landingsFor func(planner.Item) []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		for _, l := range landingsFor(it) {
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
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
