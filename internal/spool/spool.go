package spool

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// spool.go is the consuming half of a dump: the run's buffered, concurrency-safe archive store — a
// WriteFS over a backing medium plus holding disks. A producer ingests each
// archive into a Sink the Spool hands out (Acquire → transfer → commit); the Spool decides
// per archive whether to buffer it on a holding disk (then
// copy it to the authoritative backing later) or — when no disk fits, or none is configured — write
// it straight to the backing. It is the run's sole catalog writer: every placement record, every
// volume roll's catalog write, and the holding reclaim happen on its one orchestrator goroutine.
//
// Backing bundles everything the Spool needs to write the authoritative backing — its name, the raw
// copy writer, the clerk Session for direct writes, the orchestrator Client its control calls route
// through, and how many writes may run at once.
//
// Three actors, all private here:
//   - the orchestrator goroutine (orchestrate): the sole catalog writer and the server. It records
//     placements, serves the backing sink's Client calls, dispatches holding→backing copies
//     copies-first, grants the backing permit, and finalizes each drain.
//   - the copy goroutine (drainLoop): streams one holding→backing copy at a time (copyOne) while the
//     orchestrator stays free to serve that copy's Client calls.
//   - the producer's own goroutines (external): a direct write runs on the producer goroutine that
//     holds the backing permit, routing its control calls back to the orchestrator.
//
// The backing is a single serial writer when it can span volumes (a copy and a direct write must
// not interleave parts), modelled as a permit of BackingSlots: 1 when buffering or spanning, else
// the worker count. Holding back-pressure is the Pool's; a backing failure aborts the Pool so the
// producers stop and the run fails — never dropping data.

// Config is what the engine wires a Spool from: the Catalog it solely writes, the slot it
// records under, the Backing medium it drains to, the Holding disks (empty = never buffer), and the
// run's progress + log seams.
type Config struct {
	Catalog  *catalog.Catalog
	SlotMeta *record.Slot
	Backing  Backing
	Holding  *Pool // holding disks; empty = no buffering (every archive goes direct)
	Tracker  *progress.Tracker
	Logf     func(format string, args ...any)
}

// Backing is the authoritative store the drain copies (or writes) archives to: its medium name, the
// archiveio Writer built over the orchestrator-client proxy (for the raw holding→backing copy), the
// slot Session for direct writes and the final seal, the Client its control calls route back through,
// and Slots — how many backing writes may run at once (1 while buffering or spanning, else the worker
// count).
type Backing struct {
	Name    string
	Writer  *archiveio.Writer
	Session *clerk.Session
	Client  *Client
	Slots   int
}

// Spool is a concurrency-safe archiveio.WriteFS.
var _ archiveio.WriteFS = (*Spool)(nil)

// Spool is the consuming side of a dump — the run's archive store (see the file comment). Build
// it with New, drive it from the producer with Create + transfer/commit, and close it with Finish.
type Spool struct {
	cat            *catalog.Catalog
	backing        string
	slotMeta       *record.Slot
	backingW       *archiveio.Writer
	backingSession *clerk.Session
	realSink       archiveio.VolumeSink
	reqCh          chan sinkReq // the backing sink's orchestrator-client channel
	pool           *Pool
	backingSlots   int
	tr             *progress.Tracker
	logf           func(format string, args ...any)

	placeCh     chan placeReq  // a producer reporting a committed archive (holding or direct)
	permitReqCh chan permitReq // a direct write waiting for the backing permit
	workCh      chan handoff   // copies dispatched to the copy goroutine
	copyDoneCh  chan copyResult
	shutdownCh  chan struct{} // closed by Finish: no more producers
	finished    chan struct{} // closed when the orchestrator goroutine exits

	mu       sync.Mutex
	abortErr error
}

// New builds a Spool from cfg and starts its orchestrator and copy goroutines. The producer may
// call Acquire concurrently at once; Finish stops it.
func New(cfg Config) *Spool {
	d := &Spool{
		cat:            cfg.Catalog,
		backing:        cfg.Backing.Name,
		slotMeta:       cfg.SlotMeta,
		backingW:       cfg.Backing.Writer,
		backingSession: cfg.Backing.Session,
		realSink:       cfg.Backing.Client.real,
		reqCh:          cfg.Backing.Client.reqCh,
		pool:           cfg.Holding,
		backingSlots:   cfg.Backing.Slots,
		tr:             cfg.Tracker,
		logf:           cfg.Logf,
		placeCh:        make(chan placeReq),
		permitReqCh:    make(chan permitReq),
		workCh:         make(chan handoff),
		copyDoneCh:     make(chan copyResult),
		shutdownCh:     make(chan struct{}),
		finished:       make(chan struct{}),
	}
	if d.logf == nil {
		d.logf = func(string, ...any) {}
	}
	go d.orchestrate()
	go d.drainLoop()
	return d
}

