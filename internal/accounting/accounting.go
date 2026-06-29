// Package accounting is NBackup's capacity-and-retention ledger: it answers what a
// medium holds against its capacity, what a prune could reclaim, and how much room a
// run may write. It is the read-mostly arithmetic the engine used to do inline,
// split out so the orchestrator depends only on a narrow Ledger.
package accounting

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/retention"
	"github.com/Niloen/nbackup/internal/sizeutil"
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
func (l *Ledger) Capacity() int64 { return l.landingProfile.TotalBytes() }

// CapacityStatus reports whether current usage exceeds capacity and the percent
// used (0 when unbounded).
func (l *Ledger) CapacityStatus(current int64) (over bool, pct float64) {
	c := l.landingProfile.TotalBytes()
	if c <= 0 {
		return false, 0
	}
	return current > c, float64(current) / float64(c) * 100
}

// StoredBytes is the bytes currently stored on the engine's own medium.
func (l *Ledger) StoredBytes() int64 { return l.cat.MediumBytes(l.landing) }

// MediumAppendable reports whether a medium packs many runs per volume (the
// default) rather than one run per volume — so inventory can label a written
// non-appendable reel "used" instead of "append".
func (l *Ledger) MediumAppendable(name string) bool {
	if m, ok := l.cfg.Media[name]; ok {
		return m.IsAppendable()
	}
	return true
}

// Media returns a summary of every configured medium, sorted by name.
func (l *Ledger) Media() []MediumInfo {
	names := make([]string, 0, len(l.cfg.Media))
	for n := range l.cfg.Media {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]MediumInfo, 0, len(names))
	for _, n := range names {
		info, _ := l.Medium(n)
		out = append(out, info)
	}
	return out
}

// Medium returns the summary for one configured medium; ok is false if the name
// is unknown.
func (l *Ledger) Medium(name string) (MediumInfo, bool) {
	d, ok := l.cfg.Media[name]
	if !ok {
		return MediumInfo{}, false
	}
	info := MediumInfo{
		Name:  name,
		Type:  d.Type,
		Slots: len(l.cat.SlotsOn(name)),
		Used:  l.cat.MediumBytes(name),
	}
	if prof, err := media.OpenProfile(d.Type, media.Options(d.ProfileOptions())); err == nil {
		info.Capacity = prof.TotalBytes()
	}
	// Summarize the medium's labeled volumes from the catalog (no medium type
	// special-casing): address-identified media (disk, s3) carry no label so the
	// pool is empty and Volume stays ""; a single labeled volume shows its name and
	// epoch; a pool of several (a tape library/station) shows the count, with the
	// per-volume detail in `nb medium <name>`.
	switch pool := l.volumesInPool(name); len(pool) {
	case 0:
		// nothing labeled (address-identified, or a still-blank changer)
	case 1:
		info.Volume, info.Epoch = pool[0].Label.Name, pool[0].Label.Epoch
	default:
		info.Volume = fmt.Sprintf("%d volume(s)", len(pool))
	}
	return info, true
}

// MediumOverCapacity reports whether the named medium still holds more than its
// capacity (a 0 capacity means unbounded). used and capacity are returned for
// messaging — used after a prune to tell the operator that reclaiming every dead
// archive was not enough because the protected recovery set alone exceeds capacity.
func (l *Ledger) MediumOverCapacity(name string) (over bool, used, capacity int64, err error) {
	prof, err := l.ProfileFor(name)
	if err != nil {
		return false, 0, 0, err
	}
	capacity = prof.TotalBytes()
	used = l.cat.MediumBytes(name)
	return capacity > 0 && used > capacity, used, capacity, nil
}

// MediumProtectedOverCapacity reports whether the bytes a prune *cannot* reclaim —
// the protected recovery set — still exceed the medium's capacity. It subtracts
// everything Reclaim would free from the current total, so the answer is the same
// whether or not a real prune has run: a dry-run still sees the would-delete archives
// in the catalog while a completed prune has already removed them, but
// `residual = current − reclaimable` is identical either way (after a real prune the
// reclaimable set is empty and the current total is already the residual). This is
// what `nb prune` warns on, so its preview and its real run agree.
func (l *Ledger) MediumProtectedOverCapacity(name string, now time.Time) (over bool, residual, capacity int64, err error) {
	prof, err := l.ProfileFor(name)
	if err != nil {
		return false, 0, 0, err
	}
	def, ok := l.cfg.Media[name]
	if !ok {
		return false, 0, 0, fmt.Errorf("unknown medium %q", name)
	}
	capacity = prof.TotalBytes()
	archives := l.cat.ArchivesOn(name)
	floor := retention.Compute(archives, l.cfg.MinAgeFor(def), now)
	var reclaimable int64
	for _, r := range prof.Reclaim(archives, floor, now) {
		reclaimable += r.Bytes
	}
	residual = l.cat.MediumBytes(name) - reclaimable
	return capacity > 0 && residual > capacity, residual, capacity, nil
}

