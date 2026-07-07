package accounting

import (
	"fmt"
	"github.com/Niloen/nbackup/internal/archiveio"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
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
	profile, err := a.ProfileFor(mediumName)
	if err != nil {
		return 0, 0, 0, err
	}
	return a.reclaimTo(mediumName, a.capacityFor(mediumName, profile), now, apply, out)
}

// MakeRoom frees space on a bounded medium for incoming bytes BEFORE they land —
// capacity as a promise: the fslike sibling of tape's recycle-at-write rotation,
// run by `nb dump` (plan estimate) and `nb copy`/`nb sync` (exact bytes) so a
// write never overshoots the declared capacity and then waits for a janitor.
// It reclaims the oldest Floor-cleared archives down to capacity−incoming, and
// FAILS LOUD — before any byte is written — when the protected recovery set plus
// the incoming bytes cannot fit (see planRoom). A no-op for an unbounded medium
// or a labeled pool (whose rotation reclaims at the write itself).
func (a *Accountant) MakeRoom(mediumName string, incoming int64, now time.Time, out logf.Logf) (freed int64, err error) {
	plan, err := a.planRoom(mediumName, incoming, now)
	if err != nil || !plan.need {
		return 0, err
	}
	out.Log("make room on %q: reclaiming to fit ~%s under %s (%s used)",
		mediumName, sizeutil.FormatBytes(incoming), sizeutil.FormatBytes(plan.capacity), sizeutil.FormatBytes(plan.used))
	_, _, freed, err = a.reclaimTo(mediumName, plan.target, now, true, out)
	return freed, err
}

// MakeRoomPreview reports what MakeRoom would do for incoming bytes on medium
// without touching anything: (0, 0, nil) when the write fits as-is, the bytes and
// archive count a reclaim would free, or the same fail-loud error MakeRoom raises
// — `nb plan`'s dry view of the dump's pre-write step, so the plan shows what
// tonight costs in history.
func (a *Accountant) MakeRoomPreview(mediumName string, incoming int64, now time.Time) (freed int64, archives int, err error) {
	plan, err := a.planRoom(mediumName, incoming, now)
	if err != nil || !plan.need {
		return 0, 0, err
	}
	profile, err := a.ProfileFor(mediumName)
	if err != nil {
		return 0, 0, err
	}
	def := a.d.Cfg.Media[mediumName]
	all := a.d.Cat.ArchivesOn(mediumName)
	floor := retention.Compute(all, a.d.Cfg.MinAgeFor(def), now)
	for _, r := range profile.Reclaim(plan.target, all, floor, now) {
		freed += r.Bytes
		archives++
	}
	return freed, archives, nil
}

// roomPlan is the make-room decision MakeRoom executes and MakeRoomPreview
// reports: whether a reclaim is needed and down to what target.
type roomPlan struct {
	capacity, used, target int64
	need                   bool
}

// planRoom decides the make-room step for incoming bytes on a medium — the one
// place the fits/reclaim/infeasible triage lives. Infeasible (the protected
// recovery set plus the incoming bytes exceed capacity) is the returned error:
// pruning could never free enough, so the honest answers are more capacity or
// trimmed retention.
func (a *Accountant) planRoom(mediumName string, incoming int64, now time.Time) (roomPlan, error) {
	profile, err := a.ProfileFor(mediumName)
	if err != nil {
		return roomPlan{}, err
	}
	capacity := a.capacityFor(mediumName, profile)
	if capacity <= 0 || incoming <= 0 {
		return roomPlan{}, nil
	}
	used := a.d.Cat.MediumBytes(mediumName)
	if used+incoming <= capacity {
		return roomPlan{capacity: capacity, used: used}, nil // fits as-is
	}
	residual, _, err := a.MediumProtected(mediumName, now)
	if err != nil {
		return roomPlan{}, err
	}
	if residual+incoming > capacity {
		return roomPlan{}, fmt.Errorf("medium %q: capacity %s cannot hold the incoming ~%s plus the %s retention protects — increase capacity or trim retention",
			mediumName, sizeutil.FormatBytes(capacity), sizeutil.FormatBytes(incoming), sizeutil.FormatBytes(residual))
	}
	return roomPlan{capacity: capacity, used: used, target: capacity - incoming, need: true}, nil
}

