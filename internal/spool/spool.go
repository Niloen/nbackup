package spool

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// spool.go is the consuming half of a dump: the run's buffered, concurrency-safe archive store over
// one or more landing backings plus holding disks. A producer ingests each archive into a Sink the
// Spool hands out for the archive's landing (Store(landing).NewArchive → transfer → commit); the Spool
// decides per archive whether to buffer it on a holding disk (then copy it to that landing later) or —
// when no disk fits, or none is configured — write it straight to the landing.
//
// Routing is the caller's: the dumper resolves each DLE's landing (its dumptype's `landing` override,
// else the config-wide one) and asks the Spool for that backing's store. The Spool never picks a
// landing itself; it just owns the concurrency. The sink it hands a producer is a remoteSink: its
// NextPart and Commit are control calls that run on the orchestrator goroutine (the librarian that may
// roll/change volumes and the sole catalog writer), while the payload bytes the producer writes into
// each part stay on the producer's goroutine. So every volume roll and every placement record (which
// an ArchiveWriter's Commit does) lands single-threaded on the orchestrator, with no bytes flowing
// through it — across all backings.
//
// Concurrency, all private here:
//   - the orchestrator goroutine (orchestrate): the sole catalog writer. It runs each remoteSink's
//     NextPart/Commit (so rolls + placement records serialize across every backing), dispatches each
//     backing's holding→landing copies copies-first, grants its backing permits, and finalizes drains.
//     Per-backing state (permits, pending copies/permits) lives on a *backing value object the
//     orchestrator owns exclusively — no map lookups on the hot path, every request carries its *backing.
//   - a copy goroutine per dispatched drain (copyWorker): streams one holding→landing copy — itself a
//     remoteSink write — while the orchestrator stays free to serve its control calls. Up to a backing's
//     Slots copies run at once, so independent landings (a tape and a cloud) drain in parallel.
//   - the producer's own goroutines (external): a direct write runs on the producer goroutine that
//     holds its backing's permit, routing its control calls to the orchestrator.
//
// A backing is a single serial writer when its medium writes serially — a single rolling drive (tape),
// where a copy and a direct write must not interleave parts — modelled as a permit of Slots: 1 when
// buffering or for a serial medium, else the worker count (a concurrent-write object store/disk admits
// parallel writes even when it splits archives into parts). The caller sets Slots; the spool just
// honors it. Holding back-pressure is the Pool's (shared across backings); a backing failure aborts the
// run so the producers stop and the run fails — never dropping data — and a rerun fills in.

// Backing is one landing the spool drains (or writes) archives to: its medium name, the ArchiveStore
// authoring it, and Slots — how many writes to it may run at once (1 while buffering or for a serial
// single-drive medium, else the worker count).
type Backing struct {
	Name    string
	Storage archiveio.ArchiveStore
	Slots   int
}

// Config is what the engine wires a Spool from: the Backings it drains to (the run's landings), the
// Holding disks (empty = never buffer), and the run's progress + log seams.
type Config struct {
	Backings []Backing
	Holding  *Pool // holding disks; empty = no buffering (every archive goes direct)
	Tracker  *progress.Tracker
	Logf     func(format string, args ...any)
}

// backing is one landing's lane state, owned EXCLUSIVELY by the orchestrate goroutine — no mutex,
// because only that goroutine reads or writes these fields. Per-backing isolation is structural: each
// landing is its own object, not a key in a map, and every routed request carries a *backing so the
// orchestrator does b.inUse++ rather than a name lookup.
type backing struct {
	name       string
	store      archiveio.ArchiveStore
	slots      int
	inUse      int         // writes in flight to this backing (direct writes + drain copies)
	pendCopy   []handoff   // staged archives waiting to drain to this backing
	pendPermit []permitReq // direct writers waiting for a permit on this backing
}

// Spool is the consuming side of a dump — the run's archive store over its backings (see the file
// comment). Build it with New, route a producer to a backing with Store(name).NewArchive + transfer/
// commit, and close it with Drain.
type Spool struct {
	backings map[string]*backing
	pool     *Pool
	tr       *progress.Tracker
	logf     func(format string, args ...any)
	ctx      context.Context

	reqCh       chan sinkReq   // a remoteSink's NextPart/Commit, served on the orchestrator
	permitReqCh chan permitReq // a direct write waiting for its backing's permit
	releaseCh   chan release   // a direct write returning its backing permit (remoteSink.Close)
	copyDoneCh  chan copyResult
	shutdownCh  chan struct{} // closed by Drain: no more producers
	finished    chan struct{} // closed when the orchestrator goroutine exits

	mu       sync.Mutex
	abortErr error
}

