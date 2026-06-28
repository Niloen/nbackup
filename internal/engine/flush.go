package engine

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
)

// flush.go is the holding disk's drain. The dump orchestrator (engine.go) drives every run; when
// the dump medium is a holding disk it hands each committed archive here to be drained: copied to
// the authoritative landing, then reclaimed off the disk. The copy runs on a drainer goroutine
// (copyOne) while the orchestrator stays free to record control; the orchestrator finalizes each
// copy (finalizeDrain). A byteGate sized to the holding disk's capacity back-pressures the
// dumpers; a landing failure aborts it so the dumpers stop and the run fails — never dropping
// data. The dumpers write an unbounded disk/cloud sink (parallel-safe); the landing is written by
// one serial writer (the two combinations the writer already documents). Flush is the amflush
// analogue, draining a crashed run's leftover holding archives on the next dump.

// byteGate is the holding disk's capacity back-pressure. A dumper charges an archive's bytes
// when it commits, then waits while the disk is over capacity; the drain releases the bytes
// once the archive has landed and been reclaimed. A landing failure aborts the gate, waking
// blocked dumpers (which then stop) so the run fails rather than overfilling the disk. A
// zero/negative capacity is unbounded (no back-pressure).
type byteGate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	capacity int64
	used     int64
	aborted  error
}

func newByteGate(capacity int64) *byteGate {
	g := &byteGate{capacity: capacity}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// charge accounts n landed bytes against the disk budget (does not block). Charging before
// the archive is enqueued keeps the accounting correct: the drain's release happens-after.
func (g *byteGate) charge(n int64) {
	g.mu.Lock()
	g.used += n
	g.mu.Unlock()
}

// waitUnderCapacity blocks while the disk holds more than its capacity, returning the abort
// error if the drain failed. The archive that pushed it over is already enqueued, so the drain
// copies it and releases, unblocking the dumper.
func (g *byteGate) waitUnderCapacity() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.aborted == nil && g.capacity > 0 && g.used > g.capacity {
		g.cond.Wait()
	}
	return g.aborted
}

// release returns n charged bytes and wakes any blocked dumpers.
func (g *byteGate) release(n int64) {
	g.mu.Lock()
	g.used -= n
	if g.used < 0 {
		g.used = 0
	}
	g.cond.Broadcast()
	g.mu.Unlock()
}

// abort wakes every blocked dumper — the landing is unreachable, so the run must fail rather
// than wait for space that will never free.
func (g *byteGate) abort(err error) {
	g.mu.Lock()
	if g.aborted == nil {
		g.aborted = err
	}
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *byteGate) err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.aborted
}

// copyOne is the drain's DATA half: it reads one staged archive from the holding disk and streams
// it to the landing, returning the committed archive and its landing position. It runs on the
// drainer goroutine. It drives the landing Writer directly (not the clerk Session), so it touches
// the catalog only through the Writer's VolumeSink — whose control calls the proxy funnels back to
// the orchestrator, never writing the catalog here. The placement record and the holding reclaim
// are the control half (finalizeDrain), run by the orchestrator (the sole catalog writer).
func (e *Engine) copyOne(landW *archiveio.Writer, slotMeta *record.Slot, holdVol media.Volume, it handoff, tr *progress.Tracker) (record.Archive, record.ArchivePos, error) {
	if tr != nil {
		tr.StartFlush(it.dleID)
	}
	ref := clerk.Ref{Slot: slotMeta.ID, DLE: it.arch.DLE, Level: it.arch.Level}
	rc, err := openArchiveAt(holdVol, ref, it.pos.Parts)
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, fmt.Errorf("flush %s L%d: read holding disk: %w", it.dleID, it.arch.Level, err)
	}
	arch, pos, err := landW.CopyArchive(it.arch, rc)
	rc.Close()
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, fmt.Errorf("flush %s L%d to %q: %w", it.dleID, it.arch.Level, e.mediumName, err)
	}
	return arch, pos, nil
}

// finalizeDrain is the drain's CONTROL half: it records the landed archive's placement, then
// reclaims the holding copy (files + placement) and releases its back-pressure. It runs on the
// orchestrator (the sole catalog writer). The landing placement is recorded before the holding
// copy is dropped, so the archive is never absent from the catalog.
func (e *Engine) finalizeDrain(slotMeta *record.Slot, holding string, holdVol media.Volume, it handoff, arch record.Archive, pos record.ArchivePos, gate *byteGate, tr *progress.Tracker, logf Logf) error {
	if err := e.cat.AddArchive(slotMeta, e.mediumName, arch, pos); err != nil {
		return fmt.Errorf("flush %s: record landing placement: %w", it.dleID, err)
	}
	for _, p := range archivePosFiles(it.pos) {
		if err := holdVol.RemoveFile(p); err != nil {
			return fmt.Errorf("flush %s: reclaim holding disk: %w", it.dleID, err)
		}
	}
	if _, _, err := e.cat.RemoveArchive(slotMeta.ID, holding, it.arch.DLE); err != nil {
		return fmt.Errorf("flush %s: drop holding placement: %w", it.dleID, err)
	}
	gate.release(it.arch.Compressed)
	if tr != nil {
		tr.FinishFlush(it.dleID)
	}
	logf.log("flushed %s L%d to %q", it.dleID, it.arch.Level, e.mediumName)
	return nil
}

