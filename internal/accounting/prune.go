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
// medium's protected runs (its own minimum_age and last-recovery-path floor) and
// asks its retention strategy which non-protected runs to reclaim to fit its
// capacity. Retention is per-medium, so each store is pruned against its own runs
// — pruning one medium never touches a copy on another. Any configured medium can
// be pruned (not only the landing one), so an offsite tier can be trimmed too.
func (a *Accountant) Prune(mediumName string, now time.Time, apply bool, out logf.Logf) (eligible int, swept int, freed int64, err error) {
	def, ok := a.d.Cfg.Media[mediumName]
	if !ok {
		return 0, 0, 0, fmt.Errorf("unknown medium %q", mediumName)
	}
	profile, err := a.ProfileFor(mediumName)
	if err != nil {
		return 0, 0, 0, err
	}
	minAge := a.d.Cfg.MinAgeFor(def)
	archives := a.d.Cat.ArchivesOn(mediumName)
	floor := retention.Compute(archives, minAge, now)

	// Reclamation is per archive (run+DLE): a medium's Reclaim walks the oldest
	// non-protected archives, so an old run can lose one DLE's image while keeping
	// another the chain still needs. The map is keyed by record.Ref with Level left
	// zero on both sides — (run,DLE) already names an archive uniquely.
	reclaim := map[record.Ref]media.Reclamation{}
	for _, r := range profile.Reclaim(archives, floor, now) {
		reclaim[record.Ref{Run: r.RunID, DLE: r.DLE}] = r
	}

	for _, ar := range archives {
		if _, ok := reclaim[record.Ref{Run: ar.Run, DLE: ar.DLE}]; ok {
			continue // reported below
		}
		if reason, ok := floor.ReasonArchive(ar.Run, ar.DLE); ok {
			out.Log("keep   %s %s  (%s)", ar.Run, a.d.DisplayDLE(ar.DLE), reason)
		} else {
			out.Log("keep   %s %s  (dead, retained — fits capacity)", ar.Run, a.d.DisplayDLE(ar.DLE))
		}
	}

	// Open the medium's volume only when there is something to actually delete.
	var vol media.Volume
	if apply && len(reclaim) > 0 {
		if vol, err = a.d.OpenVolume(mediumName); err != nil {
			return eligible, swept, freed, err
		}
	}
	for _, ar := range archives {
		r, ok := reclaim[record.Ref{Run: ar.Run, DLE: ar.DLE}]
		if !ok {
			continue
		}
		eligible++
		if apply {
			// Reclaim this archive's copy on this medium only — its files, one
			// position at a time; the run (and the archive's copies elsewhere)
			// survives in the catalog.
			for _, pos := range archivePositions(a.d.Cat.Placements(ar.Run), mediumName, ar.DLE) {
				if err := vol.RemoveFile(pos); err != nil {
					return eligible, swept, freed, fmt.Errorf("delete %s %s: %w", ar.Run, ar.DLE, err)
				}
			}
			if _, _, err := a.d.Cat.RemoveArchive(ar.Run, mediumName, ar.DLE); err != nil {
				return eligible, swept, freed, fmt.Errorf("update catalog cache: %w", err)
			}
			freed += r.Bytes
			out.Log("DELETE %s %s  (%s freed, %s)", ar.Run, a.d.DisplayDLE(ar.DLE), sizeutil.FormatBytes(r.Bytes), r.Note)
		} else {
			out.Log("would delete %s %s  (%s, %s)", ar.Run, a.d.DisplayDLE(ar.DLE), sizeutil.FormatBytes(r.Bytes), r.Note)
		}
	}

	// Sweep crash leftovers: files no committed archive references, which a run that
	// died before writing its commit footer left behind. Retention above cannot see
	// them (assemble discards footer-less files, so the catalog never recorded them),
	// yet they still hold the store's capacity — and on an address-identified medium
	// nothing else reclaims them, so a prune is where they go.
	if media.ConcurrentWrite(def.Type) {
		if swept, err = a.sweepOrphans(mediumName, minAge, now, apply, out); err != nil {
			return eligible, swept, freed, err
		}
	}
	return eligible, swept, freed, nil
}