// Create reserves ingestion for an archive described by meta, estimated at est bytes, and returns the
// xfer.Sink to transfer it into — an ingest handle on the chosen medium's clerk Session. It blocks
// for back-pressure: a holding write waits while every fitting disk is over capacity; a direct write
// (no disk fits, or none is configured) waits for a free backing permit. prog receives the running
// compressed (landed) byte count. It returns the run's error if the spool has aborted. It makes Spool
// an archiveio.WriteFS.
func (d *Spool) Create(meta record.Archive, est int64, prog func(int64)) (xfer.Sink, error) {
	idx, direct, err := d.pool.Acquire(est)
	if err != nil {
		return nil, err
	}
	if !direct {
		return &ingest{d: d, aw: d.pool.Session(idx).NewArchive(meta, prog), disk: idx}, nil
	}
	reply := make(chan error, 1)
	d.permitReqCh <- permitReq{reply: reply}
	if err := <-reply; err != nil {
		return nil, err
	}
	return &ingest{d: d, aw: d.backingSession.NewArchive(meta, prog), disk: -1}, nil
}

// ingest is one archive's xfer.Sink: NextPart hands the producer the next part writer (rolling
// volumes via the clerk/archiveio handle); on Commit it forwards the committed archive's (arch, pos)
// to the orchestrator over placeCh to record the placement (a holding write queues the
// holding→backing copy; a direct write records the backing placement and releases the permit).
// disk < 0 is the backing itself. The placement is an orchestrator RPC, in the same family as the
// Client's volume rolls.
type ingest struct {
	d    *Spool
	aw   *clerk.ArchiveWriter
	disk int
}

func (s *ingest) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	return s.aw.NextPart(ctx)
}

func (s *ingest) Commit(ctx context.Context, p xfer.Produced) error {
	arch, pos, err := s.aw.Commit(ctx, p.FileCount, p.Uncompressed, p.Members)
	if err != nil {
		return err
	}
	reply := make(chan error, 1)
	s.d.placeCh <- placeReq{arch: arch, pos: pos, disk: s.disk, reply: reply}
	return <-reply
}

// Aborted returns the run's error once the drain has failed (so producers can stop scheduling), or
// nil while healthy.
func (d *Spool) Aborted() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.abortErr
}

// Finish signals that the producers are done, waits for the orchestrator to drain every queued
// copy, then seals the backing slot and returns it — or the run's error if the drain failed.
func (d *Spool) Finish(now time.Time) (*record.Slot, error) {
	close(d.shutdownCh)
	<-d.finished
	if err := d.Aborted(); err != nil {
		return nil, err
	}
	return d.backingSession.Finish(now)
}

func (d *Spool) setAbort(err error) {
	d.mu.Lock()
	if d.abortErr == nil {
		d.abortErr = err
	}
	d.mu.Unlock()
	d.pool.Abort(err) // wake producers blocked on holding back-pressure
}

// orchestrate is the sole catalog writer (the server). Its select multiplexes the producers, the
// Client calls, and the copy goroutine so control never blocks on a byte stream; it dispatches one backing write at a
// time copies-first (draining the disks keeps the Pool from stalling producers). On error it aborts
// the Pool and replies the error to every producer, then drains the in-flight copy and exits once
// Finish has signalled no more producers.
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
		case lr := <-d.placeCh:
			if failErr != nil {
				if lr.disk < 0 {
					backingInUse--
				}
				lr.reply <- failErr
				break
			}
			if lr.disk >= 0 {
				if err := d.cat.AddArchive(d.slotMeta, d.pool.Name(lr.disk), lr.arch, lr.pos); err != nil {
					failErr = err
					lr.reply <- err
					break
				}
				pendingCopy = append(pendingCopy, handoff{arch: lr.arch, pos: lr.pos, disk: lr.disk})
				lr.reply <- nil
			} else {
				// A direct write has no holding copy and no Pool charge — just record the backing.
				backingInUse--
				if err := d.cat.AddArchive(d.slotMeta, d.backing, lr.arch, lr.pos); err != nil {
					failErr = err
				}
				lr.reply <- failErr
			}
		case pr := <-d.permitReqCh:
			if failErr != nil {
				pr.reply <- failErr
				break
			}
			pendingPermit = append(pendingPermit, pr)
		case req := <-d.reqCh:
			req.reply <- serve(d.realSink, req)
		case res := <-d.copyDoneCh:
			copying = false
			backingInUse--
			if res.err != nil {
				failErr = res.err
			} else if err := d.finalizeDrain(res.it, res.arch, res.pos); err != nil {
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

		// Dispatch one backing write, copies-first: a copy needs the backing exclusively (it only
		// ever runs when buffering, where BackingSlots is 1), so dispatch it when the backing is idle;
		// otherwise hand the free permits to waiting direct writes.
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
		arch, pos, err := d.copyOne(j)
		d.copyDoneCh <- copyResult{it: j, arch: arch, pos: pos, err: err}
	}
}

