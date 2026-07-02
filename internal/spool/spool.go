package spool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// spool.go is the consuming half of a dump: the run's concurrency-safe archive store over one or more
// landing backings plus holding disks. It models Amanda's driver/taper split, in one process:
//
//   - Each producer (and each drain) writes bytes on its own goroutine, driving an archiveio writer
//     the clerk built over a landing's (or holding disk's) Session. All byte I/O — part headers,
//     payload, footer, member index, SHA — happens there.
//   - A single orchestrator goroutine is the Coordinator every Session's control calls route through:
//     volume alloc/roll (the librarian) and the catalog Record. So across every backing, rolls and
//     placements serialize onto one goroutine (the sole catalog writer), with no bulk bytes flowing
//     through it — a slow drive can't block it.
//
// Routing is the caller's: the dumper resolves each DLE's landing and asks the spool for that
// backing's store. The spool decides per archive whether to stage it on a holding disk (then drain it
// to the landing later) or, when no disk fits or none is configured, write it straight to the landing.
// Per-backing Writers serialize a serial medium (a tape writes one archive at a time); a concurrent
// medium (cloud/disk) runs Writers writers at once. Holding back-pressure is the Pool's. A backing
// failure aborts the run so producers stop and the run fails — never dropping data — and a rerun fills
// in.

// Backing is one landing the spool drains (or writes) archives to: its medium name, the WriteStore it
// lands on (the clerk Session, which the spool wraps in a routing WriteStore), the medium's byte-rate
// Limiter, and Writers — how many writes to it may run at once, direct writes and drains alike (the
// medium's `writers` cap, defaulting to its drive count for a serial medium or the worker count for
// a concurrent one; see conductor.landingWriters).
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

// backing is one landing's lane: its per-drive stores (the WriteStores the spool wraps), the medium's
// rate limiter, and free — the lease that both bounds concurrency and hands each writer a store index.
// free is buffered to the concurrency width; each buffered value is a store index a write may use (a
// distinct drive for a multi-store tape library, or index 0 repeated for a shared single store). A
// writer (a direct write or a drain) pops an index for the duration of its write and pushes it back on
// close, so a drive is used by one archive at a time and the byte I/O of the others runs in parallel.
type backing struct {
	name   string
	stores []archiveio.WriteStore
	lim    *ratelimit.Limiter
	free   chan int
}

// lease pops a free store index (blocking until one frees); release returns it.
func (b *backing) lease() int      { return <-b.free }
func (b *backing) release(idx int) { b.free <- idx }

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

// orchestrator is the single-goroutine Coordinator (archiveio.Coordinator): it runs each routed
// control call — a librarian alloc/roll (NextPart/PlaceRecord), a catalog Record, or a holding drain's
// reclaim — to completion, serially, so those single-owner resources need no lock. It runs only these
// typed operations, never arbitrary work, and carries no bulk bytes.
type orchestrator struct {
	vol     chan volReq
	record  chan recordReq
	reclaim chan reclaimReq
	stop    chan struct{}
}

// volReq asks the orchestrator to run real's NextPart (or PlaceRecord, when place) and reply with the
// allocated volume; recordReq runs real's Record; reclaimReq runs store's Reclaim.
type volReq struct {
	real  archiveio.WriteStore
	place bool
	size  int64
	reply chan volResp
}
type volResp struct {
	vol   media.Volume
	max   int64
	name  string
	epoch int
	err   error
}
type recordReq struct {
	real  archiveio.WriteStore
	res   archiveio.CommitResult
	reply chan error
}
type reclaimReq struct {
	store archiveio.Store
	arch  record.Archive
	pos   record.ArchivePos
	reply chan error
}

func newOrchestrator() *orchestrator {
	o := &orchestrator{
		vol:     make(chan volReq),
		record:  make(chan recordReq),
		reclaim: make(chan reclaimReq),
		stop:    make(chan struct{}),
	}
	go o.loop()
	return o
}

