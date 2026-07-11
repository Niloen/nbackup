// Package spool is the run's concurrent write seam: an archive store over one or more landing
// lanes plus optional holding disks. It models Amanda's driver/taper split, in one process (see
// docs/design/concurrent-writes.md):
//
//   - Each producer (and each drain) writes bytes on its own goroutine, driving an archiveio
//     writer bound over a landing's (or holding disk's) seams. All byte I/O — part
//     headers, payload, footer, member index, SHA — happens there.
//   - A single orchestrator goroutine runs every writer's control calls: volume alloc/roll (the
//     librarian) and the catalog Record. So across every lane, rolls and placements serialize
//     onto one goroutine (the sole catalog writer), with no bulk bytes flowing through it — a
//     slow drive can't block it.
//
// Routing is the caller's: the dumper resolves each DLE's landing route (one medium, or several
// for a fan-out, primary first) and asks the spool for that route's Ingest. The spool decides per
// archive whether to stage it on a holding disk (then drain it to each landing later, reclaiming
// only after the whole route is served) or, when no disk fits or none is configured, write it
// straight to the landing(s) — one bare writer, or an archiveio.Tee cutting lockstep parts across
// the route. A lane's writers cap serializes a serial medium (a tape writes one archive at
// a time); a concurrent medium (cloud/disk) runs several writers at once. Holding back-pressure
// is the Pool's. Failure is any-lane-suffices: a failed landing is tripped for the rest of the
// run (a Warning names the `nb sync` repair) and the run continues on the survivors; only an
// archive whose every landing failed aborts the run so producers stop — never dropping data —
// and a rerun (or flush) fills in.
//
// The crash-recovery counterpart (draining what a crashed run left staged) is conductor.Flush.
package spool

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// Backing is one landing the spool drains (or writes) archives to: its medium name, its part
// allocators and run store (the fs Session, whose Recorder the spool routes through its
// orchestrator), the medium's byte-rate Limiter, and Writers — how many writes to it may run at
// once, direct writes and drains alike (the medium's `writers` cap, defaulting to its drive count
// for a serial medium or the worker count for a concurrent one; see conductor.landingWriters).
// Each Backing configures one runtime lane.
type Backing struct {
	Name string
	// Allocs is the landing's part allocators, one per concurrent writer: a single allocator shared
	// by all writers for a concurrent medium (disk/cloud), or one per drive for a serial multi-drive
	// tape library (each bound to its own drive so two archives write independent tapes).
	Allocs []archiveio.PartAllocator
	// Rec is the landing's recorder — the fs Session every writer's commits are recorded
	// through, one per medium regardless of drive count. (A landing is only recorded to;
	// read-back and reclaim exist only on the holding Pool's Disks.)
	Rec     archiveio.Recorder
	Writers int
	Lim     *ratelimit.Limiter
}

// Config is what the conductor wires a Spool from: the Backings it drains to (the run's landings), the
// Holding Pool (empty = never buffer), the RunSpec + clock the spool authors concurrent writers with,
// and the run's progress + log seams.
type Config struct {
	Backings []Backing
	Holding  *Pool
	Spec     archiveio.RunSpec
	Now      func() time.Time
	Tracker  *progress.Tracker
	Logf     func(format string, args ...any)
}

// lane is one landing's runtime state (a Backing, wired up): its per-drive allocators, its run
// store, the medium's rate limiter, and permits — the lease that both bounds concurrency and hands
// each writer an allocator index (a permit IS an allocator lease). permits is buffered to the
// concurrency width; each buffered value is an allocator index a write may use (a distinct drive
// for a multi-drive tape library, or index 0 repeated for a shared single allocator). A writer (a
// direct write or a drain) pops an index for the duration of its write and pushes it back on
// close, so a drive is used by one archive at a time and the byte I/O of the others runs in
// parallel.
type lane struct {
	name    string
	allocs  []archiveio.PartAllocator
	rec     archiveio.Recorder
	lim     *ratelimit.Limiter
	permits chan int
	// tripped marks the lane dead for the rest of the run after a write to it failed —
	// no further archive retries a down medium. Set once (by Spool.trip, which also
	// records the run warning); read before routing an archive's copies. As long as a
	// route keeps one live lane the run continues and `nb sync` repairs the gap; only
	// an archive whose every lane is dead aborts the run (it would land nowhere).
	tripped atomic.Bool
}

