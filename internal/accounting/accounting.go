// Package accounting is NBackup's capacity-and-retention ledger: it answers what a
// medium holds against its capacity, what a prune could reclaim, and how much room a
// run may write. It is the read-mostly arithmetic the engine used to do inline,
// split out so the orchestrator depends only on a narrow Ledger. The methods are
// stubs in this commit (the engine still does the real work); a later lane fills
// them in.
package accounting

import (
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
)

// MediumInfo is a per-medium summary for catalog visibility (`nb medium`): what
// the medium is, how much it holds against its capacity, and (for labeled media)
// the volume currently associated with it in the catalog.
type MediumInfo struct {
	Name     string
	Type     string
	Slots    int
	Used     int64
	Capacity int64  // 0 = unbounded
	Volume   string // label name; "" for address-identified media (disk, s3)
	Epoch    int
}

// Ledger holds the narrow slice of the orchestrator the capacity/retention
// arithmetic needs: the catalog (what is stored where), the config (media
// definitions, minimum ages), and closures the engine binds for the few things the
// ledger cannot derive on its own (opening a volume, resolving a pool, naming a DLE).
type Ledger struct {
	cat            *catalog.Catalog
	cfg            *config.Config
	landing        string
	landingProfile media.Profile
	landingMinAge  time.Duration
	openVolume     func(name string) (media.Volume, error)
	volumesInPool  func(medium string) []catalog.VolumeRecord
	displayDLE     func(slug string) string
}

// Deps is the exported mirror of the Ledger's dependency slice, so the engine can
// wire one without the fields being part of the package's public surface.
type Deps struct {
	Cat            *catalog.Catalog
	Cfg            *config.Config
	Landing        string
	LandingProfile media.Profile
	LandingMinAge  time.Duration
	OpenVolume     func(name string) (media.Volume, error)
	VolumesInPool  func(medium string) []catalog.VolumeRecord
	DisplayDLE     func(slug string) string
}

// New constructs a Ledger from its dependencies.
func New(d Deps) *Ledger {
	return &Ledger{
		cat:            d.Cat,
		cfg:            d.Cfg,
		landing:        d.Landing,
		landingProfile: d.LandingProfile,
		landingMinAge:  d.LandingMinAge,
		openVolume:     d.OpenVolume,
		volumesInPool:  d.VolumesInPool,
		displayDLE:     d.DisplayDLE,
	}
}

// Capacity returns the landing medium's total retainable bytes (0 = unbounded).
func (l *Ledger) Capacity() int64 { panic("accounting: not yet wired") }

// CapacityStatus reports whether current usage exceeds capacity and the percent
// used (0 when unbounded).
func (l *Ledger) CapacityStatus(current int64) (over bool, pct float64) {
	panic("accounting: not yet wired")
}

// StoredBytes is the bytes currently stored on the engine's own medium.
func (l *Ledger) StoredBytes() int64 { panic("accounting: not yet wired") }

// MediumAppendable reports whether a medium packs many runs per volume.
func (l *Ledger) MediumAppendable(name string) bool { panic("accounting: not yet wired") }

// Media returns a summary of every configured medium, sorted by name.
func (l *Ledger) Media() []MediumInfo { panic("accounting: not yet wired") }

// Medium returns the summary for one configured medium.
func (l *Ledger) Medium(name string) (MediumInfo, bool) { panic("accounting: not yet wired") }

// MediumOverCapacity reports whether a medium still holds more than its capacity.
func (l *Ledger) MediumOverCapacity(name string) (over bool, used, capacity int64, err error) {
	panic("accounting: not yet wired")
}

// MediumProtectedOverCapacity reports whether the bytes a prune cannot reclaim still
// exceed the medium's capacity.
func (l *Ledger) MediumProtectedOverCapacity(name string, now time.Time) (over bool, residual, capacity int64, err error) {
	panic("accounting: not yet wired")
}

// MediumProtectionIsAgeBound reports whether every archive pinning the medium over
// capacity is held by the minimum_age floor.
func (l *Ledger) MediumProtectionIsAgeBound(name string, now time.Time) bool {
	panic("accounting: not yet wired")
}

// ProjectedOverCapacity reports whether a medium would exceed capacity after add bytes.
func (l *Ledger) ProjectedOverCapacity(name string, add int64) (over bool, projected, capacity int64, err error) {
	panic("accounting: not yet wired")
}

// Prune reconciles a named medium to its own retention model.
func (l *Ledger) Prune(mediumName string, now time.Time, apply bool, lf logf.Logf) (eligible int, freed int64, err error) {
	panic("accounting: not yet wired")
}

// PoolRoom is the retention bound: capacity minus the bytes pruning cannot reclaim.
func (l *Ledger) PoolRoom(now time.Time) int64 { panic("accounting: not yet wired") }

// ProfileFor returns the capacity/reclamation profile for a named medium.
func (l *Ledger) ProfileFor(name string) (media.Profile, error) { panic("accounting: not yet wired") }

// ReclaimCopy deletes an existing copy of a slot on a removable medium.
func (l *Ledger) ReclaimCopy(slotID, mediumName string) error { panic("accounting: not yet wired") }