// drainer copies staged archives to the landing on its own goroutine: it runs copyOne (the pure
// byte-copy) for each archive the orchestrator hands it over workCh and reports the result on
// doneCh. It touches the catalog and the librarian only through the landing Writer's proxy sink,
// whose NextPart/PlaceRecord funnel back to the orchestrator — so the byte stream runs here while
// all control (catalog writes, librarian volume rolls) stays on the orchestrator. The landing is
// written by one serial writer, so there is exactly one drainer. Each job is either a staged-archive
// copy (copyOne, reading from the holding disk) or an oversized DLE's direct dump (backupItem,
// running the full pipeline straight to the landing) — both write the same landing Writer.
func (e *Engine) drainer(landW *archiveio.Writer, landSession *clerk.Session, slotMeta *record.Slot, holdVol media.Volume, workCh <-chan drainJob, doneCh chan<- copyResult, tr *progress.Tracker, logf Logf) {
	for job := range workCh {
		if job.copy != nil {
			arch, pos, err := e.copyOne(landW, slotMeta, holdVol, *job.copy, tr)
			doneCh <- copyResult{it: *job.copy, arch: arch, pos: pos, err: err}
		} else {
			arch, pos, err := e.backupItem(landSession, *job.direct, tr, logf)
			doneCh <- copyResult{arch: arch, pos: pos, err: err, direct: true}
		}
	}
}

// drainJob is one landing write the orchestrator dispatches to the drainer: exactly one of copy
// (a staged archive to copy off the holding disk) or direct (an oversized DLE to dump straight to
// the landing) is set.
type drainJob struct {
	copy   *handoff
	direct *planner.Item
}

// copyResult is one finished (or failed) landing write the drainer hands back to the orchestrator,
// which records the landing placement — and, for a copy (direct=false), reclaims the holding copy
// and releases the gate (finalizeDrain). it is set only for a copy.
type copyResult struct {
	it     handoff
	arch   record.Archive
	pos    record.ArchivePos
	err    error
	direct bool
}

// sinkReq is a VolumeSink call the drainer's proxy funnels to the orchestrator: placeRecord
// selects PlaceRecord(size) over NextPart(). The orchestrator runs the real sink and replies on
// reply — so any volume roll's catalog writes (RecordVolume / recycle) land on the sole catalog
// writer.
type sinkReq struct {
	placeRecord bool
	size        int64
	reply       chan sinkResp
}

// sinkResp is the orchestrator's reply to a sinkReq: the union of NextPart's and PlaceRecord's
// returns (max is unused for PlaceRecord).
type sinkResp struct {
	vol    media.Volume
	max    int64
	volume string
	epoch  int
	err    error
}

// proxySink is the VolumeSink the drainer's landing Writer is built over. Its NextPart/PlaceRecord
// touch neither the librarian nor the catalog — they send the call to the orchestrator over reqCh
// and block on the reply. The byte write (vol.AppendFile) the caller does on the returned volume
// is the data half, on the drainer goroutine; the control half (the sink call) runs on the
// orchestrator. The round-trip is synchronous, so the drive is never written from two goroutines.
type proxySink struct {
	reqCh chan<- sinkReq
}

func (p *proxySink) NextPart() (media.Volume, int64, string, int, error) {
	reply := make(chan sinkResp, 1)
	p.reqCh <- sinkReq{reply: reply}
	r := <-reply
	return r.vol, r.max, r.volume, r.epoch, r.err
}

func (p *proxySink) PlaceRecord(size int64) (media.Volume, string, int, error) {
	reply := make(chan sinkResp, 1)
	p.reqCh <- sinkReq{placeRecord: true, size: size, reply: reply}
	r := <-reply
	return r.vol, r.volume, r.epoch, r.err
}

// serve runs one funneled sink call on the real WriteSink — on the orchestrator goroutine, so a
// roll's RecordVolume/recycle catalog writes land on the sole catalog writer.
func serve(real archiveio.VolumeSink, req sinkReq) sinkResp {
	if req.placeRecord {
		vol, volume, epoch, err := real.PlaceRecord(req.size)
		return sinkResp{vol: vol, volume: volume, epoch: epoch, err: err}
	}
	vol, max, volume, epoch, err := real.NextPart()
	return sinkResp{vol: vol, max: max, volume: volume, epoch: epoch, err: err}
}