// newLane wires one Backing into its runtime lane, filling permits with one allocator index per
// concurrent writer. A multi-allocator lane (a tape drive per allocator) hands out distinct
// indices 0..writers-1; a single-allocator lane (disk/cloud) hands out index 0 repeated, so its
// writers share the one allocator (their control serialises on the orchestrator).
func newLane(b Backing) *lane {
	writers := b.Writers
	if writers < 1 {
		writers = 1
	}
	permits := make(chan int, writers)
	for k := 0; k < writers; k++ {
		idx := k
		if idx > len(b.Allocs)-1 {
			idx = len(b.Allocs) - 1
		}
		permits <- idx
	}
	return &lane{name: b.Name, allocs: b.Allocs, rec: b.Rec, lim: b.Lim, permits: permits}
}

// lease pops a free allocator index (blocking until one frees); release returns it.
func (l *lane) lease() int      { return <-l.permits }
func (l *lane) release(idx int) { l.permits <- idx }

// Spool is the consuming side of a dump (see the package comment). Build it with New, route a
// producer to a landing with Ingest(name).NewArchive, and close it with Drain.
type Spool struct {
	orch  *orchestrator
	lanes map[string]*lane
	pool  *Pool
	spec  archiveio.RunSpec // authors concurrent writers with the run id
	now   func() time.Time
	tr    *progress.Tracker
	logf  func(format string, args ...any)
	ctx   context.Context

	drains sync.WaitGroup // in-flight holding->landing drains

	mu       sync.Mutex
	abortErr error
	trips    map[string]*laneTrip // per-lane failure records, keyed by lane name (see trip)
	closed   chan struct{}        // closed by Drain; stops the cancel watcher
}

// laneTrip is one tripped lane's failure record: the error that killed it and how
// many archives are missing there (failed or skipped after the trip) — the raw
// material for the run's warning + `nb sync` repair hint.
type laneTrip struct {
	err     error
	missing int
}

// New builds a Spool from cfg and starts its orchestrator — the single goroutine every write's control
// calls (alloc + Record) route onto, so rolls and placements serialize there across all lanes.
// Canceling ctx aborts the run: a watcher aborts the Pool so producers blocked on holding back-pressure
// wake and stop.
func New(ctx context.Context, cfg Config) *Spool {
	if ctx == nil {
		ctx = context.Background()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	sp := &Spool{
		orch:   newOrchestrator(),
		lanes:  make(map[string]*lane, len(cfg.Backings)),
		pool:   cfg.Holding,
		spec:   cfg.Spec,
		now:    now,
		tr:     cfg.Tracker,
		logf:   cfg.Logf,
		ctx:    ctx,
		trips:  make(map[string]*laneTrip),
		closed: make(chan struct{}),
	}
	if sp.logf == nil {
		sp.logf = func(string, ...any) {}
	}
	for _, b := range cfg.Backings {
		sp.lanes[b.Name] = newLane(b)
	}
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				sp.setAbort(ctx.Err())
			case <-sp.closed:
			}
		}()
	}
	return sp
}

// Ingest returns the writer source for the named landing(s). The dumper resolves each DLE's
// landing route (primary first) to these and pulls writers from the handle; the spool never picks
// a landing itself. A multi-name route writes every archive to all of them. A name with no lane is
// skipped — a landing that failed to open at window-start, already warned by the conductor and
// treated exactly like a tripped lane (a route whose EVERY name is laneless aborts at NewArchive,
// since the archive could land nowhere).
func (sp *Spool) Ingest(names ...string) archivefs.Ingest {
	ls := make([]*lane, 0, len(names))
	for _, name := range names {
		if l, ok := sp.lanes[name]; ok {
			ls = append(ls, l)
		}
	}
	return laneHandle{sp: sp, ls: ls}
}

