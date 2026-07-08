// Package retention judges which backups must be kept — and which must go. It does
// not hold a policy: the knobs live in config (a medium's minimum_age) and the
// recovery-chain rule is an invariant, not a tunable. Compute applies those rules to
// one medium's runs at a moment in time and returns a Floor — the runs reclamation
// must never delete (runs younger than the medium's minimum age, and every run in a
// DLE's live recovery chain: its last full plus the later incrementals a restore
// replays), each with the reason it is pinned — and the archives it CONDEMNS: those
// no restore anywhere can use, which reclamation deletes regardless of capacity.
// Callers build the Floor once and query it, rather than threading a raw map
// around. It is pure and does no I/O.
//
// Retention is per-medium: callers pass the runs of a single medium, so "last
// recovery path" is judged within that medium alone. A copy on another medium
// never makes a run reclaimable here — double storage exists for redundancy,
// and each medium retains against its own capacity and cycle. The rule's shape
// is medium-neutral; only the run set it is applied to is medium-scoped.
package retention

import (
	"fmt"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// archiveRef identifies one archive within the floor: a run and the DLE whose
// image it holds. A run dumps each DLE once at one level, so (run, DLE) names an
// archive uniquely — the floor pins protection at this granularity, finer than the
// run, so an old run may keep one DLE's archive while another DLE's is reclaimed.
type archiveRef struct{ run, dle string }

// Kind classifies why the floor pins an archive — the typed identity of a pin,
// stable across any rewording of the rendered reason text. Precedence and any
// caller classification (e.g. "is this pin age-bound?") key on the Kind, never on
// the text. Declaration order is strength order: lower = stronger.
type Kind int

const (
	// KindAge — the archive is still within the medium's minimum_age.
	KindAge Kind = iota
	// KindLastFull — the archive is a DLE's last full, its last recovery path.
	KindLastFull
	// KindChain — the archive is in a DLE's pinned recovery chain.
	KindChain
)

// pin is one floor entry: the typed kind plus the rendered reason text.
type pin struct {
	kind Kind
	text string
}

// Floor is the retention floor computed for one medium's runs: the archives
// reclamation must never delete, each with the reason it is pinned. Build it once
// with Compute, then query it — per archive (KeepsArchive, ReasonArchive,
// KindArchive), per run (Keeps, Reason — "is any archive of the run pinned"), or
// by "is any of these runs pinned" (First). The zero Floor keeps nothing.
//
// The floor is per-archive because reclamation on address-identified media (disk,
// cloud) is per-archive; the run-level queries report a run as kept when any of
// its archives is pinned, which is what the whole-volume reclaimers (tape relabel,
// ExpectedTape) and the cost forecast still reason in.
type Floor struct {
	reasons   map[archiveRef]pin   // (run,DLE) -> pin; absent ⇒ reclaimable
	condemned map[archiveRef]error // (run,DLE) -> why no restore can use it; see Condemned
}

// Compute applies a medium's retention rules to its runs and returns the floor —
// the runs reclamation must never delete. Three rules combine:
//
//  1. Age: a run younger than minAge is kept, whatever its level.
//  2. Last recovery path: the last full of each DLE is kept, so at least one
//     recovery path for it never ages out.
//  3. Recovery chain: an incremental restore replays its full PLUS every later
//     incremental up to the target (see recovery.Chain), so a kept run pins the
//     whole chain its restore needs. Each DLE's latest run pins the live chain
//     after the last full (the tip and every point in between); each young run
//     pins the older base its restore depends on. So reclamation can never orphan
//     an incremental or break a chain it leaves restorable — an incremental is
//     kept because a chain needs it, never on its own.
//
// Pass one medium's runs to get that medium's floor.
//
// The floor also renders the opposite verdict: an archive of this medium that NO
// restore anywhere can use (recovery.Stranded over catalog — pass the whole
// catalog's archives, so a base copy on another medium keeps a copy here
// restorable) is CONDEMNED. The catalog is evidence only — it can spare an
// archive, never doom one, so keeping stays purely per-medium. The per-medium
// chain-safe reclaim order already prevents stranding WITHIN a medium;
// condemnation exists for the cross-medium seams that order cannot reach —
// split copies (a lone incremental synced or `nb copy`-ed without its base,
// whose base's last copy elsewhere then ages out), tape rotation retiring a
// base volume, and history from before the ordering rule. The two verdicts are exclusive — a condemned archive
// is never pinned, not even by the structural chain rule (recovery.Chain, which
// follows the recorded BaseRun, is the authority on restorability) — and a
// stranded archive never anchors a chain. One exception defers the verdict: an
// unrestorable archive still within minimum_age keeps its age pin (the same
// WORM/Object-Lock guard as ever, its reason saying why it lingers) and is
// condemned by a later Compute once aged. A nil catalog renders no condemnations.
//
// Note: once verification status is tracked, the successor requirement should
// tighten from "a newer full exists" to "a newer verified full exists".
func Compute(archives, catalog []record.Archive, minAge time.Duration, now time.Time) Floor {
	stranded := map[archiveRef]error{}
	for _, s := range recovery.Stranded(catalog) {
		stranded[archiveRef{s.Archive.Run, s.Archive.DLE}] = s.Err
	}
	condemned := map[archiveRef]error{}
	for _, a := range archives {
		if err, ok := stranded[archiveRef{a.Run, a.DLE}]; ok && !youngArchive(a, minAge, now) {
			condemned[archiveRef{a.Run, a.DLE}] = err
		}
	}
	reasons := map[archiveRef]pin{}
	keep := func(run, dle string, kind Kind, reason string) {
		ref := archiveRef{run, dle}
		if _, dead := condemned[ref]; dead {
			return // condemned and kept are exclusive verdicts
		}
		if _, ok := reasons[ref]; !ok {
			reasons[ref] = pin{kind, reason}
		}
	}
	// runYoung[id] reports whether any archive of the run is within the minimum age — the
	// run-level view the recovery-chain rule anchors on.
	runYoung := map[string]bool{}
	for _, a := range archives {
		if youngArchive(a, minAge, now) {
			runYoung[a.Run] = true
		}
	}
	// 1) Age floor: pin each archive still within the minimum age (per archive). A
	// young UNRESTORABLE archive says so — it is spared only by the WORM guard and
	// is condemned once aged, and its keep line should not read like a healthy one.
	for _, a := range archives {
		if !youngArchive(a, minAge, now) {
			continue
		}
		if err, ok := stranded[archiveRef{a.Run, a.DLE}]; ok {
			keep(a.Run, a.DLE, KindAge, fmt.Sprintf("unrestorable (%v) — within minimum age (%s), condemned once aged", err, sizeutil.FormatDuration(minAge)))
			continue
		}
		keep(a.Run, a.DLE, KindAge, fmt.Sprintf("within minimum age (%s)", sizeutil.FormatDuration(minAge)))
	}
	// 2) Last-recovery floor (kept distinct so an archive that is a DLE's last full
	// is reported by that full, not a mere incremental the run also carries).
	for _, a := range archives {
		if a.Level == 0 && !hasNewerFull(archives, a.DLE, a.Run) {
			// The reason omits the DLE: callers render it in the line's path
			// column (as host:path), so repeating the internal slug here is
			// redundant and inconsistent.
			keep(a.Run, a.DLE, KindLastFull, "last recovery path")
		}
	}
	// 3) Recovery-chain floor.
	//
	// Note: a newer full landing does NOT retroactively free an older, still-young
	// incremental's own base. Each young run is anchored to ITS full (walking back
	// from ai, not from the DLE's latest run), because that incremental was taken
	// against that base and restoring it as-of its own date still replays that exact
	// chain — a later full is a separate, independent recovery path, not a substitute
	// for the one the young run already committed to. So a full landing exactly on
	// the prune reference date does not "supersede away" an old-but-still-young
	// sibling chain; it only stops protecting archives once every young run anchored
	// to it has itself aged out of minimum_age. This is intentional over-retention
	// (recoverability first), not an off-by-one on the reference date.
	for _, dle := range dleNames(archives) {
		ds := record.ArchivesOf(archives, dle) // the dle's archives in run order, one per run
		// A stranded archive never anchors: its restore cannot happen, so there is
		// no chain to keep for it — pinning from an older, unrelated full up through
		// it would protect junk.
		anchored := func(i int) bool {
			_, dead := stranded[archiveRef{ds[i].Run, dle}]
			return !dead
		}
		anchors := map[int]bool{}
		if n := len(ds); n > 0 && anchored(n-1) {
			anchors[n-1] = true // the latest run: keeps the live chain (and its full)
		}
		for i, a := range ds {
			if runYoung[a.Run] && anchored(i) {
				anchors[i] = true // a recent run: keep the base its restore needs
			}
		}
		for ai := range anchors {
			full := -1
			for j := 0; j <= ai; j++ {
				if ds[j].Level == 0 {
					full = j
				}
			}
			if full < 0 {
				continue // no full at or before the anchor (cannot happen for a real chain)
			}
			for j := full; j <= ai; j++ {
				keep(ds[j].Run, dle, KindChain, "in this DLE's recovery chain")
			}
		}
	}
	return Floor{reasons: reasons, condemned: condemned}
}

// youngArchive reports whether the archive is still within the medium's minimum age.
// Age is measured per archive from when it committed (CreatedAt), not the run's
// date: the date is day-granular, so comparing it would collapse every minimum_age
// under 24h to a whole-day step. CreatedAt is the real landing instant, so a sub-day
// minimum_age keeps only archives actually that recent. A zero CreatedAt (older media)
// reads as not-young, i.e. reclaimable.
func youngArchive(a record.Archive, minAge time.Duration, now time.Time) bool {
	return minAge > 0 && !a.CreatedAt.IsZero() && now.Sub(a.CreatedAt) < minAge
}

// dleNames returns the distinct DLEs across the archives, sorted for determinism.
func dleNames(archives []record.Archive) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range archives {
		if !seen[a.DLE] {
			seen[a.DLE] = true
			out = append(out, a.DLE)
		}
	}
	sort.Strings(out)
	return out
}

