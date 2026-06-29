package engine

import (
	"github.com/Niloen/nbackup/internal/accounting"
	"github.com/Niloen/nbackup/internal/media"
)

// newLedger wires an accounting.Accountant to the engine's catalog, config, landing
// profile, and the few closures the capacity/retention arithmetic cannot derive on
// its own. The engine's capacity/retention methods are thin pass-throughs to it
// (see internal/accounting).
func (e *Engine) newLedger() *accounting.Accountant {
	return accounting.New(accounting.Deps{
		Cat:            e.cat,
		Cfg:            e.cfg,
		Landing:        e.mediumName,
		LandingProfile: e.profile,
		LandingMinAge:  e.minAge,
		OpenVolume: func(n string) (media.Volume, error) {
			v, _, _, err := e.mediumVolume(n)
			return v, err
		},
		DisplayDLE: e.DisplayDLE,
	})
}
