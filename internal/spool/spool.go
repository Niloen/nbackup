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

// spool.go is the consuming half of a dump: the run's buffered, concurrency-safe archive store — an
// archiveio.ArchiveWriteStore over a backing archiveio.ArchiveStore plus holding ones. A producer
// ingests each archive into a Sink the Spool hands out (NewArchive → transfer → commit); the Spool
// decides per archive whether to buffer it on a holding disk (then copy it to the authoritative
// backing later) or — when no disk fits, or none is configured — write it straight to the backing.
//
// The Spool depends only on archiveio.ArchiveStore (one backing, an array of holding) — never on the
// clerk or the catalog. The sink it hands a producer is a RemoteSink: its NextPart and Commit are
// control calls that run on the orchestrator goroutine (the librarian that may roll/change volumes and
// the sole catalog writer), while the payload bytes the producer writes into each part stay on the
// producer's goroutine. So every volume roll and every placement record (which an ArchiveWriter's
// Commit does) lands single-threaded on the orchestrator, with no bytes flowing through it.
//
// Three actors, all private here:
//   - the orchestrator goroutine (orchestrate): the server. It runs each RemoteSink's NextPart/Commit
//     (so rolls + placement records serialize), dispatches holding→backing copies copies-first,
//     grants the backing permit, and finalizes each drain.
//   - the copy goroutine (drainLoop): streams one holding→backing copy at a time (copyOne) — itself a
//     RemoteSink write — while the orchestrator stays free to serve its control calls.
//   - the producer's own goroutines (external): a direct write runs on the producer goroutine that
//     holds the backing permit, routing its control calls to the orchestrator.
//
// The backing is a single serial writer when it can span volumes (a copy and a direct write must not
// interleave parts), modelled as a permit of Slots: 1 when buffering or spanning, else the worker
// count. Holding back-pressure is the Pool's; a backing failure aborts the Pool so the producers stop
// and the run fails — never dropping data.

// Config is what the engine wires a Spool from: the Backing medium it drains to, the Holding disks
// (empty = never buffer), and the run's progress + log seams.
type Config struct {
	Backing Backing
	Holding *Pool // holding disks; empty = no buffering (every archive goes direct)
	Tracker *progress.Tracker
	Logf    func(format string, args ...any)
}

// Backing is the authoritative store the drain copies (or writes) archives to: its medium name, the
// ArchiveStore authoring it, and Slots — how many backing writes may run at once (1 while buffering or
// spanning, else the worker count).
type Backing struct {
	Name    string
	Storage archiveio.ArchiveStore
	Slots   int
}

// Spool is a concurrency-safe archiveio.ArchiveWriteStore.
var _ archiveio.ArchiveWriteStore = (*Spool)(nil)

// Spool is the consuming side of a dump — the run's archive store (see the file comment). Build it
// with New, drive it from the producer with NewArchive + transfer/commit, and close it with Drain.
type Spool struct {
	backing      archiveio.ArchiveStore
	backingName  string
	pool         *Pool
	backingSlots int
	tr           *progress.Tracker
	logf         func(format string, args ...any)

	reqCh       chan sinkReq   // a RemoteSink's NextPart/Commit, served on the orchestrator
	permitReqCh chan permitReq // a direct write waiting for the backing permit
	workCh      chan handoff   // copies dispatched to the copy goroutine
	copyDoneCh  chan copyResult
	shutdownCh  chan struct{} // closed by Finish: no more producers
	finished    chan struct{} // closed when the orchestrator goroutine exits

	mu       sync.Mutex
	abortErr error
}

// New builds a Spool from cfg and starts its orchestrator and copy goroutines. The producer may call
// Create concurrently; Finish stops it.
func New(cfg Config) *Spool {
	d := &Spool{
		backing:      cfg.Backing.Storage,
		backingName:  cfg.Backing.Name,
		pool:         cfg.Holding,
		backingSlots: cfg.Backing.Slots,
		tr:           cfg.Tracker,
		logf:         cfg.Logf,
		reqCh:        make(chan sinkReq),
		permitReqCh:  make(chan permitReq),
		workCh:       make(chan handoff),
		copyDoneCh:   make(chan copyResult),
		shutdownCh:   make(chan struct{}),
		finished:     make(chan struct{}),
	}
	if d.logf == nil {
		d.logf = func(string, ...any) {}
	}
	go d.orchestrate()
	go d.drainLoop()
	return d
}