// New builds a Spool from cfg and starts its orchestrator goroutine. A producer may call
// Store(name).NewArchive concurrently; Drain stops it. Canceling ctx aborts the run: the orchestrator
// stops dispatching queued holding→landing copies (the leftovers flush on the next run) so a canceled
// dump does not block flushing everything it had buffered — it governs the lifetime of the goroutines
// New starts, which is why it is a parameter here rather than in Config.
func New(ctx context.Context, cfg Config) *Spool {
	d := &Spool{
		ctx:         ctx,
		backings:    make(map[string]*backing, len(cfg.Backings)),
		pool:        cfg.Holding,
		tr:          cfg.Tracker,
		logf:        cfg.Logf,
		reqCh:       make(chan sinkReq),
		permitReqCh: make(chan permitReq),
		releaseCh:   make(chan release),
		copyDoneCh:  make(chan copyResult),
		shutdownCh:  make(chan struct{}),
		finished:    make(chan struct{}),
	}
	for _, b := range cfg.Backings {
		d.backings[b.Name] = &backing{name: b.Name, store: b.Storage, slots: b.Slots}
	}
	if d.logf == nil {
		d.logf = func(string, ...any) {}
	}
	if d.ctx == nil {
		d.ctx = context.Background()
	}
	go d.orchestrate()
	return d
}

// Store returns the ArchiveWriteStore that routes archives to the named backing. The dumper resolves
// each DLE's landing to one of these; the spool never picks a backing itself.
func (d *Spool) Store(name string) archiveio.ArchiveWriteStore {
	return backingHandle{d: d, b: d.backings[name]}
}

// backingHandle is a producer's view of one landing — an archiveio.ArchiveWriteStore bound to a
// *backing, so NewArchive routes there without the spool re-deciding the landing.
type backingHandle struct {
	d *Spool
	b *backing
}

var _ archiveio.ArchiveWriteStore = backingHandle{}

func (h backingHandle) NewArchive(spec archiveio.ArchiveSpec, est int64) (archiveio.ArchiveWriter, error) {
	return h.d.newArchive(h.b, spec, est)
}

// Aborted reports the run's failure so the producer stops scheduling. It is spool-wide: a backing
// failure fails the run (a rerun fills in), so every backing's store reports the same abort.
func (h backingHandle) Aborted() error { return h.d.Aborted() }

// newArchive reserves ingestion for an archive bound for backing b, estimated at est bytes, and
// returns the ArchiveWriter to transfer it into — a remoteSink over the chosen medium's ArchiveWriter.
// It blocks for back-pressure: a holding write waits while every fitting disk is over capacity; a
// direct write (no disk fits, or none is configured) waits for a free permit on b. It returns the
// run's error if the spool has aborted.
func (d *Spool) newArchive(b *backing, spec archiveio.ArchiveSpec, est int64) (archiveio.ArchiveWriter, error) {
	idx, direct, err := d.pool.Acquire(est)
	if err != nil {
		return nil, err
	}
	if !direct {
		if d.tr != nil {
			// The dump is bound for this holding disk: mark it staging now, before any bytes commit, so
			// live status shows it staging to holding (not yet on the volume) rather than mistaking the
			// in-flight dump for a direct write to the landing.
			d.tr.MarkToHolding(spec.Host + ":" + spec.Path)
		}
		real, err := d.pool.Storage(idx).NewArchive(spec, est)
		if err != nil {
			return nil, err
		}
		return &remoteSink{d: d, b: b, real: real, kind: writeHolding, disk: idx, est: est}, nil
	}
	reply := make(chan error, 1)
	d.permitReqCh <- permitReq{b: b, reply: reply}
	if err := <-reply; err != nil {
		return nil, err
	}
	real, err := b.store.NewArchive(spec, est)
	if err != nil {
		return nil, err
	}
	return &remoteSink{d: d, b: b, real: real, kind: writeDirect}, nil
}

// writeKind tells the orchestrator how to follow up a remoteSink's Commit, after the ArchiveWriter has
// recorded the placement.
type writeKind int