// KeepsArchive reports whether the floor pins one archive (run+DLE), so per-archive
// reclamation must not delete it. It is the predicate a medium's Reclaim consults.
func (f Floor) KeepsArchive(run, dle string) bool {
	_, ok := f.reasons[archiveRef{run, dle}]
	return ok
}

// ReasonArchive returns why the floor pins one archive, and whether it pins it.
func (f Floor) ReasonArchive(run, dle string) (reason string, ok bool) {
	p, ok := f.reasons[archiveRef{run, dle}]
	return p.text, ok
}

// KindArchive returns the typed kind of the pin on one archive, and whether the
// floor pins it — the classification callers branch on (never the reason text).
func (f Floor) KindArchive(run, dle string) (Kind, bool) {
	p, ok := f.reasons[archiveRef{run, dle}]
	return p.kind, ok
}

// Condemned reports the archive the floor has given up on — no restore anywhere
// can use it — with recovery's error naming the broken link. Condemned and kept
// are exclusive: a condemned archive is never pinned, whatever the other rules
// would have said.
func (f Floor) Condemned(run, dle string) (error, bool) {
	err, ok := f.condemned[archiveRef{run, dle}]
	return err, ok
}

// CondemnsArchive is the predicate form of Condemned — what a medium's Reclaim
// consults to delete unrestorable archives first, regardless of the capacity
// target (media.Retention names it, mirroring KeepsArchive).
func (f Floor) CondemnsArchive(run, dle string) bool {
	_, ok := f.condemned[archiveRef{run, dle}]
	return ok
}