// sweepOrphans removes the files on a per-file-reclaim medium (disk, cloud) that belong
// to no committed archive — the crash detritus retention is blind to. It unions two
// cache-free sources: footer-less complete archives (catalog.OrphanFiles, detected from
// the medium's own commit footers) and torn appends (media.IncompleteEnumerator, a
// half-written file a scan never sees). Each is removed by position via RemoveFile, which
// then reclaims an emptied run for free.
//
// Three safety pillars:
//   - Detection is MEDIUM-TRUTH, never the catalog cache — a stale or empty cache can never
//     make a committed archive look orphaned, because an archive is referenced here only when
//     its own commit footer is present on the medium.
//   - The workdir lock (internal/lock, held by every mutating run including a real prune)
//     guarantees no dump is writing concurrently, so an uncommitted file is a dead leftover,
//     never a part in flight.
//   - It honors the medium's minimum_age exactly as retention does, and a refused delete is
//     tolerated, so it never fights immutable storage (S3 Object Lock, WORM): operators set
//     minimum_age >= their Object-Lock retention so a still-locked orphan is simply not
//     attempted, and any delete the storage still refuses is left for a later prune rather
//     than failing the run.
//
// Tape is excluded by the caller (ConcurrentWrite gate): it reclaims orphans at relabel.
func (a *Accountant) sweepOrphans(mediumName string, minAge time.Duration, now time.Time, apply bool, out logf.Logf) (swept int, err error) {
	vol, err := a.d.OpenVolume(mediumName)
	if err != nil {
		return 0, err
	}
	orphans, err := catalog.OrphanFiles(vol)
	if err != nil {
		return 0, err
	}

	// remove is best-effort: a refused delete is how immutable storage (S3 Object Lock,
	// WORM) reports a still-locked object, and sweeping is opportunistic cleanup, so it
	// warns and leaves the file for a later prune rather than failing the whole run.
	remove := func(pos int, desc string) {
		if !apply {
			out.Log("would sweep orphan at %d  (%s)", pos, desc)
			swept++
			return
		}
		if rerr := vol.RemoveFile(pos); rerr != nil {
			out.Log("WARN   could not sweep orphan at %d (%s): %v — left for a later prune", pos, desc, rerr)
			return
		}
		out.Log("SWEEP  orphan at %d reclaimed  (%s)", pos, desc)
		swept++
	}

	for _, f := range orphans {
		// Honor minimum_age like retention does: a crash leftover younger than the floor is
		// left alone. Age is the file's run stamp (Header.CreatedAt); with the default
		// minimum_age of 0 every orphan is eligible at once, unchanged from before.
		if minAge > 0 && now.Sub(f.Header.CreatedAt) < minAge {
			out.Log("keep   orphan at %d  (%s %s, within minimum_age)", f.Pos, f.Header.Run, a.d.DisplayDLE(f.Header.DLE))
			continue
		}
		remove(f.Pos, fmt.Sprintf("%s %s, no commit footer", f.Header.Run, a.d.DisplayDLE(f.Header.DLE)))
	}

	torn, ok := vol.(media.IncompleteEnumerator)
	if !ok {
		return swept, nil
	}
	positions, err := torn.IncompleteFiles()
	if err != nil {
		return swept, err
	}
	for _, pos := range positions {
		// A torn file carries no run stamp to age (its header may be the missing half), so it
		// cannot be minimum_age-gated; the best-effort remove tolerates an immutable-storage
		// refusal, so on a WORM medium it is simply retried on a later prune.
		remove(pos, "torn file, interrupted append")
	}
	return swept, nil
}

// ReclaimCopy deletes an existing copy of a run on a removable (fslike: disk
// or cloud) medium, so a forced re-copy replaces the old files instead of orphaning
// them (the leak a plain `nb copy --force` would otherwise cause — orphaned parts
// that no placement references yet still consume capacity). Tape reclaims only whole
// volumes (relabel), so its prior copy stays orphaned-until-relabel as documented and
// this is a no-op there. Best-effort: it runs before the re-copy re-authors the run.
func (a *Accountant) ReclaimCopy(runID, mediumName string) error {
	if m, ok := a.d.Cfg.Media[mediumName]; ok && m.Type == "tape" {
		return nil
	}
	s, err := a.d.Cat.ReadRun(runID)
	if err != nil {
		return err
	}
	vol, err := a.d.OpenVolume(mediumName)
	if err != nil {
		return err
	}
	for _, ar := range s.Archives {
		for _, pos := range archivePositions(a.d.Cat.Placements(runID), mediumName, ar.DLE) {
			if err := vol.RemoveFile(pos); err != nil {
				return fmt.Errorf("reclaim prior copy of %s %s on %q: %w", runID, ar.DLE, mediumName, err)
			}
		}
	}
	if _, err := a.d.Cat.RemovePlacement(runID, mediumName); err != nil {
		return fmt.Errorf("update catalog cache: %w", err)
	}
	return nil
}

// archivePositions gathers the volume file positions of one archive (a DLE's image)
// in the copy of a run on medium, in safe removal order: commit footer first, then
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
