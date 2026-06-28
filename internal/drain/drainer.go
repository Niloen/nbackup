package drain

import (
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

// drainer.go is the consuming half of a dump: the run's archive store. A producer ingests each
// archive into a Sink the Drainer hands out (Acquire → transfer → commit); the Drainer decides
// per archive whether to buffer it on a holding disk (then
// copy it to the authoritative landing later) or — when no disk fits, or none is configured — write
// it straight to the landing. It is the run's sole catalog writer: every placement record, every
// volume roll's catalog write, and the holding reclaim happen on its one orchestrator goroutine.
//
// Landing bundles everything the Drainer needs to write the authoritative landing — its name,
// writer, session, the funnel its control calls route through, and how many writes may run at once.
//
// Three actors, all private here:
//   - the orchestrator goroutine (orchestrate): the sole catalog writer. It records placements,
//     serves the landing sink's funnel, dispatches holding→landing copies copies-first, grants the
//     landing permit, and finalizes each drain.
//   - the copy goroutine (drainLoop): streams one holding→landing copy at a time (copyOne) while the
//     orchestrator stays free to serve that copy's funnel.
//   - the producer's own goroutines (external): a direct write runs on the producer goroutine that
//     holds the landing permit, funnelling its control calls back to the orchestrator.
//
// The landing is a single serial writer when it can span volumes (a copy and a direct write must
// not interleave parts), modelled as a permit of LandingSlots: 1 when buffering or spanning, else
// the worker count. Holding back-pressure is the Pool's; a landing failure aborts the Pool so the
// producers stop and the run fails — never dropping data.

// Config is what the engine wires a Drainer from: the catalog it solely writes, the slot it
// records under, the Landing it drains to, the holding Pool (empty = never buffer), and the run's
// progress + log seams.
type Config struct {
	Cat      *catalog.Catalog
	SlotMeta *record.Slot
	Landing  Landing
	Pool     *Pool // holding disks; empty = no buffering (every archive goes direct)
	Tracker  *progress.Tracker
	Logf     func(format string, args ...any)
}

// Landing is the authoritative store the drain copies (or writes) archives to: its medium name, the
// archiveio Writer built over the funnel proxy, the slot session (for direct writes and the final
// seal), the Funnel its control calls route back through, and Slots — how many landing writes may
// run at once (1 while buffering or spanning, else the worker count).
type Landing struct {
	Name    string
	Writer  *archiveio.Writer
	Session *clerk.Session
	Funnel  *Funnel
	Slots   int
}

// Drainer is the consuming side of a dump — the run's archive store (see the file comment). Build
// it with New, drive it from the producer with Acquire + Sink.Commit, and close it with Finish.
type Drainer struct {
	cat          *catalog.Catalog
	landing      string
	slotMeta     *record.Slot
	landW        *archiveio.Writer
	landSession  *clerk.Session
	realSink     archiveio.VolumeSink
	reqCh        chan sinkReq // the landing sink's funnel
	pool         *Pool
	landingSlots int
	tr           *progress.Tracker
	logf         func(format string, args ...any)

	landCh      chan landReq   // a producer reporting a committed archive (holding or direct)
	permitReqCh chan permitReq // a direct write waiting for the landing permit
	workCh      chan handoff   // copies dispatched to the copy goroutine
	copyDoneCh  chan copyResult
	shutdownCh  chan struct{} // closed by Finish: no more producers
	finished    chan struct{} // closed when the orchestrator goroutine exits

	mu       sync.Mutex
	abortErr error
}

// New builds a Drainer from cfg and starts its orchestrator and copy goroutines. The producer may
// call Acquire concurrently at once; Finish stops it.
func New(cfg Config) *Drainer {
	d := &Drainer{
		cat:          cfg.Cat,
		landing:      cfg.Landing.Name,
		slotMeta:     cfg.SlotMeta,
		landW:        cfg.Landing.Writer,
		landSession:  cfg.Landing.Session,
		realSink:     cfg.Landing.Funnel.real,
		reqCh:        cfg.Landing.Funnel.reqCh,
		pool:         cfg.Pool,
		landingSlots: cfg.Landing.Slots,
		tr:           cfg.Tracker,
		logf:         cfg.Logf,
		landCh:       make(chan landReq),
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

// Sink is one archive's ingestion handle from Acquire: an xfer.Sink that writes the encoded bytes
// onto the chosen medium (a holding disk, or the landing for a direct write). Draining yields a
// committer that finalizes the stored archive. disk < 0 means the landing itself (a direct write);
// meta + tap are the archive's input record and the running-compressed-count progress hook.
type Sink struct {
	d       *Drainer
	session *clerk.Session
	disk    int
	meta    record.Archive
	tap     func(int64) // running count of bytes that have landed, for live status
}

// Drain writes the encoded stream onto the chosen medium, metering it. It runs on the producer
// goroutine (the bytes' data half) and returns a committer carrying the measured archive + parts,
// which Transfer seals once the producer's totals are known.
func (s *Sink) Drain(in io.Reader) (xfer.Committer, error) {
	arch, parts, err := s.session.WriteArchive(s.meta, in, s.tap)
	if err != nil {
		return nil, err
	}
	return &committer{d: s.d, session: s.session, disk: s.disk, measured: arch, parts: parts}, nil
}

// committer finalizes one staged archive once the producer's totals are known: it writes the commit
// footer (merging the stats) and hands the committed archive to the orchestrator to record (a
// holding write queues the holding→landing copy; a direct write records the landing placement and
// releases the landing permit). It is the xfer.Committer a Drain returns, so it cannot run before
// its bytes have landed.
type committer struct {
	d        *Drainer
	session  *clerk.Session
	disk     int
	measured record.Archive
	parts    []record.FilePos
}

// Commit writes the footer and records the placement, returning the run's error if the store has
// aborted. The committed catalog record is the store's own (recorded via the orchestrator); the
// producer needs nothing back, so Commit yields only an error.
func (c *committer) Commit(p xfer.Produced) error {
	committed, pos, err := c.session.Commit(c.measured, c.parts, p.FileCount, p.Uncompressed, p.Members)
	if err != nil {
		return err
	}
	reply := make(chan error, 1)
	c.d.landCh <- landReq{arch: committed, pos: pos, disk: c.disk, reply: reply}
	return <-reply
}

// Acquire reserves ingestion for an archive described by meta, estimated at est bytes, and returns
// the Sink to transfer it into. It blocks for back-pressure: a holding write waits while every
// fitting disk is over capacity; a direct write (no disk fits, or none is configured) waits for a
// free landing permit. prog receives the running compressed (landed) byte count. It returns the
// run's error if the drain has aborted.
func (d *Drainer) Acquire(est int64, meta record.Archive, prog func(int64)) (xfer.Sink, error) {
	idx, direct, err := d.pool.Acquire(est)
	if err != nil {
		return nil, err
	}
	if !direct {
		return &Sink{d: d, session: d.pool.Session(idx), disk: idx, meta: meta, tap: prog}, nil
	}
	reply := make(chan error, 1)
	d.permitReqCh <- permitReq{reply: reply}
	if err := <-reply; err != nil {
		return nil, err
	}
	return &Sink{d: d, session: d.landSession, disk: -1, meta: meta, tap: prog}, nil
}

// Aborted returns the run's error once the drain has failed (so producers can stop scheduling), or
// nil while healthy.
func (d *Drainer) Aborted() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.abortErr
}

// Finish signals that the producers are done, waits for the orchestrator to drain every queued
// copy, then seals the landing slot and returns it — or the run's error if the drain failed.
func (d *Drainer) Finish(now time.Time) (*record.Slot, error) {
	close(d.shutdownCh)
	<-d.finished
	if err := d.Aborted(); err != nil {
		return nil, err
	}
	return d.landSession.Finish(now)
}

func (d *Drainer) setAbort(err error) {
	d.mu.Lock()
	if d.abortErr == nil {
		d.abortErr = err
	}
	d.mu.Unlock()
	d.pool.Abort(err) // wake producers blocked on holding back-pressure
}

// orchestrate is the sole catalog writer. Its select multiplexes the producers, the funnel, and the
// copy goroutine so control never blocks on a byte stream; it dispatches one landing write at a
// time copies-first (draining the disks keeps the Pool from stalling producers). On error it aborts
// the Pool and replies the error to every producer, then drains the in-flight copy and exits once
// Finish has signalled no more producers.
func (d *Drainer) orchestrate() {
	defer close(d.finished)
	defer close(d.workCh) // stops drainLoop

	var (
		pendingCopy   []handoff
		pendingPermit []permitReq
		failErr       error
		copying       bool
		landingInUse  int
		shutting      bool
	)
	shutdownCh := d.shutdownCh
	for {
		if shutting && !copying && len(pendingCopy) == 0 {
			return
		}
		select {
		case lr := <-d.landCh:
			if failErr != nil {
				if lr.disk < 0 {
					landingInUse--
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
				if d.tr != nil {
					// Mark it buffered now, while it is queued behind other drains, so live status
					// shows it staged on holding (a 0% flush bar) instead of mistaking it for a direct write.
					d.tr.StageHolding(lr.arch.Host+":"+lr.arch.Path, d.pool.Name(lr.disk))
				}
				lr.reply <- nil
			} else {
				// A direct write has no holding copy and no Pool charge — just record the landing.
				landingInUse--
				if err := d.cat.AddArchive(d.slotMeta, d.landing, lr.arch, lr.pos); err != nil {
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
			landingInUse--
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

		// Dispatch one landing write, copies-first: a copy needs the landing exclusively (it only
		// ever runs when buffering, where LandingSlots is 1), so dispatch it when the landing is idle;
		// otherwise hand the free permits to waiting direct writes.
		if landingInUse == 0 && !copying && len(pendingCopy) > 0 {
			j := pendingCopy[0]
			pendingCopy = pendingCopy[1:]
			copying = true
			landingInUse = 1
			d.workCh <- j
		} else {
			for len(pendingPermit) > 0 && !copying && landingInUse < d.landingSlots {
				pr := pendingPermit[0]
				pendingPermit = pendingPermit[1:]
				landingInUse++
				pr.reply <- nil
			}
		}
	}
}

// drainLoop streams each dispatched copy on its own goroutine, reporting the result back to the
// orchestrator. The orchestrator dispatches at most one copy at a time, so there is exactly one in
// flight.
func (d *Drainer) drainLoop() {
	for j := range d.workCh {
		arch, pos, err := d.copyOne(j)
		d.copyDoneCh <- copyResult{it: j, arch: arch, pos: pos, err: err}
	}
}

// copyOne reads one staged archive from its holding disk and streams it to the landing, returning
// the committed archive and its landing position. It runs on the copy goroutine and drives the
// landing Writer directly — so its only catalog touch is via the Writer's funnelled sink, never
// here; the placement record and the holding reclaim are the orchestrator's (finalizeDrain).
func (d *Drainer) copyOne(j handoff) (record.Archive, record.ArchivePos, error) {
	holding := d.pool.Name(j.disk)
	dleID := j.arch.Host + ":" + j.arch.Path
	if d.tr != nil {
		d.tr.StartFlush(dleID, holding)
	}
	ref := clerk.Ref{Slot: d.slotMeta.ID, DLE: j.arch.DLE, Level: j.arch.Level}
	rc, err := openArchiveAt(d.pool.HoldVol(j.disk), ref, j.pos.Parts)
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, fmt.Errorf("flush %s L%d: read holding disk: %w", dleID, j.arch.Level, err)
	}
	var tap func(int64)
	if d.tr != nil {
		tap = func(copied int64) { d.tr.AddDrainBytes(dleID, copied) }
	}
	arch, pos, err := d.landW.CopyArchive(j.arch, rc, tap)
	rc.Close()
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, fmt.Errorf("flush %s L%d to %q: %w", dleID, j.arch.Level, d.landing, err)
	}
	return arch, pos, nil
}

// finalizeDrain records the landed archive's placement, then reclaims the holding copy (files +
// placement) and releases its back-pressure. It runs on the orchestrator (the sole catalog writer).
// The landing placement is recorded before the holding copy is dropped, so the archive is never
// absent from the catalog.
func (d *Drainer) finalizeDrain(it handoff, arch record.Archive, pos record.ArchivePos) error {
	holding := d.pool.Name(it.disk)
	holdVol := d.pool.HoldVol(it.disk)
	dleID := it.arch.Host + ":" + it.arch.Path
	if err := d.cat.AddArchive(d.slotMeta, d.landing, arch, pos); err != nil {
		return fmt.Errorf("flush %s: record landing placement: %w", dleID, err)
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
	d.logf("flushed %s L%d to %q", dleID, it.arch.Level, d.landing)
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

// landReq is a producer reporting a committed archive to the orchestrator: disk >= 0 is a holding
// write (record placement, queue the copy), disk < 0 a direct write (record the landing, release
// the permit). reply carries back the run's error (nil while healthy).
type landReq struct {
	arch  record.Archive
	pos   record.ArchivePos
	disk  int
	reply chan error
}

// permitReq is a direct write waiting for a landing permit; reply is closed-with-error or sent nil
// once granted.
type permitReq struct {
	reply chan error
}

// copyResult is one finished (or failed) copy the drain goroutine hands back to the orchestrator,
// which records the landing placement and reclaims the holding copy (finalizeDrain).
type copyResult struct {
	it   handoff
	arch record.Archive
	pos  record.ArchivePos
	err  error
}

// openArchiveAt reads an archive's parts straight from a live volume (the holding writer's own,
// whose index the producer keeps current), concatenating them — the drain's read seam, bypassing
// the catalog and the fresh-mounter path (which would have a stale index for in-flight files).
func openArchiveAt(vol media.Volume, ref clerk.Ref, parts []record.FilePos) (io.ReadCloser, error) {
	return archiveio.NewReader().Open(parts, archiveio.Expect{Slot: ref.Slot, DLE: ref.DLE, Level: ref.Level},
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
