package engine

import (
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// flush.go wires the engine's clerk/catalog into conductor.Flush, the amflush analogue that drains
// a crashed run's leftover holding-disk archives to their landings on the next dump. The recovery
// logic lives in the conductor (the run lane); the engine supplies only the host-bound seams — the
// clerk's read/reclaim paths and opening a landing session.

// Flush drains a crashed run's leftover holding-disk archives to their landings. It is idempotent
// and a no-op when no holding disk is configured or nothing is staged.
func (e *Engine) Flush(now time.Time, logf Logf) (int, error) {
	// Resolve each holding disk's volume once per call — Reclaim runs per staged archive.
	holdVols := map[string]media.Volume{}
	holdVol := func(name string) (media.Volume, error) {
		if v, ok := holdVols[name]; ok {
			return v, nil
		}
		v, _, _, err := e.mediumVolume(name)
		if err == nil {
			holdVols[name] = v
		}
		return v, err
	}
	return conductor.Flush(conductor.FlushDeps{
		Cat:        e.cat,
		LandingFor: e.landingForDLEName,
		Holdings:   e.cfg.HoldingMedia(),
		Open: func(runID, dle string, level int, medium string) (io.ReadCloser, error) {
			return e.clerk.Open(clerk.Ref{Run: runID, DLE: dle, Level: level}, medium)
		},
		Members: func(runID, dle string, level int) ([]string, error) {
			return e.clerk.Members(clerk.Ref{Run: runID, DLE: dle, Level: level})
		},
		Reclaim: func(holding, runID, dle string, pos record.ArchivePos) error {
			vol, err := holdVol(holding)
			if err != nil {
				return err
			}
			return e.clerk.ReclaimStaged(holding, vol, runID, dle, pos)
		},
		OpenLanding: func(landing string, spec archiveio.RunSpec) (*archiveio.Author, error) {
			wt, err := e.prepareWriter(landing, spec, now, logf)
			if err != nil {
				return nil, err
			}
			return wt.writer, nil
		},
		DisplayDLE: e.DisplayDLE,
		Logf:       logf,
	})
}