// NewArchive reserves ingestion for an archive described by spec, estimated at est bytes, and returns
// the ArchiveWriter to transfer it into — a RemoteSink over the chosen medium's ArchiveWriter. It
// blocks for back-pressure: a holding write waits while every fitting disk is over capacity; a direct
// write (no disk fits, or none is configured) waits for a free backing permit. prog receives the
// running compressed (landed) byte count. It returns the run's error if the spool has aborted. It
// makes Spool an archiveio.ArchiveWriteStore.
func (d *Spool) NewArchive(spec archiveio.ArchiveSpec, est int64, prog func(int64)) (archiveio.ArchiveWriter, error) {
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
		real, err := d.pool.Storage(idx).NewArchive(spec, est, prog)
		if err != nil {
			return nil, err
		}
		return &remoteSink{d: d, real: real, kind: writeHolding, disk: idx}, nil
	}
	reply := make(chan error, 1)
	d.permitReqCh <- permitReq{reply: reply}
	if err := <-reply; err != nil {
		return nil, err
	}
	real, err := d.backing.NewArchive(spec, est, prog)
	if err != nil {
		return nil, err
	}
	return &remoteSink{d: d, real: real, kind: writeDirect}, nil
}

// writeKind tells the orchestrator how to follow up a RemoteSink's Commit, after the ArchiveWriter has
// recorded the placement.
type writeKind int

const (
	writeHolding writeKind = iota // staged on a holding disk: queue a holding→backing copy
	writeDirect                   // straight to the backing: release the backing permit
	writeCopy                     // the drain's copy to the backing: nothing (finalizeDrain reclaims)
)

// remoteSink is a producer's (or the copy goroutine's) xfer.Sink whose NextPart/Commit run on the
// orchestrator — the librarian (which may roll/change volumes) and the sole catalog writer. The
// payload bytes the caller writes into the part writer NextPart returns stay on the caller's
// goroutine; only the control calls cross to the orchestrator. real is the medium's per-archive
// ArchiveWriter the orchestrator runs the calls on.
type remoteSink struct {
	d    *Spool
	real archiveio.ArchiveWriter
	kind writeKind
	disk int // holding disk index (writeHolding)
}

func (r *remoteSink) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	reply := make(chan sinkResp, 1)
	r.d.reqCh <- sinkReq{sink: r.real, ctx: ctx, reply: reply}
	s := <-reply
	return s.w, s.max, s.err
}

func (r *remoteSink) Commit(ctx context.Context, p xfer.SourceStats) error {
	reply := make(chan sinkResp, 1)
	r.d.reqCh <- sinkReq{sink: r.real, commit: true, ctx: ctx, produced: p, kind: r.kind, disk: r.disk, reply: reply}
	return (<-reply).err
}

// Result delegates to the medium's writer so a remoteSink satisfies archiveio.ArchiveWriter. The producer
// driving it never reads the position (the orchestrator records the placement inside Commit); it is
// the orchestrator that reads Result, off the real writer, when it queues a holding→backing copy.
func (r *remoteSink) Result() (record.Archive, record.ArchivePos) { return r.real.Result() }