// Keeps reports whether the floor pins any archive of run id — the run-level view
// the whole-volume reclaimers (tape relabel, ExpectedTape) and the cost forecast
// reason in: a run is kept if reclaiming it would destroy any pinned archive.
func (f Floor) Keeps(id string) bool {
	_, ok := f.Reason(id)
	return ok
}

// Reason returns why the floor pins run id, and whether it pins any archive at all.
// When several archives pin the run it reports the strongest reason — age, then a
// DLE's last recovery path (its full), then a recovery chain — so a run that holds a
// DLE's last full is reported by that full, not by a mere incremental it also carries
// (the precedence Compute applies per archive, projected to the run). Ties within a
// rank break by DLE for a stable message.
func (f Floor) Reason(id string) (reason string, ok bool) {
	var bestKind Kind
	bestDLE := ""
	for ref, p := range f.reasons {
		if ref.run != id {
			continue
		}
		// Kind is the strength order (lower = stronger), so the run-level Reason
		// reports the same precedence Compute uses per archive.
		if !ok || p.kind < bestKind || (p.kind == bestKind && ref.dle < bestDLE) {
			bestKind, bestDLE, reason, ok = p.kind, ref.dle, p.text, true
		}
	}
	return reason, ok
}

// First returns the first of the given run ids that the floor pins, with the reason — the
// medium-wide floor projected onto one volume's runs. The caller computes the Floor over a
// whole medium (so "a newer full exists" is judged medium-wide), then passes the ids of the
// runs that have a part on the one volume being considered for reclamation — tape recycling
// or relabel. Because a spanned run has a placement on every tape it touches, it is reported
// for each of them: reclaiming any one tape would destroy the run, even the tapes that hold
// no seal record. Shared by the prune/recycle path and `nb label --relabel` so both judge a
// volume's reusability identically.
func (f Floor) First(runIDs []string) (runID, reason string, ok bool) {
	for _, id := range runIDs {
		if r, p := f.Reason(id); p {
			return id, r, true
		}
	}
	return "", "", false
}

func hasNewerFull(archives []record.Archive, dle, targetRun string) bool {
	for _, a := range archives {
		if a.DLE == dle && a.Level == 0 && record.RunIDLess(targetRun, a.Run) {
			return true // a full of dle in a strictly later run
		}
	}
	return false
}