// laneHandle is a producer's view of one landing route — an Ingest bound to its lanes (primary
// first), so NewArchive routes there without the spool re-deciding the landing.
type laneHandle struct {
	sp *Spool
	ls []*lane
}

var _ archivefs.Ingest = laneHandle{}

func (h laneHandle) NewArchive(spec archiveio.ArchiveSpec, est int64) (archivefs.ArchiveSink, error) {
	return h.sp.ingest(h.ls, spec.Host+":"+spec.Path, est, func(a *archiveio.Writer) *archiveio.ArchiveWriter {
		return a.NewArchive(spec)
	})
}

func (h laneHandle) NewCopy(arch record.Archive, est int64) (archivefs.ArchiveSink, error) {
	return h.sp.ingest(h.ls, arch.Host+":"+arch.Path, est, func(a *archiveio.Writer) *archiveio.ArchiveWriter {
		return a.NewCopy(arch)
	})
}

// writerOver builds a concurrent writer over a medium's two seams: a fresh archiveio.Writer whose
// allocator and recorder are wrapped in routed seams, so rolls + Record hop to the orchestrator
// while its byte I/O runs on the caller's goroutine. Cheap enough to build per write.
func (sp *Spool) writerOver(alloc archiveio.PartAllocator, rec archiveio.Recorder, lim *ratelimit.Limiter) *archiveio.Writer {
	return archiveio.NewWriter(&routedAllocator{real: alloc, orch: sp.orch}, &routedRecorder{real: rec, orch: sp.orch}, sp.spec, lim, sp.now)
}

// landingVolume names the distinct volume label(s) an archive's parts landed on: one volume normally,
// several comma-joined when it spanned volumes or, on a multi-drive library, drives. Empty for an
// address-identified landing (no labels), which `nb status` then shows without a volume.
func landingVolume(pos archiveio.ArchivePos) string {
	var labels []string
	seen := map[string]bool{}
	for _, p := range pos.Parts {
		if p.Label != "" && !seen[p.Label] {
			seen[p.Label] = true
			labels = append(labels, p.Label)
		}
	}
	return strings.Join(labels, ",")
}