// copyOne reads one staged archive from its holding disk and streams it to the backing, returning
// the committed archive and its backing position. It runs on the copy goroutine and drives the
// backing Writer directly — so its only catalog touch is via the Writer's Client sink, never
// here; the placement record and the holding reclaim are the orchestrator's (finalizeDrain).
func (d *Spool) copyOne(j handoff) (record.Archive, record.ArchivePos, error) {
	holding := d.pool.Name(j.disk)
	dleID := j.arch.Host + ":" + j.arch.Path
	if d.tr != nil {
		d.tr.StartFlush(dleID, holding)
	}
	exp := archiveio.Expect{Slot: d.slotMeta.ID, DLE: j.arch.DLE, Level: j.arch.Level}
	rc, err := openArchiveAt(d.pool.HoldVol(j.disk), exp, j.pos.Parts)
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, fmt.Errorf("flush %s L%d: read holding disk: %w", dleID, j.arch.Level, err)
	}
	var tap func(int64)
	if d.tr != nil {
		tap = func(copied int64) { d.tr.AddDrainBytes(dleID, copied) }
	}
	arch, pos, err := d.backingW.CopyArchive(context.Background(), j.arch, rc, tap)
	rc.Close()
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, fmt.Errorf("flush %s L%d to %q: %w", dleID, j.arch.Level, d.backing, err)
	}
	return arch, pos, nil
}

// finalizeDrain records the landed archive's placement, then reclaims the holding copy (files +
// placement) and releases its back-pressure. It runs on the orchestrator (the sole catalog writer).
// The backing placement is recorded before the holding copy is dropped, so the archive is never
// absent from the catalog.
func (d *Spool) finalizeDrain(it handoff, arch record.Archive, pos record.ArchivePos) error {
	holding := d.pool.Name(it.disk)
	holdVol := d.pool.HoldVol(it.disk)
	dleID := it.arch.Host + ":" + it.arch.Path
	if err := d.cat.AddArchive(d.slotMeta, d.backing, arch, pos); err != nil {
		return fmt.Errorf("flush %s: record backing placement: %w", dleID, err)
	}
	for _, p := range archivePosFiles(it.pos) {
		if err := holdVol.RemoveFile(p); err != nil {
			return fmt.Errorf("flush %s: reclaim holding disk: %w", dleID, err)
		}
	}
	if _, _, err := d.cat.RemoveArchive(d.slotMeta.ID, holding, it.arch.DLE); err != nil {
		return fmt.Errorf("flush %s: drop holding placement: %w", dleID, err)
	}
	d.pool.Release(it.disk, it.arch.Compressed)
	if d.tr != nil {
		d.tr.FinishFlush(dleID)
	}
	d.logf("flushed %s L%d to %q", dleID, it.arch.Level, d.backing)
	return nil
}

// handoff is one committed holding archive: its full metadata + member list (so the orchestrator
// needs no catalog read), its positions on the holding disk, and which disk it landed on (so the
// copy reads, reclaims, and releases the right one).
type handoff struct {
	arch record.Archive
	pos  record.ArchivePos
	disk int
}

// placeReq is a producer reporting a committed archive to the orchestrator: disk >= 0 is a holding
// write (record placement, queue the copy), disk < 0 a direct write (record the backing, release
// the permit). reply carries back the run's error (nil while healthy).
type placeReq struct {
	arch  record.Archive
	pos   record.ArchivePos
	disk  int
	reply chan error
}

// permitReq is a direct write waiting for a backing permit; reply is closed-with-error or sent nil
// once granted.
type permitReq struct {
	reply chan error
}

// copyResult is one finished (or failed) copy the drain goroutine hands back to the orchestrator,
// which records the backing placement and reclaims the holding copy (finalizeDrain).
type copyResult struct {
	it   handoff
	arch record.Archive
	pos  record.ArchivePos
	err  error
}

// openArchiveAt reads an archive's parts straight from a live volume (the holding writer's own,
// whose index the producer keeps current), concatenating them — the drain's read seam, bypassing
// the catalog and the fresh-mounter path (which would have a stale index for in-flight files).
func openArchiveAt(vol media.Volume, exp archiveio.Expect, parts []record.FilePos) (io.ReadCloser, error) {
	return archiveio.NewReader().Open(parts, exp,
		func(p record.FilePos) (record.Header, io.ReadCloser, error) { return vol.ReadFile(p.Pos) })
}

// archivePosFiles lists an archive's file positions for reclamation, the commit footer (the
// marker) first so an interrupted reclaim un-commits before dropping parts.
func archivePosFiles(a record.ArchivePos) []int {
	pos := make([]int, 0, len(a.Parts)+2)
	pos = append(pos, a.Commit.Pos)
	if a.Index != (record.FilePos{}) {
		pos = append(pos, a.Index.Pos)
	}
	for _, pt := range a.Parts {
		pos = append(pos, pt.Pos)
	}
	return pos
}
