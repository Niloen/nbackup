// Package accounting answers four families of questions over the catalog and the
// configured media:
//
//   - current state — what a medium holds now (medium.go), and whether that fits its
//     capacity (capacity.go);
//   - the promise — making room BEFORE a write, and the per-run ceiling the planner
//     gets (prune.go, expectation.go);
//   - projection — replaying the simulated schedule forward: per-medium fill, tape
//     rotation, restore depth (projection.go is the kernel, forecast*.go the readers);
//   - pricing — the dollar overlay on all of the above (cost.go).
//
// Projection never invents aging: every simulated day runs the same
// retention.Compute + Profile.Reclaim the real prune uses. The package is the
// read-mostly arithmetic the engine used to do inline, split out so the orchestrator
// depends only on a narrow Accountant; the engine's methods are thin pass-throughs
// to it.
package accounting

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
)

// Deps is the slice of the orchestrator the accounting arithmetic needs: the catalog
// (what is stored where), the config (media definitions, minimum ages), the primary
// landing medium's NAME (every medium's profile/cost/min-age resolve per medium from
// the config — the landing is just the default), and closures the engine binds for
// the few things the accountant cannot derive on its own (opening a volume, naming a
// DLE). It is exported so the engine can wire one; the Accountant embeds it directly.
type Deps struct {
	Cat        *catalog.Catalog
	Cfg        *config.Config
	Landing    string
	OpenVolume func(name string) (media.Volume, error)
	// OpenReclaimer opens the fs's delete handle on a medium — the one mechanism by which
	// an archive's copy dies (files footer-first, then its catalog placement). The engine
	// binds it to an archivefs session; Prune and ReclaimCopy decide *what* dies, never how.
	OpenReclaimer func(medium string) (Reclaimer, error)
	DisplayDLE    func(slug string) string
	// LandingLabeled reports whether the landing medium carries volume labels (tape);
	// address-identified media (disk, s3) have no tape to expect.
	LandingLabeled func() bool
	// PlacementsFor returns a run's copies in read-preference order (landing first);
	// the read-cost estimator uses it to price the copy a restore would actually read.
	PlacementsFor func(runID string) []catalog.Placement
}

// Accountant answers capacity, retention, and prune questions over a catalog and its
// configured media. It is read-mostly arithmetic; Prune/ReclaimCopy are the only
// methods that mutate (delete volume files and update the catalog cache).
type Accountant struct{ d Deps }

// New constructs an Accountant from its dependencies.
func New(d Deps) *Accountant { return &Accountant{d: d} }

// ProfileFor returns the capacity/reclamation profile for a named medium, built from
// its config. Every configured medium's profile options are validated at engine
// construction, so a build failure here means an unknown medium.
func (a *Accountant) ProfileFor(name string) (media.Profile, error) {
	d, ok := a.d.Cfg.Media[name]
	if !ok {
		return nil, fmt.Errorf("unknown medium %q", name)
	}
	return media.OpenProfile(d.Type, media.Options(d.ProfileOptions()))
}

// minAgeFor is a medium's retention minimum age from its config (the config default
// for an unknown name).
func (a *Accountant) minAgeFor(name string) time.Duration {
	return a.d.Cfg.MinAgeFor(a.d.Cfg.Media[name])
}