const (
	writeHolding writeKind = iota // staged on a holding disk: queue a holding→landing copy
	writeDirect                   // straight to the landing: holds a backing permit (released on Close)
	writeCopy                     // the drain's copy to the landing: nothing (finalizeDrain reclaims)
)

// remoteSink is a producer's (or a copy goroutine's) xfer.Sink whose NextPart/Commit run on the
// orchestrator — the librarian (which may roll/change volumes) and the sole catalog writer. The
// payload bytes the caller writes into the part writer NextPart returns stay on the caller's
// goroutine; only the control calls cross to the orchestrator. real is the medium's per-archive
// ArchiveWriter the orchestrator runs the calls on; b is the landing it writes to.
type remoteSink struct {
	d    *Spool
	b    *backing
	real archiveio.ArchiveWriter
	kind writeKind
	disk int   // holding disk index (writeHolding)
	est  int64 // in-flight reservation Acquire took on disk (writeHolding); the producer frees it on Close
}

func (r *remoteSink) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	reply := make(chan sinkResp, 1)
	r.d.reqCh <- sinkReq{b: r.b, sink: r.real, ctx: ctx, reply: reply}
	s := <-reply
	return s.w, s.max, s.err
}

func (r *remoteSink) Commit(ctx context.Context, p xfer.SourceStats) error {
	reply := make(chan sinkResp, 1)
	r.d.reqCh <- sinkReq{b: r.b, sink: r.real, commit: true, ctx: ctx, produced: p, kind: r.kind, disk: r.disk, reply: reply}
	return (<-reply).err
}

// Result delegates to the medium's writer so a remoteSink satisfies archiveio.ArchiveWriter. The producer
// driving it never reads the position (the orchestrator records the placement inside Commit); it is
// the orchestrator that reads Result, off the real writer, when it queues a holding→landing copy.
func (r *remoteSink) Result() (record.Archive, record.ArchivePos) { return r.real.Result() }

// Close releases what this archive held while it was being written, on every path — whether the
// transfer committed or faulted before commit. A direct write holds its backing's permit; return it to
// the orchestrator here (not in Commit) so a faulted direct write — which never reaches Commit —
// frees its slot too, instead of leaking it and stalling the next direct writer forever. A holding
// write holds the in-flight disk reservation Acquire took; free it here. If the dump committed, its
// landed bytes were charged separately (on the Commit follow-up) and the drain frees those, so this
// release is unconditional and never double-frees. The producer defers Close right after NewArchive,
// so it runs on every path. The orchestrator is always serving when Close runs (every Close happens
// before Drain), so the synchronous hand-off cannot block on a stopped server.
func (r *remoteSink) Close() error {
	err := r.real.Close()
	switch r.kind {
	case writeDirect:
		ack := make(chan struct{}, 1)
		r.d.releaseCh <- release{b: r.b, ack: ack}
		<-ack
	case writeHolding:
		r.d.pool.Release(r.disk, r.est)
	}
	return err
}

// sinkReq is a routed remoteSink call served on the orchestrator: commit selects Commit(produced)
// over NextPart, kind/disk carry the follow-up bookkeeping (only meaningful for a commit), b names the
// landing it writes to.
type sinkReq struct {
	b        *backing
	sink     archiveio.ArchiveWriter
	commit   bool
	ctx      context.Context
	produced xfer.SourceStats
	kind     writeKind
	disk     int
	reply    chan sinkResp
}

// sinkResp is the orchestrator's reply: NextPart's writer+cap, or just the error for a commit.
type sinkResp struct {
	w   io.WriteCloser
	max int64
	err error
}

// Aborted returns the run's error once the drain has failed (so producers can stop scheduling), or
// nil while healthy.
func (d *Spool) Aborted() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.abortErr
}

// Drain signals that the producers are done and waits for the orchestrator to flush every queued
// holding→landing copy across every backing, returning the run's error if the drain failed. There is
// no slot to return — each backing's committed archives are already in the catalog (recorded as they
// committed), so the run's slot is read from there.
func (d *Spool) Drain() error {
	close(d.shutdownCh)
	<-d.finished
	return d.Aborted()
}

func (d *Spool) setAbort(err error) {
	d.mu.Lock()
	if d.abortErr == nil {
		d.abortErr = err
	}
	d.mu.Unlock()
	d.pool.Abort(err) // wake producers blocked on holding back-pressure
}