// ingest reserves ingestion for one archive bound for the lanes ls (primary first), identified by
// dleID and estimated at est bytes, and returns the archive sink to transfer it into — built by
// build over the leased store's Author (NewArchive for a dump, NewCopy for a copy/sync; only the
// writer differs). It blocks for back-pressure: a holding write waits while every fitting disk is
// over capacity; a direct write (no disk fits, or none configured) waits for a free drive on each
// lane. The writer's control calls route onto the orchestrator, and its Close hook releases
// whatever the write leased. It returns the run's error if the spool has aborted.
func (sp *Spool) ingest(ls []*lane, dleID string, est int64, build func(*archiveio.Writer) *archiveio.ArchiveWriter) (archivefs.ArchiveSink, error) {
	if err := sp.Aborted(); err != nil {
		return nil, err
	}
	// A tripped lane is dead for the run: drop it from the route up front (counted
	// missing, `nb sync` repairs). An archive whose whole route is dead can land
	// nowhere — abort, exactly as a landing failure always has.
	live := make([]*lane, 0, len(ls))
	for _, tl := range ls {
		if tl.tripped.Load() {
			sp.noteMissing(tl)
			continue
		}
		live = append(live, tl)
	}
	if len(live) == 0 {
		err := fmt.Errorf("dump %s: every landing on its route failed", dleID)
		sp.setAbort(err)
		return nil, err
	}
	ls = live
	disk, direct, err := sp.pool.Acquire(est)
	if err != nil {
		return nil, err
	}
	if direct {
		return sp.ingestDirect(ls, dleID, build), nil
	}
	// Stage onto holding disk disk; drains copy it to every landing on the route later.
	sp.tr.MarkToHolding(dleID)
	aw := build(sp.writerOver(disk.Alloc, disk.Storage, disk.Lim))
	aw.SetCloser(func() error {
		// On commit the archive occupies the disk until its drains copy it off: charge the landed
		// bytes (so a later Acquire back-pressures on the drain backlog) and launch one drain per
		// live landing — tripped lanes are skipped (counted missing, `nb sync` repairs). A
		// faulted transfer never committed, so there is nothing to drain — just free the estimate.
		if res, ok := aw.Committed(); ok {
			sp.pool.Charge(disk, res.Archive.Compressed)
			sp.tr.StageHolding(res.Archive.Host+":"+res.Archive.Path, disk.Name)
			live := make([]*lane, 0, len(ls))
			for _, tl := range ls {
				if tl.tripped.Load() {
					sp.noteMissing(tl)
					continue
				}
				live = append(live, tl)
			}
			if len(live) == 0 {
				// Landed nowhere: every landing on the route is dead. The staged copy is
				// kept (no reclaim) so the next run's flush completes it; the run fails.
				sp.setAbort(fmt.Errorf("dump %s L%d: every landing on its route failed; staged copy kept on %q for `nb flush`", dleID, res.Archive.Level, disk.Name))
			} else {
				set := &drainSet{remaining: int32(len(live))}
				sp.drains.Add(len(live))
				for _, tl := range live {
					go sp.drainTo(disk, res, tl, set)
				}
			}
		}
		sp.pool.Release(disk, est)
		sp.pool.ReleaseWriter(disk)
		return nil
	})
	return aw, nil
}

// ingestDirect is ingest's no-holding path: the archive streams straight to its
// landing(s). One lane gets today's bare writer; a multi-landing route gets a Tee
// fanning the stream to a writer per lane, all cutting parts in lockstep (see
// archiveio.Tee). Drives are leased in sorted lane-name order — a global order, so
// two producers with overlapping routes can never deadlock on each other's permits —
// while the writers keep route order (primary first). A mid-stream lane failure
// drops that lane from the fan and trips it; the spool learns via the Tee's onDrop.
func (sp *Spool) ingestDirect(ls []*lane, dleID string, build func(*archiveio.Writer) *archiveio.ArchiveWriter) archivefs.ArchiveSink {
	byName := append([]*lane(nil), ls...)
	sort.Slice(byName, func(i, j int) bool { return byName[i].name < byName[j].name })
	drives := make(map[*lane]int, len(ls))
	for _, l := range byName {
		drives[l] = l.lease()
	}
	writers := make([]*archiveio.ArchiveWriter, len(ls))
	for i, l := range ls {
		l, drive := l, drives[l]
		// A direct dump occupies the lane for its whole stream — meter it like any
		// other landing write, so a no-holding run still shows its landing throughput.
		sp.tr.BeginLandingWrite(dleID, l.name)
		aw := build(sp.writerOver(l.allocs[drive], l.rec, l.lim))
		aw.SetCloser(func() error {
			// Surface the landing volume(s) this drive wrote to, so `nb status` shows the
			// multi-drive spread (each DLE on its own tape). A faulted write never committed.
			if res, ok := aw.Committed(); ok {
				sp.tr.LandVolume(res.Archive.Host+":"+res.Archive.Path, landingVolume(res.Pos))
			}
			sp.tr.EndLandingWrite(dleID, l.name)
			l.release(drive)
			return nil
		})
		writers[i] = aw
	}
	if len(writers) == 1 {
		return writers[0]
	}
	tee := archiveio.NewTee(writers, func(i int, err error) { sp.trip(ls[i], err) })
	// Credit each lane its real committed bytes as its parts close, so a slower landing's
	// still-in-flight second copy shows as landed-so-far (not instantly, as the fan-in
	// count would) — what keeps a direct fan-out's landing progress and ETA honest.
	tee.MeterLane(func(i int, landed int64) { sp.tr.AddDrainBytes(dleID, ls[i].name, landed) })
	return tee
}