func (o *orchestrator) loop() {
	for {
		select {
		case r := <-o.vol:
			var resp volResp
			if r.place {
				resp.max = -1
				resp.vol, resp.name, resp.epoch, resp.err = r.real.PlaceRecord(r.size)
			} else {
				resp.vol, resp.max, resp.name, resp.epoch, resp.err = r.real.NextPart()
			}
			r.reply <- resp
		case r := <-o.record:
			r.reply <- r.real.Record(r.res)
		case r := <-o.reclaim:
			r.reply <- r.store.Reclaim(r.arch, r.pos)
		case <-o.stop:
			return
		}
	}
}

func (o *orchestrator) shutdown() { close(o.stop) }

// routedWriteStore is a Session's WriteStore with its control calls hopped onto the orchestrator; the
// returned volume's AppendFile and byte writes stay on the caller's goroutine. Bounded is a constant,
// so it never crosses.
type routedWriteStore struct {
	real archiveio.WriteStore
	orch *orchestrator
}

func (r *routedWriteStore) NextPart() (media.Volume, int64, string, int, error) {
	reply := make(chan volResp, 1)
	r.orch.vol <- volReq{real: r.real, reply: reply}
	x := <-reply
	return x.vol, x.max, x.name, x.epoch, x.err
}

func (r *routedWriteStore) PlaceRecord(size int64) (media.Volume, string, int, error) {
	reply := make(chan volResp, 1)
	r.orch.vol <- volReq{real: r.real, place: true, size: size, reply: reply}
	x := <-reply
	return x.vol, x.name, x.epoch, x.err
}

func (r *routedWriteStore) Bounded() bool { return r.real.Bounded() }

func (r *routedWriteStore) Record(res archiveio.CommitResult) error {
	reply := make(chan error, 1)
	r.orch.record <- recordReq{real: r.real, res: res, reply: reply}
	return <-reply
}

// reclaimOn drops a staged archive from store on the orchestrator (Reclaim's catalog RemoveArchive is
// single-owner, like Record).
func (o *orchestrator) reclaimOn(store archiveio.Store, arch record.Archive, pos record.ArchivePos) error {
	reply := make(chan error, 1)
	o.reclaim <- reclaimReq{store: store, arch: arch, pos: pos, reply: reply}
	return <-reply
}

// Spool is the consuming side of a dump (see the file comment). Build it with New, route a producer to
// a backing with Store(name).NewArchive, and close it with Drain.
type Spool struct {
	orch     *orchestrator
	backings map[string]*backing
	pool     *Pool
	spec     archiveio.RunSpec // authors concurrent writers with the run id
	now      func() time.Time
	tr       *progress.Tracker
	logf     func(format string, args ...any)
	ctx      context.Context

	drains sync.WaitGroup // in-flight holding->landing drains

	mu       sync.Mutex
	abortErr error
	closed   chan struct{} // closed by Drain; stops the cancel watcher
}