// openArchiveAt reads an archive's parts straight from a live volume (the holding writer's own,
// whose index the dumpers keep current), concatenating them — the drain's read seam, bypassing
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

// Flush drains a crashed run's leftover archives from the holding disk to the landing — the
// amflush analogue. A holding-disk run records each archive's holding placement before
// flushing it and removes it after, so a crash leaves the un-flushed archives recorded on the
// holding medium in the catalog. Flush reads those placements (no medium scan needed), copies
// each archive to the landing, removes the holding placement, reclaims the disk, and seals the
// slot. It is idempotent and a no-op when no holding disk is configured or nothing is staged.
func (e *Engine) Flush(now time.Time, logf Logf) (flushed int, err error) {
	holding, ok := e.cfg.HoldingMedium()
	if !ok {
		return 0, nil
	}
	slots := e.cat.SlotsOn(holding)
	if len(slots) == 0 {
		return 0, nil
	}
	holdVol, _, _, err := e.mediumVolume(holding)
	if err != nil {
		return 0, err
	}
	for _, s := range slots {
		hp, ok := e.placementOn(s.ID, holding)
		if !ok {
			continue
		}
		spec := archiveio.SlotSpec{ID: s.ID, Date: s.Date, Sequence: s.Sequence, Generator: s.Generator, CreatedAt: s.CreatedAt}
		landWT, err := e.prepareWriter(e.mediumName, spec, now, logf)
		if err != nil {
			return flushed, fmt.Errorf("flush %s: open landing %q: %w", s.ID, e.mediumName, err)
		}
		landSession := e.clerk.OpenSlot(landWT.w, e.mediumName)

		for _, ap := range hp.Archives {
			ref := clerk.Ref{Slot: s.ID, DLE: ap.DLE, Level: ap.Level}
			dleID := e.DisplayDLE(ap.DLE)
			// A crash between recording the landing placement and reclaiming the holding one
			// leaves an archive on both; in that case just reclaim, don't re-copy.
			if !e.archiveOnLanding(s.ID, ap.DLE, ap.Level) {
				arch, err := e.catalogArchive(s.ID, ap.DLE, ap.Level)
				if err != nil {
					return flushed, fmt.Errorf("flush %s %s: %w", s.ID, dleID, err)
				}
				rc, err := e.clerk.Open(ref, holding)
				if err != nil {
					return flushed, fmt.Errorf("flush %s %s: read holding disk: %w", s.ID, dleID, err)
				}
				// CopyArchive records the landing placement inline.
				if err := landSession.CopyArchive(arch, rc); err != nil {
					rc.Close()
					return flushed, fmt.Errorf("flush %s %s to %q: %w", s.ID, dleID, e.mediumName, err)
				}
				rc.Close()
			}
			for _, pos := range archivePosFiles(ap) {
				if err := holdVol.RemoveFile(pos); err != nil {
					return flushed, fmt.Errorf("flush %s %s: reclaim holding disk: %w", s.ID, dleID, err)
				}
			}
			if _, _, err := e.cat.RemoveArchive(s.ID, holding, ap.DLE); err != nil {
				return flushed, err
			}
			flushed++
			logf.log("flushed %s %s to %q", s.ID, dleID, e.mediumName)
		}
		if err := e.cat.SealSlot(s.ID, now); err != nil {
			return flushed, fmt.Errorf("flush %s: seal: %w", s.ID, err)
		}
	}
	return flushed, nil
}

// catalogArchive returns a holding-disk archive's metadata for a re-copy: the catalogued
// record (checksum, sizes, scheme) plus its member list from the on-medium index.
func (e *Engine) catalogArchive(slotID, dle string, level int) (record.Archive, error) {
	s, err := e.cat.ReadSlot(slotID)
	if err != nil {
		return record.Archive{}, err
	}
	for _, a := range s.Archives {
		if a.DLE == dle && a.Level == level {
			a.Members, _ = e.clerk.Members(clerk.Ref{Slot: slotID, DLE: dle, Level: level})
			return a, nil
		}
	}
	return record.Archive{}, fmt.Errorf("archive %s L%d not in catalog", dle, level)
}

// archiveOnLanding reports whether the slot's landing placement already holds (dle, level).
func (e *Engine) archiveOnLanding(slotID, dle string, level int) bool {
	p, ok := e.placementOn(slotID, e.mediumName)
	if !ok {
		return false
	}
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return true
		}
	}
	return false
}