// drainSet coordinates one staged archive's fan-out: remaining counts the drains still
// running, landed the ones that committed. The LAST drain to finish settles the
// archive — reclaiming the staged copy if any landing has it (a tripped lane is a
// warning, sync repairs), or aborting the run if none does (the copy stays for flush).
type drainSet struct {
	remaining int32
	landed    int32
}

// drainTo copies one staged archive from holding disk disk to lane l — one of the
// archive's route's drains, holding one of l's writers for the copy so it serializes
// with direct writes and other drains to l. A failure trips the lane (the run
// continues on the survivors); the last drain of the set reclaims or aborts.
func (sp *Spool) drainTo(disk *Disk, hres archiveio.CommitResult, l *lane, set *drainSet) {
	defer sp.drains.Done()
	dleID := hres.Archive.Host + ":" + hres.Archive.Path
	sp.tr.StartFlush(dleID, disk.Name)
	drive := l.lease()
	sp.tr.BeginLandingWrite(dleID, l.name) // meter only the copy, not the wait for a free drive
	err := sp.copyOne(disk, hres, l, drive, dleID)
	sp.tr.EndLandingWrite(dleID, l.name)
	l.release(drive)
	if err != nil {
		sp.trip(l, err)
		// The failed copy never committed — nothing of it is on the landing, so its
		// partial meter would be a lie. Void it; the lane's missing copy stays visible
		// (the DLE's drain never reaches 100% there) until `nb sync` repairs it.
		sp.tr.AddDrainBytes(dleID, l.name, 0)
	} else {
		atomic.AddInt32(&set.landed, 1)
	}
	if atomic.AddInt32(&set.remaining, -1) != 0 {
		return
	}
	if atomic.LoadInt32(&set.landed) == 0 {
		sp.setAbort(fmt.Errorf("every landing on %s L%d's route failed (last: %w); staged copy kept on %q for `nb flush`", dleID, hres.Archive.Level, err, disk.Name))
		return
	}
	if rerr := sp.reclaimStaged(disk, hres, dleID); rerr != nil {
		sp.setAbort(rerr)
	}
}

// CopyStaged is the drain-copy core shared by the live drain (copyOne) and the crash-recovery
// conductor.Flush, so their error wording and semantics cannot drift: open the staged holding-disk
// payload, stream it into the landing copy writer cw (whose Commit records the landing placement;
// xfer.Reader closes the payload), wrapping each failure with the caller's label — "flush <dle>
// L<n>" live, "flush <run> <dle>" in recovery.
func CopyStaged(ctx context.Context, label string, open func() (io.ReadCloser, error), cw *archiveio.ArchiveWriter, landing string) error {
	rc, err := open()
	if err != nil {
		return fmt.Errorf("%s: read holding disk: %w", label, err)
	}
	if _, err := xfer.Transfer(ctx, xfer.Reader(rc), xfer.NewFilters(), cw); err != nil {
		return fmt.Errorf("%s to %q: %w", label, landing, err)
	}
	return nil
}

// copyOne reads a staged archive off holding disk disk (on this goroutine) and streams it to l's
// leased drive through a fresh copy writer — its volume rolls and its placement Record route onto the
// orchestrator, while the bytes flow here. The copy's Commit records the landing placement.
// Reclaiming the staged copy is the drain set's job, once every landing on the route has its copy
// (reclaimStaged).
func (sp *Spool) copyOne(disk *Disk, hres archiveio.CommitResult, l *lane, drive int, dleID string) error {
	cw := sp.writerOver(l.allocs[drive], l.rec, l.lim).NewCopy(hres.Archive)
	archiveio.MeterArchive(cw, func(copied int64) { sp.tr.AddDrainBytes(dleID, l.name, copied) })
	label := fmt.Sprintf("flush %s L%d", dleID, hres.Archive.Level)
	open := func() (io.ReadCloser, error) { return disk.Storage.OpenArchiveAt(hres.Ref(), hres.Pos) }
	if err := CopyStaged(sp.ctx, label, open, cw, l.name); err != nil {
		return err
	}
	if res, ok := cw.Committed(); ok {
		sp.tr.LandVolume(dleID, landingVolume(res.Pos)) // the landing tape the drain reached
	}
	sp.logf("flushed %s L%d to %q", dleID, hres.Archive.Level, l.name)
	return nil
}

