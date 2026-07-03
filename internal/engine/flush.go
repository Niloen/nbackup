package engine

import (
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/depot"
	"github.com/Niloen/nbackup/internal/media"
)

// flush.go wires the engine's fs/catalog into conductor.Flush, the amflush analogue that drains
// a crashed run's leftover holding-disk archives to their landings on the next dump. The recovery
// logic lives in the conductor (the run lane); the engine supplies only the host-bound seams — the
// fs's read/reclaim paths and opening a landing session.

// Flush drains a crashed run's leftover holding-disk archives to their landings. It is idempotent
// and a no-op when no holding disk is configured or nothing is staged.
func (e *Engine) Flush(now time.Time, logf Logf) (int, error) {
	// Resolve each holding disk's volume once per call — Reclaim runs per staged archive.
	holdVols := map[string]media.Volume{}
	holdVol := func(name string) (media.Volume, error) {
		if v, ok := holdVols[name]; ok {
			return v, nil
		}
		v, _, _, err := e.dep.MediumVolume(name)
		if err == nil {
			holdVols[name] = v
		}
		return v, err
	}
	// One opened write face per landing, shared across the crashed runs being drained —
	// conductor.Flush opens a writer per run×landing, but the write claim is per medium,
	// so the handle is memoized here and every run's Session is authored over it. Flush
	// takes no claim on the holding disks it reads: it is their sole owner in both
	// directions (it reads staged archives AND reclaims them), and its reclaim writes go
	// through the raw volume, not the librarian.
	landers := map[string]landerHandle{}
	defer func() {
		for _, lh := range landers {
			_ = lh.wm.Close()
		}
	}()
	return conductor.Flush(conductor.FlushDeps{
		Cat:        e.cat,
		LandingFor: e.landingForDLEName,
		Holdings:   e.cfg.HoldingMedia(),
		Open: func(runID, dle string, level int, medium string) (io.ReadCloser, error) {
			return e.fs.OpenArchive(archiveio.Ref{Run: runID, DLE: dle, Level: level}, medium)
		},
		Members: func(runID, dle string, level int) ([]string, error) {
			return e.fs.Members(archiveio.Ref{Run: runID, DLE: dle, Level: level})
		},
		Reclaim: func(holding string, ref archiveio.Ref, pos archiveio.ArchivePos) error {
			vol, err := holdVol(holding)
			if err != nil {
				return err
			}
			return archivefs.ReclaimStaged(e.cat, holding, vol, ref, pos)
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

// landerHandle pairs a landing's opened write face with its config definition, so Flush
// can author one Session per crashed run over a single per-landing claim.
type landerHandle struct {
	wm  depot.WriteMedium
	def config.Media
}