// orchestrate is the server and the sole catalog writer across every backing. Its select multiplexes
// the producers' (and the copies') routed sink calls, the permit waiters and releases, and the copy
// goroutines so control never blocks on a byte stream; after each event it dispatches each backing's
// pending work copies-first (draining the disks keeps the Pool from stalling producers), up to that
// backing's Slots. A direct write takes a permit when granted and returns it on Close (committed or
// faulted), so a failed dump never leaks its slot. On error it aborts every backing and replies the
// error to every waiter, then drains the in-flight copies and exits once Drain has signalled no more
// producers.
func (d *Spool) orchestrate() {
	defer close(d.finished)

	var (
		failErr        error
		copiesInFlight int
		shutting       bool
	)
	shutdownCh := d.shutdownCh
	cancelCh := d.ctx.Done()
	pendingCopies := func() int {
		n := 0
		for _, b := range d.backings {
			n += len(b.pendCopy)
		}
		return n
	}
	for {
		if shutting && copiesInFlight == 0 && pendingCopies() == 0 {
			return
		}
		select {
		case <-cancelCh:
			// The run was canceled: abort like a backing failure so queued copies are dropped
			// (they flush on the next run) and every waiter is released, instead of flushing
			// everything buffered. In-flight copies are left to finish; Drain then joins us.
			if failErr == nil {
				failErr = context.Canceled
			}
			cancelCh = nil // a closed Done channel is always ready; stop selecting it
		case req := <-d.reqCh:
			if !req.commit {
				if failErr != nil {
					req.reply <- sinkResp{err: failErr}
					break
				}
				w, max, err := req.sink.NextPart(req.ctx)
				req.reply <- sinkResp{w: w, max: max, err: err}
				break
			}
			if failErr != nil {
				req.reply <- sinkResp{err: failErr}
				break
			}
			if err := req.sink.Commit(req.ctx, req.produced); err != nil {
				failErr = err
				req.reply <- sinkResp{err: err}
				break
			}
			if req.kind == writeHolding {
				arch, pos := req.sink.Result()
				// The staged archive now occupies the disk until the drain copies it off; charge its
				// landed bytes so a later Acquire back-pressures on the drain backlog. The dump's
				// in-flight estimate is still reserved until the producer closes the sink — the two
				// briefly overlap, which only over-reserves.
				d.pool.Charge(req.disk, arch.Compressed)
				if d.tr != nil {
					// The dump committed to this disk: record where it staged so the queued DLE shows a
					// 0% flush bar (and which disk) while it waits behind other drains — taking over from
					// the "staging" mark its in-flight dump carried.
					d.tr.StageHolding(arch.Host+":"+arch.Path, d.pool.Name(req.disk))
				}
				req.b.pendCopy = append(req.b.pendCopy, handoff{arch: arch, pos: pos, disk: req.disk, b: req.b})
			}
			req.reply <- sinkResp{}
		case pr := <-d.permitReqCh:
			if failErr != nil {
				pr.reply <- failErr
				break
			}
			pr.b.pendPermit = append(pr.b.pendPermit, pr)
		case rel := <-d.releaseCh:
			// A direct write finished (remoteSink.Close) — committed or faulted before commit — and
			// returns its backing permit. Releasing here, not in the Commit handler, is what frees a
			// faulted direct write's slot; the dispatch below then grants a waiting writer.
			rel.b.inUse--
			rel.ack <- struct{}{}
		case res := <-d.copyDoneCh:
			copiesInFlight--
			res.it.b.inUse--
			if res.err != nil {
				failErr = res.err
			} else if err := d.finalizeDrain(res.it); err != nil {
				failErr = err
			}
		case <-shutdownCh:
			shutting = true
			shutdownCh = nil // a closed channel is always ready; stop selecting it
		}

		if failErr != nil {
			d.setAbort(failErr)
			for _, b := range d.backings {
				for _, pr := range b.pendPermit {
					pr.reply <- failErr
				}
				b.pendPermit = nil
				b.pendCopy = nil // a failed run leaves the holding placements for the next run's flush
			}
			continue
		}

		// Dispatch every backing, copies-first: a copy drains the holding backlog (keeping the Pool from
		// stalling producers); otherwise hand the free permits to waiting direct writers. Independent
		// backings dispatch independently — a tape at Slots 1 and a cloud at Slots N do not gate each other.
		for _, b := range d.backings {
			copiesInFlight += d.dispatch(b)
		}
	}
}

