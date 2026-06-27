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

// flush.go is the dump's orchestrator and the holding disk's taper. Every dump runs the same
// shape: the workers dump to the dump medium as background goroutines and hand each committed
// archive over a channel to the orchestrator, which runs on the MAIN goroutine and is the run's
// sole catalog writer (so the catalog needs no lock). The orchestrator records each archive's
// placement on the dump medium as it arrives. When the dump medium is a holding disk it is also
// the taper: it copies each archive to the authoritative landing, then reclaims the holding copy.
// A byteGate sized to the holding disk's capacity back-pressures the dumpers; a landing failure
// aborts it so the dumpers stop and the run fails — never dropping data. The dumpers write an
// unbounded disk/cloud sink (parallel-safe); the taper drives the spanning landing serially (the
// two combinations the writer already documents). Flush (below) is the amflush analogue, draining
// a crashed run's leftover holding archives on the next dump.

// byteGate is the holding disk's capacity back-pressure. A dumper charges an archive's bytes
// when it commits, then waits while the disk is over capacity; the taper releases the bytes
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
// the archive is enqueued keeps the accounting correct: the taper's release happens-after.
func (g *byteGate) charge(n int64) {
	g.mu.Lock()
	g.used += n
	g.mu.Unlock()
}

// waitUnderCapacity blocks while the disk holds more than its capacity, returning the abort
// error if the taper failed. The archive that pushed it over is already enqueued, so the taper
// drains it and releases, unblocking the dumper.
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

// flushItem is one committed archive handed from a worker to the orchestrator: the committed
// archive (full metadata + member list, so the orchestrator needs no catalog read), its
// positions on the dump medium, and the DLE's display id for progress/logging.
type flushItem struct {
	arch  record.Archive
	pos   record.ArchivePos
	dleID string
}

// runOrchestrated executes a dump: the workers dump to the dump medium as background goroutines,
// handing each committed archive to the orchestrator on this goroutine, which records its
// placement (and, when buffering, drains it to the landing). It is the one path Run takes for
// every dump — direct (dump medium == landing) or buffered (dump medium == a holding disk). The
// orchestrator is the sole catalog writer, so the catalog needs no lock.
func (e *Engine) runOrchestrated(plan *planner.Plan, workers int, spec archiveio.SlotSpec, dumpMedium string, dumpWT *writeTarget, buffering bool, tr *progress.Tracker, now time.Time, logf Logf) (*record.Slot, error) {
	dumpSession := e.clerk.OpenSlot(dumpWT.w, dumpMedium)

	// Workers hand each committed archive over commitCh; the buffer holds the whole plan so a
	// worker never blocks on the orchestrator (a holding disk back-pressures via the gate instead).
	commitCh := make(chan flushItem, len(plan.Items))

	var (
		gate        *byteGate
		landSession *clerk.Session
		holdVol     media.Volume
	)
	if buffering {
		// Landing writer + session for the drain (this goroutine drives the spanning landing
		// serially). The holding disk's capacity back-pressures the dumpers through the gate.
		landWT, err := e.prepareWriter(e.mediumName, spec, now, logf)
		if err != nil {
			tr.SetPhase(progress.PhaseFailed)
			return nil, fmt.Errorf("open landing %q: %w", e.mediumName, err)
		}
		landSession = e.clerk.OpenSlot(landWT.w, e.mediumName)
		holdVol = dumpWT.lib.Volume()
		capBytes, _ := e.cfg.Media[dumpMedium].CapacityBytes()
		gate = newByteGate(capBytes)
	}

	// Dumpers in the background; close the queue when they finish (the orchestrator's exit signal).
	var dumpErr error
	go func() {
		dumpErr = e.runWorkers(plan.Items, workers, dumpSession, commitCh, gate, tr, logf)
		close(commitCh)
	}()

	slotMeta := record.NewSlot(spec.ID, spec.Date, spec.Sequence, spec.Generator, spec.CreatedAt)
	orchErr := e.orchestrate(commitCh, dumpMedium, slotMeta, buffering, landSession, holdVol, gate, tr, logf)

	if err := firstErr(orchErr, dumpErr); err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, err
	}
	tr.SetPhase(progress.PhaseSealing)
	// Seal the authoritative slot: the landing when buffering (the holding copies were drained
	// and reclaimed), else the dump medium itself.
	sealSession := dumpSession
	if buffering {
		sealSession = landSession
	}
	sealed, err := sealSession.Finish(time.Now().UTC())
	if err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, err
	}
	tr.SetPhase(progress.PhaseDone)
	return sealed, nil
}