// reclaimStaged drops one staged archive's holding copy — files + placement, the catalog write, on
// the orchestrator — and frees its disk bytes. Run by the archive's LAST drain, so the staged copy
// outlives the fan-out until every landing on the route has been served (committed, or tripped with
// at least one survivor).
func (sp *Spool) reclaimStaged(disk *Disk, hres archiveio.CommitResult, dleID string) error {
	if err := sp.orch.reclaimOn(disk.Storage, hres.Ref(), hres.Pos); err != nil {
		return fmt.Errorf("flush %s: reclaim holding disk: %w", dleID, err)
	}
	sp.pool.Release(disk, hres.Archive.Compressed)
	sp.tr.FinishFlush(dleID)
	return nil
}

// Drain signals the producers are done and waits for every queued holding->landing drain to finish,
// then stops the orchestrator, returning the run's error if any drain (or a landing write) failed.
// There is no run object to return — each landing's committed archives are already in the catalog
// (recorded as they committed), so the run is read from there.
func (sp *Spool) Drain() error {
	sp.drains.Wait()
	close(sp.closed)
	sp.orch.shutdown()
	return sp.Aborted()
}

// Aborted returns the run's error once a drain or landing write has failed (so producers stop
// scheduling), or nil while healthy.
func (sp *Spool) Aborted() error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.abortErr
}

func (sp *Spool) setAbort(err error) {
	sp.mu.Lock()
	if sp.abortErr == nil {
		sp.abortErr = err
	}
	sp.mu.Unlock()
	if sp.pool != nil {
		sp.pool.Abort(err) // wake producers blocked on holding back-pressure
	}
}

// trip marks lane l dead for the rest of the run, recording the first error that
// killed it and counting the archive that failed as missing there. Idempotent per
// lane; loud once, so a down medium doesn't spam a line per archive.
func (sp *Spool) trip(l *lane, err error) {
	l.tripped.Store(true)
	sp.mu.Lock()
	t, seen := sp.trips[l.name]
	if !seen {
		t = &laneTrip{err: err}
		sp.trips[l.name] = t
	}
	t.missing++
	sp.mu.Unlock()
	if !seen {
		sp.logf("WARNING landing %q failed and is skipped for the rest of the run: %v", l.name, err)
		sp.tr.TripLanding(l.name, err.Error()) // the status file must name the trip, not just show a silent 0
	}
}

// noteMissing counts an archive skipped on an already-tripped lane.
func (sp *Spool) noteMissing(l *lane) {
	sp.mu.Lock()
	if t, ok := sp.trips[l.name]; ok {
		t.missing++
	}
	sp.mu.Unlock()
}

// Warnings reports the run's tripped landings — one line each, with the failure and
// the repair (`nb sync` computes exactly the missing backlog from the catalog's
// placements, so nothing here needs persisting). Empty for a healthy run. Sorted for
// stable output.
func (sp *Spool) Warnings() []string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	names := make([]string, 0, len(sp.trips))
	for name := range sp.trips {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, len(names))
	for i, name := range names {
		t := sp.trips[name]
		out[i] = fmt.Sprintf("landing %q failed (%v); %d archive(s) missing there — repair: %s", name, t.err, t.missing, progress.RepairSync(sp.spec.ID, name))
	}
	return out
}
