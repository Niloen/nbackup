package accounting

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/retention"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// Prune reconciles a named medium to its own retention model: it computes that
// medium's protected slots (its own minimum_age and last-recovery-path floor) and
// asks its retention strategy which non-protected slots to reclaim to fit its
// capacity. Retention is per-medium, so each store is pruned against its own slots
// — pruning one medium never touches a copy on another. Any configured medium can
// be pruned (not only the landing one), so an offsite tier can be trimmed too.
func (a *Accountant) Prune(mediumName string, now time.Time, apply bool, out logf.Logf) (eligible int, freed int64, err error) {
	def, ok := a.d.Cfg.Media[mediumName]
	if !ok {
		return 0, 0, fmt.Errorf("unknown medium %q", mediumName)
	}
	profile, err := a.ProfileFor(mediumName)
	if err != nil {
		return 0, 0, err
	}
	minAge := a.d.Cfg.MinAgeFor(def)
	archives := a.d.Cat.ArchivesOn(mediumName)
	floor := retention.Compute(archives, minAge, now)

	// Reclamation is per archive (slot+DLE): a medium's Reclaim walks the oldest
	// non-protected archives, so an old slot can lose one DLE's image while keeping
	// another the chain still needs.
	type archiveRef struct{ slot, dle string }
	reclaim := map[archiveRef]media.Reclamation{}
	for _, r := range profile.Reclaim(archives, floor, now) {
		reclaim[archiveRef{r.SlotID, r.DLE}] = r
	}

	for _, ar := range archives {
		if _, ok := reclaim[archiveRef{ar.Slot, ar.DLE}]; ok {
			continue // reported below
		}
		if reason, ok := floor.ReasonArchive(ar.Slot, ar.DLE); ok {
			out.Log("keep   %s %s  (%s)", ar.Slot, a.d.DisplayDLE(ar.DLE), reason)
		} else {
			out.Log("keep   %s %s  (fits capacity)", ar.Slot, a.d.DisplayDLE(ar.DLE))
		}
	}

	// Open the medium's volume only when there is something to actually delete.
	var vol media.Volume
	if apply && len(reclaim) > 0 {
		if vol, err = a.d.OpenVolume(mediumName); err != nil {
			return eligible, freed, err
		}
	}
	for _, ar := range archives {
		r, ok := reclaim[archiveRef{ar.Slot, ar.DLE}]
		if !ok {
			continue
		}
		eligible++
		if apply {
			// Reclaim this archive's copy on this medium only — its files, one
			// position at a time; the slot (and the archive's copies elsewhere)
			// survives in the catalog.
			for _, pos := range archivePositions(a.d.Cat.Placements(ar.Slot), mediumName, ar.DLE) {
				if err := vol.RemoveFile(pos); err != nil {
					return eligible, freed, fmt.Errorf("delete %s %s: %w", ar.Slot, ar.DLE, err)
				}
			}
			if _, _, err := a.d.Cat.RemoveArchive(ar.Slot, mediumName, ar.DLE); err != nil {
				return eligible, freed, fmt.Errorf("update catalog cache: %w", err)
			}
			freed += r.Bytes
			out.Log("DELETE %s %s  (%s freed, %s)", ar.Slot, a.d.DisplayDLE(ar.DLE), sizeutil.FormatBytes(r.Bytes), r.Note)
		} else {
			out.Log("would delete %s %s  (%s, %s)", ar.Slot, a.d.DisplayDLE(ar.DLE), sizeutil.FormatBytes(r.Bytes), r.Note)
		}
	}
	return eligible, freed, nil
}

// ReclaimCopy deletes an existing copy of a slot on a removable (fslike: disk
// or cloud) medium, so a forced re-copy replaces the old files instead of orphaning
// them (the leak a plain `nb copy --force` would otherwise cause — orphaned parts
// that no placement references yet still consume capacity). Tape reclaims only whole
// volumes (relabel), so its prior copy stays orphaned-until-relabel as documented and
// this is a no-op there. Best-effort: it runs before the re-copy re-authors the slot.
func (a *Accountant) ReclaimCopy(slotID, mediumName string) error {
	if m, ok := a.d.Cfg.Media[mediumName]; ok && m.Type == "tape" {
		return nil
	}
	s, err := a.d.Cat.ReadSlot(slotID)
	if err != nil {
		return err
	}
	vol, err := a.d.OpenVolume(mediumName)
	if err != nil {
		return err
	}
	for _, ar := range s.Archives {
		for _, pos := range archivePositions(a.d.Cat.Placements(slotID), mediumName, ar.DLE) {
			if err := vol.RemoveFile(pos); err != nil {
				return fmt.Errorf("reclaim prior copy of %s %s on %q: %w", slotID, ar.DLE, mediumName, err)
			}
		}
	}
	if _, err := a.d.Cat.RemovePlacement(slotID, mediumName); err != nil {
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