// orchestrate records each committed archive's dump-medium placement as it arrives and, when
// buffering, drains it to the landing. Each loop it first records every commit available right
// now (so the catalog reflects the dump medium's contents promptly — the live view), then, when
// buffering, flushes one staged archive to the landing. A direct dump never stages, so it just
// records each placement as it arrives.
func (e *Engine) orchestrate(commitCh <-chan flushItem, dumpMedium string, slotMeta *record.Slot, buffering bool, landSession *clerk.Session, holdVol media.Volume, gate *byteGate, tr *progress.Tracker, logf Logf) error {
	var pending []flushItem
	open := true
	for open || len(pending) > 0 {
		if open {
			// Record every immediately-available commit's dump-medium placement.
		drain:
			for {
				select {
				case it, ok := <-commitCh:
					if !ok {
						open = false
						break drain
					}
					if err := e.cat.AddArchive(slotMeta, dumpMedium, it.arch, it.pos); err != nil {
						return err
					}
					if buffering {
						pending = append(pending, it)
					}
				default:
					break drain
				}
			}
		}
		if len(pending) > 0 {
			it := pending[0]
			pending = pending[1:]
			if err := e.flushOne(landSession, slotMeta, dumpMedium, holdVol, it, gate, tr, logf); err != nil {
				gate.abort(err)
				return err
			}
			continue
		}
		if !open {
			break
		}
		// Nothing pending (or not buffering) and the dumpers are still going: block for the next.
		it, ok := <-commitCh
		if !ok {
			open = false
			continue
		}
		if err := e.cat.AddArchive(slotMeta, dumpMedium, it.arch, it.pos); err != nil {
			return err
		}
		if buffering {
			pending = append(pending, it)
		}
	}
	return nil
}

// flushOne copies one archive from the holding disk to the landing, then reclaims the holding
// copy (files + placement) and releases its back-pressure. CopyArchive records the landing
// placement inline (single-threaded here), so the archive is on the landing in the catalog
// before its holding copy is dropped — never absent.
func (e *Engine) flushOne(landSession *clerk.Session, slotMeta *record.Slot, holding string, holdVol media.Volume, it flushItem, gate *byteGate, tr *progress.Tracker, logf Logf) error {
	if tr != nil {
		tr.StartFlush(it.dleID)
	}
	ref := clerk.Ref{Slot: slotMeta.ID, DLE: it.arch.DLE, Level: it.arch.Level}
	rc, err := openArchiveAt(holdVol, ref, it.pos.Parts)
	if err != nil {
		return fmt.Errorf("flush %s L%d: read holding disk: %w", it.dleID, it.arch.Level, err)
	}
	if err := landSession.CopyArchive(it.arch, rc); err != nil {
		rc.Close()
		return fmt.Errorf("flush %s L%d to %q: %w", it.dleID, it.arch.Level, e.mediumName, err)
	}
	rc.Close()

	for _, pos := range archivePosFiles(it.pos) {
		if err := holdVol.RemoveFile(pos); err != nil {
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

// openArchiveAt reads an archive's parts straight from a live volume (the holding writer's own,
// whose index the dumpers keep current), concatenating them — the taper's read seam, bypassing
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

// firstErr returns the first non-nil error, in order.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