// sinkReq is a routed RemoteSink call served on the orchestrator: commit selects Commit(produced)
// over NextPart, kind/disk carry the follow-up bookkeeping (only meaningful for a commit).
type sinkReq struct {
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
// holding->backing copy, returning the run's error if the drain failed. There is no slot to return —
// the backing's committed archives are already in the catalog (each recorded as it committed), so the
// run's slot is read from there.
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

// orchestrate is the server and the sole catalog writer. Its select multiplexes the producers' (and
// the copy's) routed sink calls, the permit waiters, and the copy goroutine so control never blocks
// on a byte stream; it dispatches one backing write at a time copies-first (draining the disks keeps
// the Pool from stalling producers). On error it aborts the Pool and replies the error to every
// waiter, then drains the in-flight copy and exits once Finish has signalled no more producers.
func (d *Spool) orchestrate() {
	defer close(d.finished)
	defer close(d.workCh) // stops drainLoop

	var (
		pendingCopy   []handoff
		pendingPermit []permitReq
		failErr       error
		copying       bool
		backingInUse  int
		shutting      bool
	)
	shutdownCh := d.shutdownCh
	for {
		if shutting && !copying && len(pendingCopy) == 0 {
			return
		}
		select {
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
			// A direct write consumes a backing permit slot; release it whether or not its Commit
			// (the placement record) succeeds.
			if req.kind == writeDirect {
				backingInUse--
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
				if d.tr != nil {
					// The dump committed to this disk: record where it staged so the queued DLE shows a
					// 0% flush bar (and which disk) while it waits behind other drains — taking over from
					// the "staging" mark its in-flight dump carried.
					d.tr.StageHolding(arch.Host+":"+arch.Path, d.pool.Name(req.disk))
				}
				pendingCopy = append(pendingCopy, handoff{arch: arch, pos: pos, disk: req.disk})
			}
			req.reply <- sinkResp{}
		case pr := <-d.permitReqCh:
			if failErr != nil {
				pr.reply <- failErr
				break
			}
			pendingPermit = append(pendingPermit, pr)
		case res := <-d.copyDoneCh:
			copying = false
			backingInUse--
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
			for _, pr := range pendingPermit {
				pr.reply <- failErr
			}
			pendingPermit = nil
			pendingCopy = nil // a failed run leaves the holding placements for the next run's flush
			continue
		}

		// Dispatch one backing write, copies-first: a copy needs the backing exclusively (it only ever
		// runs when buffering, where Slots is 1), so dispatch it when the backing is idle; otherwise
		// hand the free permits to waiting direct writes.
		if backingInUse == 0 && !copying && len(pendingCopy) > 0 {
			j := pendingCopy[0]
			pendingCopy = pendingCopy[1:]
			copying = true
			backingInUse = 1
			d.workCh <- j
		} else {
			for len(pendingPermit) > 0 && !copying && backingInUse < d.backingSlots {
				pr := pendingPermit[0]
				pendingPermit = pendingPermit[1:]
				backingInUse++
				pr.reply <- nil
			}
		}
	}
}

// drainLoop streams each dispatched copy on its own goroutine, reporting the result back to the
// orchestrator. The orchestrator dispatches at most one copy at a time, so there is exactly one in
// flight.
func (d *Spool) drainLoop() {
	for j := range d.workCh {
		d.copyDoneCh <- copyResult{it: j, err: d.copyOne(j)}
	}
}

// copyOne reads one staged archive from its holding disk and streams it to the backing through a
// RemoteSink — so its volume rolls and its placement record land on the orchestrator, while the bytes
// flow on this copy goroutine. The backing's NewCopy writer re-checksums the payload against the
// recorded digest (catching holding-disk corruption) and preserves the source's identity. The
// placement record and the holding reclaim are the orchestrator's (the backing commit records the
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
	var tap func(int64)
	if d.tr != nil {
		tap = func(copied int64) { d.tr.AddDrainBytes(dleID, copied) }
	}
	real, err := d.backing.NewCopy(j.arch, tap)
	if err != nil {
		return fmt.Errorf("flush %s L%d to %q: %w", dleID, j.arch.Level, d.backingName, err)
	}
	rs := &remoteSink{d: d, real: real, kind: writeCopy}
	if _, err := xfer.Transfer(context.Background(), xfer.Reader(rc), xfer.Filters{}, rs); err != nil {
		return fmt.Errorf("flush %s L%d to %q: %w", dleID, j.arch.Level, d.backingName, err)
	}
	return nil
}

// finalizeDrain reclaims the holding copy (files + placement, via the holding ArchiveStore) and
// releases its back-pressure once the archive has landed on the backing. It runs on the orchestrator.
// The backing placement was already recorded by the copy's commit, so the archive is never absent
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
	d.logf("flushed %s L%d to %q", dleID, it.arch.Level, d.backingName)
	return nil
}

// handoff is one committed holding archive the orchestrator queues to copy: its metadata, its
// positions on the holding disk, and which disk it landed on (so the copy reads, reclaims, and
// releases the right one).
type handoff struct {
	arch record.Archive
	pos  record.ArchivePos
	disk int
}

// permitReq is a direct write waiting for a backing permit; reply is sent the run's error or nil once
// granted.
type permitReq struct {
	reply chan error
}

// copyResult is one finished (or failed) copy the drain goroutine hands back to the orchestrator,
// which reclaims the holding copy on success (finalizeDrain).
type copyResult struct {
	it  handoff
	err error
}
