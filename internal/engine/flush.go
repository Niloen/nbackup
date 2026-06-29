package engine

import (
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/spool"
)

// flush.go wires the engine's writers/catalog into spool.Flush, the amflush analogue that drains a
// crashed run's leftover holding-disk archives to the landing on the next dump. The recovery logic
// lives in package drain alongside the live drain; the engine supplies only the host-bound seams
// (resolving a holding volume and opening a landing session).

// Flush drains a crashed run's leftover holding-disk archives to the landing. It is idempotent and
// a no-op when no holding disk is configured or nothing is staged.
func (e *Engine) Flush(now time.Time, logf Logf) (int, error) {
	return spool.Flush(spool.FlushDeps{
		Cat:      e.cat,
		Clerk:    e.clerk,
		Backing:  e.mediumName,
		Holdings: e.cfg.HoldingMedia(),
		HoldVol: func(name string) (media.Volume, error) {
			vol, _, _, err := e.mediumVolume(name)
			return vol, err
		},
		OpenBacking: func(spec archiveio.SlotSpec) (*clerk.Session, error) {
			wt, err := e.prepareWriter(e.mediumName, spec, now, logf)
			if err != nil {
				return nil, err
			}
			return e.clerk.OpenSlot(wt.w, e.mediumName), nil
		},
		DisplayDLE: e.DisplayDLE,
		Logf:       logf,
	}, now)
}
