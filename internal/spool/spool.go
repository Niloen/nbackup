// Package spool is the run's concurrent write seam: an archive store over one or more landing
// lanes plus optional holding disks. It models Amanda's driver/taper split, in one process (see
// docs/design/concurrent-writes.md):
//
//   - Each producer (and each drain) writes bytes on its own goroutine, driving an archiveio
//     writer the clerk built over a landing's (or holding disk's) Session. All byte I/O — part
//     headers, payload, footer, member index, SHA — happens there.
//   - A single orchestrator goroutine runs every Session's control calls: volume alloc/roll (the
//     librarian) and the catalog Record. So across every lane, rolls and placements serialize
//     onto one goroutine (the sole catalog writer), with no bulk bytes flowing through it — a
//     slow drive can't block it.
//
// Routing is the caller's: the dumper resolves each DLE's landing and asks the spool for that
// landing's Ingest. The spool decides per archive whether to stage it on a holding disk (then
// drain it to the landing later) or, when no disk fits or none is configured, write it straight
// to the landing. A lane's writers cap serializes a serial medium (a tape writes one archive at
// a time); a concurrent medium (cloud/disk) runs several writers at once. Holding back-pressure
// is the Pool's. A landing failure aborts the run so producers stop and the run fails — never
// dropping data — and a rerun fills in.
//
// The crash-recovery counterpart (draining what a crashed run left staged) is conductor.Flush.
package spool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// Backing is one landing the spool drains (or writes) archives to: its medium name, the WriteStore it
// lands on (the clerk Session, which the spool wraps in a routing WriteStore), the medium's byte-rate
// Limiter, and Writers — how many writes to it may run at once, direct writes and drains alike (the
// medium's `writers` cap, defaulting to its drive count for a serial medium or the worker count for
// a concurrent one; see conductor.landingWriters). Each Backing configures one runtime lane.
type Backing struct {
	Name string
	// Stores is the landing's write stores, one per concurrent writer: a single store shared by all
	// writers for a concurrent medium (disk/cloud), or one per drive for a serial multi-drive tape
	// library. Each is only written; the spool wraps it and builds writers over it.
	Stores  []archiveio.WriteStore
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

// lane is one landing's runtime state (a Backing, wired up): its per-drive stores (the WriteStores
// the spool wraps), the medium's rate limiter, and free — the lease that both bounds concurrency and
// hands each writer a store index. free is buffered to the concurrency width; each buffered value is
// a store index a write may use (a distinct drive for a multi-store tape library, or index 0 repeated
// for a shared single store). A writer (a direct write or a drain) pops an index for the duration of
// its write and pushes it back on close, so a drive is used by one archive at a time and the byte I/O
// of the others runs in parallel.
type lane struct {
	name   string
	stores []archiveio.WriteStore
	lim    *ratelimit.Limiter
	free   chan int
}

// lease pops a free store index (blocking until one frees); release returns it.
func (l *lane) lease() int      { return <-l.free }
func (l *lane) release(idx int) { l.free <- idx }

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
	closed   chan struct{} // closed by Drain; stops the cancel watcher
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
		closed: make(chan struct{}),
	}
	if sp.logf == nil {
		sp.logf = func(string, ...any) {}
	}
	for _, b := range cfg.Backings {
		writers := b.Writers
		if writers < 1 {
			writers = 1
		}
		// free holds one entry per concurrent writer. A multi-store lane (a tape drive per store)
		// hands out distinct indices 0..writers-1; a single-store lane (disk/cloud) hands out index 0
		// repeated, so its writers share the one store (their control serialises on the orchestrator).
		free := make(chan int, writers)
		for k := 0; k < writers; k++ {
			idx := k
			if idx > len(b.Stores)-1 {
				idx = len(b.Stores) - 1
			}
			free <- idx
		}
		sp.lanes[b.Name] = &lane{name: b.Name, stores: b.Stores, lim: b.Lim, free: free}
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

// Ingest returns the writer source for the named landing, which must be one of Config.Backings — an
// unknown name is a caller bug (the handle panics on first use). The dumper resolves each DLE's
// landing to one of these and pulls writers from it; the spool never picks a landing itself.
func (sp *Spool) Ingest(name string) archiveio.Ingest {
	return laneHandle{sp: sp, l: sp.lanes[name]}
}

// laneHandle is a producer's view of one landing — an Ingest bound to a lane, so NewArchive routes
// there without the spool re-deciding the landing.
type laneHandle struct {
	sp *Spool
	l  *lane
}

var _ archiveio.Ingest = laneHandle{}

func (h laneHandle) NewArchive(spec archiveio.ArchiveSpec, est int64) (*archiveio.ArchiveWriter, error) {
	return h.sp.ingest(h.l, spec.Host+":"+spec.Path, est, func(a *archiveio.Author) *archiveio.ArchiveWriter {
		return a.NewArchive(spec)
	})
}

func (h laneHandle) NewCopy(arch record.Archive, est int64) (*archiveio.ArchiveWriter, error) {
	return h.sp.ingest(h.l, arch.Host+":"+arch.Path, est, func(a *archiveio.Author) *archiveio.ArchiveWriter {
		return a.NewCopy(arch)
	})
}

// writerOver authors a concurrent writer over store: a fresh archiveio.Author whose WriteStore is
// store wrapped in a routedWriteStore, so its alloc + Record hop to the orchestrator while its byte I/O
// runs on the caller's goroutine. Cheap enough to build per write.
func (sp *Spool) writerOver(store archiveio.WriteStore, lim *ratelimit.Limiter) *archiveio.Author {
	return archiveio.NewAuthor(&routedWriteStore{real: store, orch: sp.orch}, sp.spec, lim, sp.now)
}

// landingVolume names the distinct volume label(s) an archive's parts landed on: one volume normally,
// several comma-joined when it spanned volumes or, on a multi-drive library, drives. Empty for an
// address-identified landing (no labels), which `nb status` then shows without a volume.
func landingVolume(pos record.ArchivePos) string {
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

// ingest reserves ingestion for one archive bound for lane l, identified by dleID and estimated at
// est bytes, and returns the archiveio writer to transfer it into — built by build over the leased
// store's Author (NewArchive for a dump, NewCopy for a copy/sync; only the writer differs). It blocks
// for back-pressure: a holding write waits while every fitting disk is over capacity; a direct write (no
// disk fits, or none configured) waits for a free drive on l. The writer's control calls route onto the
// orchestrator, and its Close hook releases whatever the write leased. It returns the run's error if the
// spool has aborted.
func (sp *Spool) ingest(l *lane, dleID string, est int64, build func(*archiveio.Author) *archiveio.ArchiveWriter) (*archiveio.ArchiveWriter, error) {
	if err := sp.Aborted(); err != nil {
		return nil, err
	}
	disk, direct, err := sp.pool.Acquire(est)
	if err != nil {
		return nil, err
	}
	if direct {
		// No holding disk fits: write straight to the landing, leasing one of l's drives for the write.
		store := l.lease()
		aw := build(sp.writerOver(l.stores[store], l.lim))
		aw.SetCloser(func() error {
			// Surface the landing volume(s) this drive wrote to, so `nb status` shows the
			// multi-drive spread (each DLE on its own tape). A faulted write never committed.
			if res, ok := aw.Committed(); ok && sp.tr != nil {
				sp.tr.LandVolume(res.Archive.Host+":"+res.Archive.Path, landingVolume(res.Pos))
			}
			l.release(store)
			return nil
		})
		return aw, nil
	}
	// Stage onto holding disk disk; a drain copies it to l later.
	if sp.tr != nil {
		sp.tr.MarkToHolding(dleID)
	}
	aw := build(sp.writerOver(sp.pool.Storage(disk), sp.pool.Lim(disk)))
	aw.SetCloser(func() error {
		// On commit the archive occupies the disk until its drain copies it off: charge the landed
		// bytes (so a later Acquire back-pressures on the drain backlog) and launch the drain to l. A
		// faulted transfer never committed, so there is nothing to drain — just free the estimate.
		if res, ok := aw.Committed(); ok {
			sp.pool.Charge(disk, res.Archive.Compressed)
			if sp.tr != nil {
				sp.tr.StageHolding(res.Archive.Host+":"+res.Archive.Path, sp.pool.Name(disk))
			}
			sp.drains.Add(1)
			go sp.drain(disk, res, l)
		}
		sp.pool.Release(disk, est)
		sp.pool.ReleaseWriter(disk)
		return nil
	})
	return aw, nil
}

// drain copies one staged archive from holding disk disk to lane l, then reclaims the holding copy.
// It runs on its own goroutine, holding one of l's writers for the copy so it serializes with direct
// writes and other drains to l. A failure aborts the run.
func (sp *Spool) drain(disk int, hres archiveio.CommitResult, l *lane) {
	defer sp.drains.Done()
	dleID := hres.Archive.Host + ":" + hres.Archive.Path
	if sp.tr != nil {
		sp.tr.StartFlush(dleID, sp.pool.Name(disk))
	}
	store := l.lease()
	defer l.release(store)
	if err := sp.copyOne(disk, hres, l, store, dleID); err != nil {
		sp.setAbort(err)
	}
}

// copyOne reads a staged archive off holding disk disk (on this goroutine) and streams it to l's
// leased store through a fresh copy writer — its volume rolls and its placement Record route onto the
// orchestrator, while the bytes flow here. The copy's Commit records the landing placement; then the
// holding copy is reclaimed on the orchestrator (files + placement, the catalog write) and its disk
// bytes freed.
func (sp *Spool) copyOne(disk int, hres archiveio.CommitResult, l *lane, store int, dleID string) error {
	rc, err := sp.pool.Storage(disk).OpenArchive(hres.Archive, hres.Pos)
	if err != nil {
		return fmt.Errorf("flush %s L%d: read holding disk: %w", dleID, hres.Archive.Level, err)
	}
	cw := sp.writerOver(l.stores[store], l.lim).NewCopy(hres.Archive)
	if sp.tr != nil {
		archiveio.MeterArchive(cw, func(copied int64) { sp.tr.AddDrainBytes(dleID, copied) })
	}
	// Transfer streams the raw payload and has the copy writer Commit it (footer + routed Record); it
	// closes rc.
	if _, err := xfer.Transfer(sp.ctx, xfer.Reader(rc), xfer.NewFilters(), cw); err != nil {
		return fmt.Errorf("flush %s L%d to %q: %w", dleID, hres.Archive.Level, l.name, err)
	}
	if res, ok := cw.Committed(); ok && sp.tr != nil {
		sp.tr.LandVolume(dleID, landingVolume(res.Pos)) // the landing tape the drain reached
	}
	if err := sp.orch.reclaimOn(sp.pool.Storage(disk), hres.Archive, hres.Pos); err != nil {
		return fmt.Errorf("flush %s: reclaim holding disk: %w", dleID, err)
	}
	sp.pool.Release(disk, hres.Archive.Compressed)
	if sp.tr != nil {
		sp.tr.FinishFlush(dleID)
	}
	sp.logf("flushed %s L%d to %q", dleID, hres.Archive.Level, l.name)
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