// dispatch grants backing b's free permits — copies first, then waiting direct writers — up to b.slots,
// returning how many copies it launched (so the orchestrator tracks them for shutdown). It runs only on
// the orchestrator goroutine, on b's own fields.
func (d *Spool) dispatch(b *backing) int {
	launched := 0
	for b.inUse < b.slots {
		switch {
		case len(b.pendCopy) > 0:
			j := b.pendCopy[0]
			b.pendCopy = b.pendCopy[1:]
			b.inUse++
			launched++
			go d.copyWorker(j)
		case len(b.pendPermit) > 0:
			pr := b.pendPermit[0]
			b.pendPermit = b.pendPermit[1:]
			b.inUse++
			pr.reply <- nil
		default:
			return launched
		}
	}
	return launched
}

// copyWorker streams one dispatched copy on its own goroutine, reporting the result back to the
// orchestrator. Up to a backing's Slots run at once; different backings run concurrently.
func (d *Spool) copyWorker(j handoff) {
	d.copyDoneCh <- copyResult{it: j, err: d.copyOne(j)}
}

// copyOne reads one staged archive from its holding disk and streams it to its landing through a
// remoteSink — so its volume rolls and its placement record land on the orchestrator, while the bytes
// flow on this copy goroutine. The landing's NewCopy writer re-checksums the payload against the
// recorded digest (catching holding-disk corruption) and preserves the source's identity. The
// placement record and the holding reclaim are the orchestrator's (the landing commit records the
// former; finalizeDrain does the latter).
func (d *Spool) copyOne(j handoff) error {
	holding := d.pool.Name(j.disk)
	dleID := j.arch.Host + ":" + j.arch.Path
	if d.tr != nil {
		d.tr.StartFlush(dleID, holding)
	}
	rc, err := d.pool.Storage(j.disk).OpenArchive(j.arch, j.pos)
	if err != nil {
		return fmt.Errorf("flush %s L%d: read holding disk: %w", dleID, j.arch.Level, err)
	}
	real, err := j.b.store.NewCopy(j.arch)
	if err != nil {
		return fmt.Errorf("flush %s L%d to %q: %w", dleID, j.arch.Level, j.b.name, err)
	}
	if d.tr != nil {
		real = archiveio.MeterArchive(real, func(copied int64) { d.tr.AddDrainBytes(dleID, copied) })
	}
	rs := &remoteSink{d: d, b: j.b, real: real, kind: writeCopy}
	if _, err := xfer.Transfer(d.ctx, xfer.Reader(rc), xfer.Filters{}, rs); err != nil {
		return fmt.Errorf("flush %s L%d to %q: %w", dleID, j.arch.Level, j.b.name, err)
	}
	return nil
}

// finalizeDrain reclaims the holding copy (files + placement, via the holding ArchiveStore) and
// releases its back-pressure once the archive has landed on its backing. It runs on the orchestrator.
// The landing placement was already recorded by the copy's commit, so the archive is never absent
// from the catalog.
func (d *Spool) finalizeDrain(it handoff) error {
	dleID := it.arch.Host + ":" + it.arch.Path
	if err := d.pool.Storage(it.disk).Reclaim(it.arch, it.pos); err != nil {
		return fmt.Errorf("flush %s: reclaim holding disk: %w", dleID, err)
	}
	d.pool.Release(it.disk, it.arch.Compressed)
	if d.tr != nil {
		d.tr.FinishFlush(dleID)
	}
	d.logf("flushed %s L%d to %q", dleID, it.arch.Level, it.b.name)
	return nil
}

// handoff is one committed holding archive the orchestrator queues to copy: its metadata, its
// positions on the holding disk, which disk it landed on (so the copy reads, reclaims, and releases
// the right one), and which backing it drains to.
type handoff struct {
	arch record.Archive
	pos  record.ArchivePos
	disk int
	b    *backing
}

// permitReq is a direct write waiting for backing b's permit; reply is sent the run's error or nil
// once granted.
type permitReq struct {
	b     *backing
	reply chan error
}

// release is a direct write returning backing b's permit on Close; ack is closed-back once the
// orchestrator has decremented the count, so the producer's Close blocks until the slot is free.
type release struct {
	b   *backing
	ack chan struct{}
}

// copyResult is one finished (or failed) copy a copy goroutine hands back to the orchestrator, which
// reclaims the holding copy on success (finalizeDrain).
type copyResult struct {
	it  handoff
	err error
}