// reclaimTo is the shared reclamation core behind Prune (target = capacity) and
// MakeRoom (target = capacity − incoming): delete the oldest Floor-cleared
// archives until the medium's stored bytes fit the target.
func (a *Accountant) reclaimTo(mediumName string, target int64, now time.Time, apply bool, out logf.Logf) (eligible int, swept int, freed int64, err error) {
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
	// another the chain still needs. The map is keyed by archiveio.Ref with Level left
	// zero on both sides — (run,DLE) already names an archive uniquely.
	reclaim := map[archiveio.Ref]media.Reclamation{}
	for _, r := range profile.Reclaim(target, archives, floor, now) {
		reclaim[archiveio.Ref{Run: r.RunID, DLE: r.DLE}] = r
	}

	for _, ar := range archives {
		if _, ok := reclaim[archiveio.Ref{Run: ar.Run, DLE: ar.DLE}]; ok {
			continue // reported below
		}
		if reason, ok := floor.ReasonArchive(ar.Run, ar.DLE); ok {
			out.Log("keep   %s %s  (%s)", ar.Run, a.d.DisplayDLE(ar.DLE), reason)
		} else {
			out.Log("keep   %s %s  (dead, retained — fits capacity)", ar.Run, a.d.DisplayDLE(ar.DLE))
		}
	}

	// Open the fs's delete handle only when there is something to actually delete.
	var rec Reclaimer
	if apply && len(reclaim) > 0 {
		if rec, err = a.d.OpenReclaimer(mediumName); err != nil {
			return eligible, swept, freed, err
		}
	}
	for _, ar := range archives {
		r, ok := reclaim[archiveio.Ref{Run: ar.Run, DLE: ar.DLE}]
		if !ok {
			continue
		}
		eligible++
		if apply {
			// Reclaim this archive's copy on this medium only; the run (and the
			// archive's copies elsewhere) survives in the catalog.
			if err := a.reclaimArchive(rec, mediumName, ar.Run, ar.DLE, ar.Level); err != nil {
				return eligible, swept, freed, fmt.Errorf("delete %s %s: %w", ar.Run, ar.DLE, err)
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
//
// Detection reads the medium, but only the SURPRISES: the catalog's own placements name
// every position it already accounts for on this medium (knownPositions, no I/O), and
// OrphanFiles reads headers/footers only for the positions the catalog does not already
// hold. On a healthy store the surprise set is empty, so the sweep is nearly free even on
// a large cloud bucket — instead of a network round trip per object. Excluding the known
// set only ever narrows what is read/deleted, never the reverse, so the medium-truth
// safety property is intact: a surprise is deleted only after its own (absent) commit
// footer is confirmed, and an empty catalog degrades to a full medium scan.
func (a *Accountant) sweepOrphans(mediumName string, minAge time.Duration, now time.Time, apply bool, out logf.Logf) (swept int, err error) {
	vol, err := a.d.OpenVolume(mediumName)
	if err != nil {
		return 0, err
	}
	orphans, err := catalog.OrphanFiles(vol, a.knownPositions(mediumName))
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

// knownPositions returns every file position the catalog already accounts for on a
// medium — each placed archive's parts, its commit footer, and its member index. It is
// what the orphan sweep excludes from its medium read: only positions NOT in this set
// (the "surprises") are worth opening, so a healthy store is diffed for free rather than
// re-read object by object. It reads only the in-memory catalog, no medium I/O. An empty
// result (a lost or empty cache) simply means nothing is excluded, so OrphanFiles falls
// back to a full medium-truth scan — the safe degradation.
func (a *Accountant) knownPositions(medium string) map[int]bool {
	known := map[int]bool{}
	for _, run := range a.d.Cat.RunsOn(medium) {
		for _, p := range a.d.Cat.Placements(run.ID) {
			if p.Medium != medium {
				continue
			}
			for _, ar := range p.Archives {
				for _, pt := range ar.Parts {
					known[pt.Pos] = true
				}
				known[ar.Commit.Pos] = true
				if ar.Index != (archiveio.FilePos{}) {
					known[ar.Index.Pos] = true
				}
			}
		}
	}
	return known
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
	rec, err := a.d.OpenReclaimer(mediumName)
	if err != nil {
		return err
	}
	// Per archive; the catalog drops the placement with its last archive, and the run
	// entry with its last placement, so no separate placement removal is needed.
	for _, ar := range s.Archives {
		if err := a.reclaimArchive(rec, mediumName, runID, ar.DLE, ar.Level); err != nil {
			return fmt.Errorf("reclaim prior copy of %s %s on %q: %w", runID, ar.DLE, mediumName, err)
		}
	}
	return nil
}

// Reclaimer is the accountant's slice of the fs's delete handle on one medium:
// ReclaimAt deletes an archive's copy there — its files footer-first, then its
// catalog placement (archivefs.Session implements it; the engine binds Deps.
// OpenReclaimer). The accountant only decides which archives die; how one dies
// lives in the fs.
type Reclaimer interface {
	ReclaimAt(ref archiveio.Ref, pos archiveio.ArchivePos) error
}

// reclaimArchive deletes one archive's copy on medium through rec, resolving the
// archive's recorded positions from its catalog placement. An archive the placement
// does not hold is a no-op (a partial copy being replaced, for instance).
func (a *Accountant) reclaimArchive(rec Reclaimer, medium, runID, dle string, level int) error {
	for _, p := range a.d.Cat.Placements(runID) {
		if p.Medium != medium {
			continue
		}
		for _, pa := range p.Archives {
			if pa.DLE == dle {
				return rec.ReclaimAt(archiveio.Ref{Run: runID, DLE: dle, Level: level}, pa.Pos())
			}
		}
	}
	return nil
}
