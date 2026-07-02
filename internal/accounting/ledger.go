// Package accounting is NBackup's capacity-and-retention ledger: it answers what a
// medium holds against its capacity, what a prune could reclaim, and how much room a
// run may write. It is the read-mostly arithmetic the engine used to do inline,
// split out so the orchestrator depends only on a narrow Accountant. The engine's
// capacity/retention methods are thin pass-throughs to this package.
package accounting

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
)

// Deps is the slice of the orchestrator the capacity/retention arithmetic needs:
// the catalog (what is stored where), the config (media definitions, minimum ages),
// the landing medium's cached profile/min-age, and closures the engine binds for the
// few things the accountant cannot derive on its own (opening a volume, naming a DLE).
// It is exported so the engine can wire one; the Accountant embeds it directly.
type Deps struct {
	Cat            *catalog.Catalog
	Cfg            *config.Config
	Landing        string
	LandingProfile media.Profile
	LandingCost    media.Cost // the landing medium's pricing (dollar peer of the profile)
	LandingMinAge  time.Duration
	OpenVolume     func(name string) (media.Volume, error)
	DisplayDLE     func(slug string) string
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

// ProfileFor returns the capacity/reclamation profile for a named medium: the
// landing medium's cached profile, or one opened on demand for any other medium.
func (a *Accountant) ProfileFor(name string) (media.Profile, error) {
	if name == a.d.Landing {
		return a.d.LandingProfile, nil
	}
	d, ok := a.d.Cfg.Media[name]
	if !ok {
		return nil, fmt.Errorf("unknown medium %q", name)
	}
	return media.OpenProfile(d.Type, media.Options(d.ProfileOptions()))
}