// MediumProtectionIsAgeBound reports whether every archive pinning the medium over
// capacity is held by the minimum_age floor (vs a live recovery chain). When false,
// advising the operator to shorten minimum_age is useless — a DLE's last full and its
// later incrementals are pinned regardless of age — so the remedy text drops it.
func (l *Ledger) MediumProtectionIsAgeBound(name string, now time.Time) bool {
	def, ok := l.cfg.Media[name]
	if !ok {
		return true
	}
	archives := l.cat.ArchivesOn(name)
	floor := retention.Compute(archives, l.cfg.MinAgeFor(def), now)
	for _, a := range archives {
		reason, ok := floor.ReasonArchive(a.Slot, a.DLE)
		if ok && !strings.Contains(reason, "minimum age") {
			return false // a recovery-chain pin that shortening minimum_age can't release
		}
	}
	return true
}

// ProjectedOverCapacity reports whether the named medium would exceed its capacity
// after add more bytes land on it (a 0 capacity means unbounded) — the check
// `nb copy` runs before/after a copy so it warns about overshooting a target's
// budget the way `nb sync` already does.
func (l *Ledger) ProjectedOverCapacity(name string, add int64) (over bool, projected, capacity int64, err error) {
	prof, err := l.ProfileFor(name)
	if err != nil {
		return false, 0, 0, err
	}
	capacity = prof.TotalBytes()
	projected = l.cat.MediumBytes(name) + add
	return capacity > 0 && projected > capacity, projected, capacity, nil
}

// Prune reconciles a named medium to its own retention model: it computes that
// medium's protected slots (its own minimum_age and last-recovery-path floor) and
// asks its retention strategy which non-protected slots to reclaim to fit its
// capacity. Retention is per-medium, so each store is pruned against its own slots
// — pruning one medium never touches a copy on another. Any configured medium can
// be pruned (not only the landing one), so an offsite tier can be trimmed too.
func (l *Ledger) Prune(mediumName string, now time.Time, apply bool, logf logf.Logf) (eligible int, freed int64, err error) {
	def, ok := l.cfg.Media[mediumName]
	if !ok {
		return 0, 0, fmt.Errorf("unknown medium %q", mediumName)
	}
	profile, err := l.ProfileFor(mediumName)
	if err != nil {
		return 0, 0, err
	}
	minAge := l.cfg.MinAgeFor(def)
	archives := l.cat.ArchivesOn(mediumName)
	floor := retention.Compute(archives, minAge, now)

	// Reclamation is per archive (slot+DLE): a medium's Reclaim walks the oldest
	// non-protected archives, so an old slot can lose one DLE's image while keeping
	// another the chain still needs.
	type archiveRef struct{ slot, dle string }
	reclaim := map[archiveRef]media.Reclamation{}
	for _, r := range profile.Reclaim(archives, floor, now) {
		reclaim[archiveRef{r.SlotID, r.DLE}] = r
	}

	for _, a := range archives {
		if _, ok := reclaim[archiveRef{a.Slot, a.DLE}]; ok {
			continue // reported below
		}
		if reason, ok := floor.ReasonArchive(a.Slot, a.DLE); ok {
			logf.Log("keep   %s %s  (%s)", a.Slot, l.displayDLE(a.DLE), reason)
		} else {
			logf.Log("keep   %s %s  (fits capacity)", a.Slot, l.displayDLE(a.DLE))
		}
	}

	// Open the medium's volume only when there is something to actually delete.
	var vol media.Volume
	if apply && len(reclaim) > 0 {
		if vol, err = l.openVolume(mediumName); err != nil {
			return eligible, freed, err
		}
	}
	for _, a := range archives {
		r, ok := reclaim[archiveRef{a.Slot, a.DLE}]
		if !ok {
			continue
		}
		eligible++
		if apply {
			// Reclaim this archive's copy on this medium only — its files, one
			// position at a time; the slot (and the archive's copies elsewhere)
			// survives in the catalog.
			for _, pos := range archivePositions(l.cat.Placements(a.Slot), mediumName, a.DLE) {
				if err := vol.RemoveFile(pos); err != nil {
					return eligible, freed, fmt.Errorf("delete %s %s: %w", a.Slot, a.DLE, err)
				}
			}
			if _, _, err := l.cat.RemoveArchive(a.Slot, mediumName, a.DLE); err != nil {
				return eligible, freed, fmt.Errorf("update catalog cache: %w", err)
			}
			freed += r.Bytes
			logf.Log("DELETE %s %s  (%s freed, %s)", a.Slot, l.displayDLE(a.DLE), sizeutil.FormatBytes(r.Bytes), r.Note)
		} else {
			logf.Log("would delete %s %s  (%s, %s)", a.Slot, l.displayDLE(a.DLE), sizeutil.FormatBytes(r.Bytes), r.Note)
		}
	}
	return eligible, freed, nil
}

