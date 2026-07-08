// chain.go computes the ordered chain of archives needed to reconstruct a DLE as
// of a target run. It is pure: it works over archive metadata and returns the
// steps; the restorer performs the I/O and extraction.
package recovery

import (
	"fmt"

	"github.com/Niloen/nbackup/internal/record"
)

// Step is one archive to extract during a restore. It identifies the archive
// logically; the engine resolves its volume position via the catalog.
type Step struct {
	RunID    string
	DLE      string
	Level    int
	Archiver string       // archiver type that produced the archive
	Compress string       // compression scheme to reverse before extracting
	Encrypt  string       // encryption scheme to reverse before decompressing ("" = plaintext)
	Shape    record.Shape // recorded stream shape — selects the decode mode (one child vs per-atom loop)
}

// Chain returns the archives needed to restore a DLE as of the target run, in
// run order: one archive per level along the real base chain — the most recent
// dump for the DLE at or before the target (the "tip"), then the base each
// incremental was built on (its recorded BaseRun), walked back to the full.
//
// This is the per-level restore: a level-N dump is cumulative since the
// most recent level-(N-1) dump, so only the newest dump of each level is
// replayed and earlier same-level repeats are skipped. Replaying them is not
// merely redundant — GNU tar's directory directives (rename, delete) are not
// idempotent across independent incremental extractions, so a second cumulative
// L1 carrying the same rename aborts the chain. Following BaseRun is also what
// keeps the chain consistent: each step's base is the exact dump it derives
// from, never an unrelated same-level dump. A missing base is a broken chain and
// is an error (a deliberate failure, not a partial restore). The input is the
// catalog's archives (each carrying its run tag); a run is just their grouping.
func Chain(archives []record.Archive, dleName, targetRunID string) ([]Step, error) {
	targetExists := false
	for _, a := range archives {
		if a.Run == targetRunID {
			targetExists = true
			break
		}
	}
	if !targetExists {
		return nil, fmt.Errorf("run %s not found in catalog", targetRunID)
	}

	// The DLE's archives in run order (one per run — a run dumps each DLE once).
	ds := record.ArchivesOf(archives, dleName)

	// The tip is the most recent dump of the DLE at or before the target.
	cur := -1
	for i := len(ds) - 1; i >= 0; i-- {
		if !record.RunIDLess(targetRunID, ds[i].Run) { // ds[i].Run <= target
			cur = i
			break
		}
	}
	if cur < 0 {
		return nil, fmt.Errorf("no backup found for DLE %q at or before %s", dleName, targetRunID)
	}

	// Walk back along the base chain, newest level first.
	var steps []Step
	for {
		a := ds[cur]
		steps = append(steps, Step{RunID: a.Run, DLE: a.DLE, Level: a.Level, Archiver: a.Archiver, Compress: a.Compress, Encrypt: a.Encrypt, Shape: a.Shape})
		if a.Level == 0 {
			break
		}
		baseIdx, err := baseIndex(ds, cur, dleName)
		if err != nil {
			return nil, err
		}
		cur = baseIdx
	}

	// Reverse into run order: the full first, then each level up to the tip.
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	return steps, nil
}

// BaseOf locates the archive that ds[curIdx]'s restore directly builds on, within ds
// (one DLE's archives in run order): the recorded BaseRun when present, else the most
// recent dump one level down (what the planner would have recorded). It reports false
// for a full (nothing to build on) and for a broken chain (the base is not in ds).
// It is the one owner of the direct-base rule: Chain's walk keys on it, and so does
// reclamation's chain-safe delete order (media's Reclaim), so "what depends on what"
// can never drift from what a restore actually replays.
func BaseOf(ds []record.Archive, curIdx int) (int, bool) {
	a := ds[curIdx]
	if a.Level == 0 {
		return 0, false
	}
	if a.BaseRun != "" {
		for i := curIdx - 1; i >= 0; i-- {
			if ds[i].Run == a.BaseRun {
				return i, true
			}
		}
		return 0, false
	}
	for i := curIdx - 1; i >= 0; i-- {
		if ds[i].Level == a.Level-1 {
			return i, true
		}
	}
	return 0, false
}

// baseIndex locates the archive that an incremental builds on, within ds (the DLE's archives
// in run order). It honors the recorded BaseRun strictly — if it names a run that no longer
// holds a backup for the DLE (pruned away), that is a broken chain and an error, never a
// silent substitution. When BaseRun was not recorded it derives the base as the most recent
// dump one level down before curIdx (what the planner would have recorded). A missing base
// either way is an error, not a partial restore.
func baseIndex(ds []record.Archive, curIdx int, dleName string) (int, error) {
	if i, ok := BaseOf(ds, curIdx); ok {
		return i, nil
	}
	a := ds[curIdx]
	if a.BaseRun != "" {
		return 0, fmt.Errorf("broken incremental chain for DLE %q: run %s (level %d) builds on run %q, which holds no backup for it", dleName, a.Run, a.Level, a.BaseRun)
	}
	return 0, fmt.Errorf("broken incremental chain for DLE %q: run %s (level %d) has no level-%d base at or before it", dleName, a.Run, a.Level, a.Level-1)
}

// StrandedArchive is an archive no restore can use: an incremental whose base chain
// is broken, so Chain cannot assemble the restore that would replay it. Err is
// Chain's error naming the missing link.
type StrandedArchive struct {
	Archive record.Archive
	Err     error
}

// Stranded reports the unrestorable archives: each incremental whose restore chain
// Chain cannot assemble from the given archives. A full is never stranded — it
// restores alone. Restorability is judged against exactly what the caller passes:
// give it the whole catalog's archives, so an archive counts as stranded only when
// its base is gone from every medium (a restore assembles its chain across media,
// so a base copy elsewhere keeps a lone incremental restorable).
//
// A stranded archive holds capacity while protecting nothing, so prune reclaims it
// ahead of everything else and `nb check` warns about it. Reclamation's chain-safe
// delete order (a base outlives its dependents) keeps new ones from being created,
// so one existing is history from before that rule — or a hand-edited catalog —
// and worth a warning, never silence.
func Stranded(archives []record.Archive) []StrandedArchive {
	var out []StrandedArchive
	for _, a := range archives {
		if a.Level == 0 {
			continue
		}
		if _, err := Chain(archives, a.DLE, a.Run); err != nil {
			out = append(out, StrandedArchive{Archive: a, Err: err})
		}
	}
	return out
}
