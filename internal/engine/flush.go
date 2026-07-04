package engine

import (
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/depot"
	"github.com/Niloen/nbackup/internal/record"
)

// flush.go wires the engine's fs/catalog into conductor.Flush, the amflush analogue that drains
// a crashed run's leftover holding-disk archives to their landings on the next dump. The recovery
// logic lives in the conductor (the run lane); the engine supplies only the host-bound seams —
// each holding disk's write session and opening a landing session.

// Flush drains a crashed run's leftover holding-disk archives to their landings. It is idempotent
// and a no-op when no holding disk is configured or nothing is staged.
func (e *Engine) Flush(now time.Time, logf Logf) (int, error) {
	// One write session per holding disk, opened lazily and shared across the crashed runs
	// being drained — the same handle the live drain holds: the holding is write-claimed for
	// the duration, its staged archives are read back and reclaimed positionally through the
	// session, never through the catalog-resolving read face (never-read-own-writes, as ever).
	holdings := map[string]holdingHandle{}
	holding := func(name string) (holdingHandle, error) {
		if h, ok := holdings[name]; ok {
			return h, nil
		}
		wm, _, err := e.dep.OpenForWrite(name)
		if err != nil {
			return holdingHandle{}, err
		}
		h := holdingHandle{wm: wm, session: e.fs.OpenRun(e.cat, wm)}
		holdings[name] = h
		return h, nil
	}
	// One opened write face per landing, shared across the crashed runs being drained —
	// conductor.Flush opens a writer per run×landing, but the write claim is per medium,
	// so the handle is memoized here and every run's Session is authored over it.
	landers := map[string]landerHandle{}
	defer func() {
		for _, h := range holdings {
			_ = h.wm.Close()
		}
		for _, lh := range landers {
			_ = lh.wm.Close()
		}
	}()
	return conductor.Flush(conductor.FlushDeps{
		Cat:        e.cat,
		LandingFor: e.landingForDLEName,
		Holdings:   e.cfg.HoldingMedia(),
		Open: func(name string, ref archiveio.Ref, pos archiveio.ArchivePos) (io.ReadCloser, error) {
			h, err := holding(name)
			if err != nil {
				return nil, err
			}
			return h.session.OpenArchiveAt(ref, pos)
		},
		Members: func(name string, ref archiveio.Ref, index archiveio.FilePos) ([]record.Member, error) {
			// The member cache (or another copy) first; else read the index positionally off
			// the holding's own volume — its read face is refused while the session's write
			// claim holds it, exactly as during the live drain.
			if members, err := e.fs.Members(ref); err != nil || members != nil {
				return members, err
			}
			if index == (archiveio.FilePos{}) {
				return nil, nil
			}
			h, err := holding(name)
			if err != nil {
				return nil, err
			}
			_, rc, err := h.wm.Volume().ReadFile(index.Pos)
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return record.DecodeIndex(rc)
		},
		Reclaim: func(name string, ref archiveio.Ref, pos archiveio.ArchivePos) error {
			h, err := holding(name)
			if err != nil {
				return err
			}
			return h.session.ReclaimAt(ref, pos)
		},
		OpenLanding: func(landing string, spec archiveio.RunSpec) (*archiveio.Writer, error) {
			lh, ok := landers[landing]
			if !ok {
				wm, def, err := e.dep.OpenForWrite(landing)
				if err != nil {
					return nil, err
				}
				lh = landerHandle{wm: wm, def: def}
				landers[landing] = lh
			}
			wt, err := e.prepareWriterOn(lh.wm, lh.def, spec, now, logf)
			if err != nil {
				return nil, err
			}
			return wt.writer, nil
		},
		DisplayDLE: e.DisplayDLE,
		Logf:       logf,
	})
}

// holdingHandle pairs a holding disk's opened write face with the flush's Session over it —
// the staged archives' read-back and reclaim handle.
type holdingHandle struct {
	wm      depot.WriteMedium
	session *archivefs.Session
}

// landerHandle pairs a landing's opened write face with its config definition, so Flush
// can author one Session per crashed run over a single per-landing claim.
type landerHandle struct {
	wm  depot.WriteMedium
	def config.Media
}