// New builds a Spool from cfg and starts its orchestrator — the single goroutine every write's control
// calls (alloc + Record) route onto, so rolls and placements serialize there across all backings.
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
		orch:     newOrchestrator(),
		backings: make(map[string]*backing, len(cfg.Backings)),
		pool:     cfg.Holding,
		spec:     cfg.Spec,
		now:      now,
		tr:       cfg.Tracker,
		logf:     cfg.Logf,
		ctx:      ctx,
		closed:   make(chan struct{}),
	}
	if sp.logf == nil {
		sp.logf = func(string, ...any) {}
	}
	for _, b := range cfg.Backings {
		writers := b.Writers
		if writers < 1 {
			writers = 1
		}
		// free holds one entry per concurrent writer. A multi-store backing (a tape drive per store)
		// hands out distinct indices 0..writers-1; a single-store backing (disk/cloud) hands out index 0
		// repeated, so its writers share the one store (their control serialises on the orchestrator).
		free := make(chan int, writers)
		for k := 0; k < writers; k++ {
			idx := k
			if idx > len(b.Stores)-1 {
				idx = len(b.Stores) - 1
			}
			free <- idx
		}
		sp.backings[b.Name] = &backing{name: b.Name, stores: b.Stores, lim: b.Lim, free: free}
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

// Ingest returns the writer source for the named landing. The dumper resolves each DLE's landing to
// one of these and pulls writers from it; the spool never picks a landing itself.
func (sp *Spool) Ingest(name string) archiveio.Ingest {
	return backingHandle{sp: sp, b: sp.backings[name]}
}

// backingHandle is a producer's view of one landing — an Ingest bound to a backing, so NewArchive
// routes there without the spool re-deciding the landing.
type backingHandle struct {
	sp *Spool
	b  *backing
}

var _ archiveio.Ingest = backingHandle{}

func (h backingHandle) NewArchive(spec archiveio.ArchiveSpec, est int64) (*archiveio.ArchiveWriter, error) {
	return h.sp.ingest(h.b, spec.Host+":"+spec.Path, est, func(a *archiveio.Author) *archiveio.ArchiveWriter {
		return a.NewArchive(spec)
	})
}

func (h backingHandle) NewCopy(arch record.Archive, est int64) (*archiveio.ArchiveWriter, error) {
	return h.sp.ingest(h.b, arch.Host+":"+arch.Path, est, func(a *archiveio.Author) *archiveio.ArchiveWriter {
		return a.NewCopy(arch)
	})
}

// writerOver authors a concurrent writer over store: a fresh archiveio.Author whose WriteStore is
// store wrapped in a routedWriteStore, so its alloc + Record hop to the orchestrator while its byte I/O
// runs on the caller's goroutine. Cheap enough to build per write.
func (sp *Spool) writerOver(store archiveio.WriteStore, lim *ratelimit.Limiter) *archiveio.Author {
	return archiveio.NewAuthor(&routedWriteStore{real: store, orch: sp.orch}, sp.spec, lim, sp.now)
}

// ingest reserves ingestion for one archive bound for backing b, identified by dleID and estimated at
// est bytes, and returns the archiveio writer to transfer it into — built by build over the leased
// store's Author (NewArchive for a dump, NewCopy for a copy/sync; only the writer differs). It blocks
// for back-pressure: a holding write waits while every fitting disk is over capacity; a direct write (no
// disk fits, or none configured) waits for a free drive on b. The writer's control calls route onto the
// orchestrator, and its Close hook releases whatever the write leased. It returns the run's error if the
// spool has aborted.
func (sp *Spool) ingest(b *backing, dleID string, est int64, build func(*archiveio.Author) *archiveio.ArchiveWriter) (*archiveio.ArchiveWriter, error) {
	if err := sp.Aborted(); err != nil {
		return nil, err
	}
	idx, direct, err := sp.pool.Acquire(est)
	if err != nil {
		return nil, err
	}
	if direct {
		// No holding disk fits: write straight to the landing, leasing one of b's drives for the write.
		i := b.lease()
		aw := build(sp.writerOver(b.stores[i], b.lim))
		aw.SetCloser(func() error {
			// Surface the landing volume(s) this drive wrote to, so `nb status` shows the
			// multi-drive spread (each DLE on its own tape). A faulted write never committed.
			if res, ok := aw.Committed(); ok && sp.tr != nil {
				sp.tr.LandVolume(res.Archive.Host+":"+res.Archive.Path, landingVolume(res.Pos))
			}
			b.release(i)
			return nil
		})
		return aw, nil
	}
	// Stage onto holding disk idx; a drain copies it to b later.
	if sp.tr != nil {
		sp.tr.MarkToHolding(dleID)
	}
	aw := build(sp.writerOver(sp.pool.Storage(idx), sp.pool.Lim(idx)))
	aw.SetCloser(func() error {
		// On commit the archive occupies the disk until its drain copies it off: charge the landed
		// bytes (so a later Acquire back-pressures on the drain backlog) and launch the drain to b. A
		// faulted transfer never committed, so there is nothing to drain — just free the estimate.
		if res, ok := aw.Committed(); ok {
			sp.pool.Charge(idx, res.Archive.Compressed)
			if sp.tr != nil {
				sp.tr.StageHolding(res.Archive.Host+":"+res.Archive.Path, sp.pool.Name(idx))
			}
			sp.drains.Add(1)
			go sp.drain(idx, res, b)
		}
		sp.pool.Release(idx, est)
		sp.pool.ReleaseWriter(idx)
		return nil
	})
	return aw, nil
}

// drain copies one staged archive from holding disk idx to backing b, then reclaims the holding copy.
// It runs on its own goroutine, holding one of b's writers for the copy so it serializes with direct
// writes and other drains to b. A failure aborts the run.
func (sp *Spool) drain(idx int, hres archiveio.CommitResult, b *backing) {
	defer sp.drains.Done()
	dleID := hres.Archive.Host + ":" + hres.Archive.Path
	if sp.tr != nil {
		sp.tr.StartFlush(dleID, sp.pool.Name(idx))
	}
	i := b.lease()
	defer b.release(i)
	if err := sp.copyOne(idx, hres, b, i, dleID); err != nil {
		sp.setAbort(err)
	}
}

// copyOne reads a staged archive off its holding disk (on this goroutine) and streams it to b through
// a fresh copy writer — its volume rolls and its placement Record route onto the orchestrator, while
// the bytes flow here. The copy's Commit records the landing placement; then the holding copy is
// reclaimed on the orchestrator (files + placement, the catalog write) and its disk bytes freed.
func (sp *Spool) copyOne(idx int, hres archiveio.CommitResult, b *backing, store int, dleID string) error {
	rc, err := sp.pool.Storage(idx).OpenArchive(hres.Archive, hres.Pos)
	if err != nil {
		return fmt.Errorf("flush %s L%d: read holding disk: %w", dleID, hres.Archive.Level, err)
	}
	cw := sp.writerOver(b.stores[store], b.lim).NewCopy(hres.Archive)
	if sp.tr != nil {
		archiveio.MeterArchive(cw, func(copied int64) { sp.tr.AddDrainBytes(dleID, copied) })
	}
	// Transfer streams the raw payload and has the copy writer Commit it (footer + routed Record); it
	// closes rc.
	if _, err := xfer.Transfer(sp.ctx, xfer.Reader(rc), xfer.NewFilters(), cw); err != nil {
		return fmt.Errorf("flush %s L%d to %q: %w", dleID, hres.Archive.Level, b.name, err)
	}
	if res, ok := cw.Committed(); ok && sp.tr != nil {
		sp.tr.LandVolume(dleID, landingVolume(res.Pos)) // the landing tape the drain reached
	}
	if err := sp.orch.reclaimOn(sp.pool.Storage(idx), hres.Archive, hres.Pos); err != nil {
		return fmt.Errorf("flush %s: reclaim holding disk: %w", dleID, err)
	}
	sp.pool.Release(idx, hres.Archive.Compressed)
	if sp.tr != nil {
		sp.tr.FinishFlush(dleID)
	}
	sp.logf("flushed %s L%d to %q", dleID, hres.Archive.Level, b.name)
	return nil
}

// Drain signals the producers are done and waits for every queued holding->landing drain to finish,
// then stops the orchestrator, returning the run's error if any drain (or a backing) failed. There is
// no run object to return — each backing's committed archives are already in the catalog (recorded as they
// committed), so the run is read from there.
func (sp *Spool) Drain() error {
	sp.drains.Wait()
	close(sp.closed)
	sp.orch.shutdown()
	return sp.Aborted()
}

// Aborted returns the run's error once a drain or backing has failed (so producers stop scheduling), or
// nil while healthy.
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
