package engine

import (
	"github.com/Niloen/nbackup/internal/accounting"
	"github.com/Niloen/nbackup/internal/media"
)

// newLedger wires an accounting.Accountant to the engine's catalog, config, landing
// name, and the few closures the capacity/retention arithmetic cannot derive on
// its own. The engine's capacity/retention methods are thin pass-throughs to it
// (see internal/accounting).
func (e *Engine) newLedger() *accounting.Accountant {
	return accounting.New(accounting.Deps{
		Cat:     e.cat,
		Cfg:     e.cfg,
		Landing: e.dep.LandingName(),
		OpenVolume: func(n string) (media.Volume, error) {
			v, _, _, err := e.dep.MediumVolume(n)
			return v, err
		},
		// The fs's delete handle on a medium: a session over the raw volume. Prune runs
		// single-owner outside any run window (the claim guards in-process windows only),
		// so like the rest of the ledger it uses the claim-exempt volume; the footer-first
		// delete mechanics still live in the one place, the session's ReclaimAt.
		OpenReclaimer: func(n string) (accounting.Reclaimer, error) {
			v, _, _, err := e.dep.MediumVolume(n)
			if err != nil {
				return nil, err
			}
			return e.fs.OpenRun(e.cat, rawMedium{name: n, vol: v}), nil
		},
		DisplayDLE:    e.DisplayDLE,
		PlacementsFor: e.placementsFor,
		LandingLabeled: func() bool {
			am, _, err := e.dep.OpenAdmin(e.dep.LandingName())
			if err != nil {
				return false
			}
			defer am.Close()
			return am.Labeled()
		},
	})
}

// rawMedium adapts a claim-exempt raw volume to the archivefs write session's Medium —
// the ledger's maintenance flows (prune, forced-re-copy reclaim) hold no run window,
// so they session over the bare handle rather than an opened write face.
type rawMedium struct {
	name string
	vol  media.Volume
}

func (m rawMedium) Name() string         { return m.name }
func (m rawMedium) Volume() media.Volume { return m.vol }