// PoolRoom is the retention bound: capacity minus the bytes pruning cannot
// reclaim (the protected set). Negative = unbounded (no pool budget).
func (l *Ledger) PoolRoom(now time.Time) int64 {
	capacity := l.landingProfile.TotalBytes()
	if capacity <= 0 {
		return -1
	}
	slots := l.cat.SlotsOn(l.landing)
	floor := retention.Compute(l.cat.ArchivesOn(l.landing), l.landingMinAge, now)
	var keptBytes int64
	for _, s := range slots {
		if floor.Keeps(s.ID) {
			keptBytes += s.TotalBytes()
		}
	}
	if room := capacity - keptBytes; room > 0 {
		return room
	}
	return 0
}

// ProfileFor returns the capacity/reclamation profile for a named medium: the
// landing medium's cached profile, or one opened on demand for any other medium.
func (l *Ledger) ProfileFor(name string) (media.Profile, error) {
	if name == l.landing {
		return l.landingProfile, nil
	}
	d, ok := l.cfg.Media[name]
	if !ok {
		return nil, fmt.Errorf("unknown medium %q", name)
	}
	return media.OpenProfile(d.Type, media.Options(d.ProfileOptions()))
}

// ReclaimCopy deletes an existing copy of a slot on a removable (fslike: disk
// or cloud) medium, so a forced re-copy replaces the old files instead of orphaning
// them (the leak a plain `nb copy --force` would otherwise cause — orphaned parts
// that no placement references yet still consume capacity). Tape reclaims only whole
// volumes (relabel), so its prior copy stays orphaned-until-relabel as documented and
// this is a no-op there. Best-effort: it runs before the re-copy re-authors the slot.
func (l *Ledger) ReclaimCopy(slotID, mediumName string) error {
	if m, ok := l.cfg.Media[mediumName]; ok && m.Type == "tape" {
		return nil
	}
	s, err := l.cat.ReadSlot(slotID)
	if err != nil {
		return err
	}
	vol, err := l.openVolume(mediumName)
	if err != nil {
		return err
	}
	for _, a := range s.Archives {
		for _, pos := range archivePositions(l.cat.Placements(slotID), mediumName, a.DLE) {
			if err := vol.RemoveFile(pos); err != nil {
				return fmt.Errorf("reclaim prior copy of %s %s on %q: %w", slotID, a.DLE, mediumName, err)
			}
		}
	}
	if _, err := l.cat.RemovePlacement(slotID, mediumName); err != nil {
		return fmt.Errorf("update catalog cache: %w", err)
	}
	return nil
}

// archivePositions gathers the volume file positions of one archive (a DLE's image)
// in the copy of a slot on medium, in safe removal order: commit footer first, then
// the member index, then the parts.
//
// The order is crash-safety-critical and mirrors the write order in reverse. An
// archive is made durable by its commit footer, written LAST (after its parts and
// index); the footer's presence is what proves the whole archive landed, and a
// catalog rebuild assembles only archives that have a footer (assemble iterates the
// commits — parts without one are orphans it ignores). So removing the footer FIRST
// "un-commits" the archive: a crash mid-prune then leaves parts/index as orphans with
// no footer, which a rebuild skips. Removing parts first would leave a footer whose
// parts are gone — which a rebuild would resurrect into the catalog as a committed-
// but-unreadable archive (the exact "we think it's committed but it's only partly
// there" hazard). Removal is one os.Remove per file, so the ordering holds at the same
// level the write path relies on (no fsync either side).
func archivePositions(ps []catalog.Placement, medium, dle string) []int {
	for _, p := range ps {
		if p.Medium != medium {
			continue
		}
		for _, a := range p.Archives {
			if a.DLE != dle {
				continue
			}
			pos := make([]int, 0, len(a.Parts)+2)
			pos = append(pos, a.Commit.Pos) // the marker: un-commit first
			if a.Index != (record.FilePos{}) {
				pos = append(pos, a.Index.Pos)
			}
			for _, pt := range a.Parts {
				pos = append(pos, pt.Pos)
			}
			return pos
		}
	}
	return nil
}
