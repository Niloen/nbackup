package engine

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/record"
)

// sync.go is the copier's bulk front-end: `nb sync` mirrors a source medium's
// sealed runs onto a target by computing the backlog (runs on the source the
// target is missing) and copying it through the same CopyRuns path as `nb copy`.

// SyncSelection bounds which landing runs a sync considers. The zero value
// selects every run on the landing medium.
type SyncSelection struct {
	Last  int       // 0 = all; else only the N most recent landing runs
	Since time.Time // zero = no lower bound; else only runs created at/after this
}

// SyncItem is one run in a sync's backlog: a copy the target is missing — entirely, or
// in part (an interrupted earlier copy left some archives uncommitted there). Archives
// and Bytes count only what this sync would copy: the archives the source copy holds
// that the target copy does not.
type SyncItem struct {
	RunID    string
	Archives int
	Bytes    int64 // compressed size of the missing archives on the volume
	Copied   bool  // set true once the target holds the run completely (checked after a real run)
}

// SyncReport is the backlog of one sync target (and, after a real run, what was
// copied). It is what the CLI renders for both dry-run and apply.
type SyncReport struct {
	From  string
	To    string
	Items []SyncItem // runs on From not yet on To, oldest-first, after selection

	// TargetCapacity is the target medium's retainable capacity (0 = unbounded), and
	// ProjectedBytes is what the target would hold once this backlog lands (its
	// current usage plus the backlog). Sync does not prune — retention is a separate
	// per-medium concern — so it copies even when the projection exceeds capacity, but
	// surfaces the overshoot so the operator is not left to discover it via `nb plan`.
	TargetCapacity int64
	ProjectedBytes int64
}

// OverCapacity reports whether landing this backlog would push the target past its
// capacity (false for an unbounded target).
func (r *SyncReport) OverCapacity() bool {
	return r.TargetCapacity > 0 && r.ProjectedBytes > r.TargetCapacity
}

// Bytes is the total size of the backlog.
func (r *SyncReport) Bytes() int64 {
	var n int64
	for _, it := range r.Items {
		n += it.Bytes
	}
	return n
}

// Copied counts the items actually copied (after a real run).
func (r *SyncReport) Copied() int {
	n := 0
	for _, it := range r.Items {
		if it.Copied {
			n++
		}
	}
	return n
}

// CopiedBytes is the total bytes of the runs actually copied this run (Bytes() is the
// whole backlog, copied or not), for the run record's BytesMoved.
func (r *SyncReport) CopiedBytes() int64 {
	var n int64
	for _, it := range r.Items {
		if it.Copied {
			n += it.Bytes
		}
	}
	return n
}

// SyncTo mirrors a source medium's sealed runs onto target; see copier.SyncTo.
func (e *Engine) SyncTo(from, target string, sel SyncSelection, apply, force bool, logf Logf) (*SyncReport, error) {
	return e.cop.SyncTo(from, target, sel, apply, force, logf)
}

// SyncRules returns the configured replication rules, for the CLI to run when
// `nb sync` is invoked without an explicit --to.
func (e *Engine) SyncRules() []config.SyncRule { return e.cfg.Sync }

// SyncTo mirrors a source medium's sealed runs onto target: every run whose copy on
// the source holds archives the target copy does not, oldest-first. Oldest first
// means an interrupted sync makes contiguous, replayable progress and a run's
// full lands before the incrementals that build on it. The source defaults to the
// landing medium when from is ""; any other medium is allowed (e.g. tape -> disk).
//
// With apply==false it only computes the backlog (a dry run). With apply==true it
// copies the backlog via CopyRuns — the same label-verified, placement-recording
// path as `nb copy` — stopping at the first error and returning the report so far
// alongside it (a full or offline target won't fix itself by continuing). Presence
// is archive-granular (see copier.copySets): each archive commits its target
// placement atomically as it lands, and a run counts as mirrored only once the
// target holds every archive its source copy does — so a sync that fails mid-run
// leaves a resumable partial copy, never one that reads as "up to date", and
// re-running copies exactly what is missing. With force==true already-present
// runs are re-copied wholesale (CopyRun --force).
func (c *copier) SyncTo(from, target string, sel SyncSelection, apply, force bool, logf Logf) (*SyncReport, error) {
	if from == "" {
		from = c.landing
	}
	if from == target {
		return nil, fmt.Errorf("sync source and target are the same medium %q", target)
	}
	if !c.knownMedium(from) {
		return nil, fmt.Errorf("unknown source medium %q", from)
	}
	if !c.knownMedium(target) {
		return nil, fmt.Errorf("unknown medium %q", target)
	}

	report := &SyncReport{From: from, To: target}
	for _, s := range applySelection(c.cat.RunsOn(from), sel) {
		held, missing, err := c.copySets(s.ID, from, target)
		if err != nil {
			return nil, err
		}
		want := missing
		if force {
			want = held // a forced sync re-copies the source copy's whole content
		}
		if len(want) == 0 {
			continue // idempotent: the target holds everything the source copy does
		}
		report.Items = append(report.Items, SyncItem{
			RunID:    s.ID,
			Archives: len(want),
			Bytes:    archivesBytes(want),
		})
	}
	// Capacity projection (sampled before any copy, so it reads the same for dry-run
	// and apply): current target usage plus the backlog about to land.
	if prof, perr := c.profileFor(target); perr == nil {
		report.TargetCapacity = prof.TotalBytes()
	}
	report.ProjectedBytes = c.cat.MediumBytes(target) + report.Bytes()
	if !apply {
		return report, nil
	}
	runIDs := make([]string, len(report.Items))
	for i := range report.Items {
		runIDs[i] = report.Items[i].RunID
	}
	// Copy every selected run in one spool pass, so a multi-drive target stays saturated across run
	// boundaries. A failure aborts the sync (a partial sync is safe to re-run — it is idempotent).
	copyErr := c.CopyRuns(runIDs, from, target, force, logf)
	// Mark what actually landed by re-reading the catalog, not by assuming success: a
	// sync that failed partway still reports the runs that completed before the error
	// (and the run record's BytesMoved counts them), instead of "copied 0 run(s)".
	for i := range report.Items {
		if _, missing, err := c.copySets(report.Items[i].RunID, from, target); err == nil && len(missing) == 0 {
			report.Items[i].Copied = true
		}
	}
	if copyErr != nil {
		return report, fmt.Errorf("sync -> %q: %w", target, copyErr)
	}
	return report, nil
}

// applySelection narrows landing runs (oldest-first) to the selection window.
func applySelection(runs []*catalog.Run, sel SyncSelection) []*catalog.Run {
	if !sel.Since.IsZero() {
		kept := runs[:0:0]
		for _, s := range runs {
			// Filter on the run's logical date (the day it backs up), not its
			// physical CreatedAt seal time — otherwise back-dated or imported runs,
			// whose CreatedAt is "now", all slip past any --since bound.
			d, _ := record.ParseDateField(s.Date())
			if !d.Before(sel.Since) {
				kept = append(kept, s)
			}
		}
		runs = kept
	}
	if sel.Last > 0 && len(runs) > sel.Last {
		runs = runs[len(runs)-sel.Last:] // most recent N (runs are oldest-first)
	}
	return runs
}
