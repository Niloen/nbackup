package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/record"
)

// sync.go is the copier's bulk front-end: `nb sync` mirrors a source medium's
// sealed runs onto a target by resolving the backlog (runs on the source the
// target is missing) and carrying it through the same execute path as `nb copy`.

// SyncSelection bounds which runs a sync considers. The zero value selects every
// candidate run.
type SyncSelection struct {
	Last  int       // 0 = all; else only the N most recent runs
	Since time.Time // zero = no lower bound; else only runs created at/after this
	// RunIDs restricts the sync to exactly these runs — the surgical repair after a
	// tripped landing (`nb sync --run <id> --to <landing>`). An unknown id is an
	// error, not an empty backlog.
	RunIDs []string
}

// SyncItem is one run in a sync's backlog: a copy the target is missing — entirely, or
// in part (an interrupted earlier copy left some archives uncommitted there). Archives
// and Bytes count only what this sync would copy: the archives the source copy holds
// that the target copy does not.
type SyncItem struct {
	RunID    string
	Source   string // the medium this item's archives are read from (the --from, or the auto-resolved source)
	Archives int
	Bytes    int64 // compressed size of the missing archives on the volume
	Copied   bool  // set true once the target holds the run completely (checked after a real run)

	want []record.Archive // the resolved archives — apply carries exactly these (what the backlog priced)
}

// SyncReport is the backlog of one sync target (and, after a real run, what was
// copied). It is what the CLI renders for both dry-run and apply.
type SyncReport struct {
	From  string // the named source, or "" when each item's source was auto-resolved (see SyncItem.Source)
	To    string
	Items []SyncItem // runs not yet whole on To, oldest-first, after selection

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

// Sources returns the backlog's distinct source media in first-seen (oldest-run)
// order — the media an auto-resolved sync reads from, for the CLI's source label.
func (r *SyncReport) Sources() []string {
	var out []string
	seen := map[string]bool{}
	for _, it := range r.Items {
		if !seen[it.Source] {
			seen[it.Source] = true
			out = append(out, it.Source)
		}
	}
	return out
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

// SyncTo mirrors sealed runs onto target: every run holding archives the target copy
// does not, oldest-first. Oldest first means an interrupted sync makes contiguous,
// replayable progress and a run's full lands before the incrementals that build on it.
//
// The source: an explicit from reads that one medium (any medium is allowed, e.g.
// tape -> disk). With from == "" the source is resolved PER RUN — the landing when
// its copy holds everything the target is missing, else whichever other medium does
// (see sourceFor). That auto mode is what makes the trip repair work when the failed
// landing is the primary: `nb sync --run <id> --to <primary>` reads the surviving
// landing without the operator having to know which one that is.
//
// With apply==false it only computes the backlog (a dry run). With apply==true it
// carries the resolved backlog via execute — the same label-verified,
// placement-recording path as `nb copy`, transferring exactly what the report
// priced — stopping at the first error and returning the report so far
// alongside it (a full or offline target won't fix itself by continuing). Presence
// is archive-granular (see copier.copySets): each archive commits its target
// placement atomically as it lands, and a run counts as mirrored only once the
// target holds every archive its source copy does — so a sync that fails mid-run
// leaves a resumable partial copy, never one that reads as "up to date", and
// re-running copies exactly what is missing. With force==true already-present
// runs are re-copied wholesale (CopyRun --force).
func (c *copier) SyncTo(from, target string, sel SyncSelection, apply, force bool, logf Logf) (*SyncReport, error) {
	auto := from == ""
	if auto {
		if !c.knownMedium(target) {
			return nil, fmt.Errorf("unknown medium %q %s", target, mediaNamesHint(c.cfg))
		}
	} else if err := c.validatePair(from, target); err != nil {
		return nil, err
	}
	// An explicitly named run must exist — a typo'd --run is an error, never a
	// silent "up to date". With an explicit source it must have a copy there too.
	for _, id := range sel.RunIDs {
		if _, err := c.cat.ReadRun(id); err != nil {
			return nil, err
		}
		if !auto {
			if _, ok := placementOn(c.cat, id, from); !ok {
				return nil, fmt.Errorf("run %s has no copy on source medium %q", id, from)
			}
		}
	}
	candidates := c.cat.Runs()
	if !auto {
		candidates = c.cat.RunsOn(from)
	}

	report := &SyncReport{From: from, To: target}
	for _, s := range applySelection(candidates, sel) {
		src := from
		if auto {
			var err error
			if src, err = c.sourceFor(s, target, force); err != nil {
				return nil, err
			}
			if src == "" {
				continue // the target already holds everything this run has
			}
		}
		held, missing, err := c.copySets(s.ID, src, target)
		if err != nil {
			return nil, err
		}
		// A forced sync re-copies the source copy's whole content.
		want := wantArchives(held, missing, force)
		if len(want) == 0 {
			continue // idempotent: the target holds everything the source copy does
		}
		report.Items = append(report.Items, SyncItem{
			RunID:    s.ID,
			Source:   src,
			Archives: len(want),
			Bytes:    archivesBytes(want),
			want:     want,
		})
	}
	// Capacity projection (sampled before any copy, so it reads the same for dry-run
	// and apply): current target usage plus the backlog about to land — the same
	// accountant figure CopyPlan carries, so copy and sync warn off one arithmetic.
	if _, projected, capacity, perr := c.acct.ProjectedOverCapacity(target, report.Bytes()); perr == nil {
		report.ProjectedBytes, report.TargetCapacity = projected, capacity
	}
	if !apply {
		return report, nil
	}
	// Carry the resolved backlog as-is — execute groups it by source medium, one
	// spool pass each (one pass total when the source was explicit), and stops at
	// the first failure. A partial sync is safe to re-run (it is idempotent).
	sets := make([]runCopy, 0, len(report.Items))
	for _, it := range report.Items {
		sets = append(sets, runCopy{runID: it.RunID, source: it.Source, want: it.want})
	}
	copyErr := c.execute(sets, target, force, logf)
	// Mark what actually landed by re-reading the catalog, not by assuming success: a
	// sync that failed partway still reports the runs that completed before the error
	// (and the run record's BytesMoved counts them), instead of "copied 0 run(s)".
	for i := range report.Items {
		if _, missing, err := c.copySets(report.Items[i].RunID, report.Items[i].Source, target); err == nil && len(missing) == 0 {
			report.Items[i].Copied = true
		}
	}
	if copyErr != nil {
		return report, fmt.Errorf("sync -> %q: %w", target, copyErr)
	}
	return report, nil
}

// sourceFor resolves which medium serves one run's backlog when the sync named no
// source: the landing when its copy holds every archive the target is missing, else
// the first other medium (alphabetical, for determinism) whose copy does. "" when
// the target already holds everything; an error when archives are missing on the
// target but no single medium holds them all (the operator must name a source, or
// run twice with explicit --from). force treats the run's whole content as missing,
// matching wantArchives' forced re-copy.
func (c *copier) sourceFor(s *catalog.Run, target string, force bool) (string, error) {
	missing := s.Archives // force treats the run's whole content as missing
	if !force {
		tgt, _ := placementOn(c.cat, s.ID, target) // a zero Placement holds nothing
		missing = tgt.Missing(s.Archives)
	}
	if len(missing) == 0 {
		return "", nil
	}
	covers := func(medium string) bool {
		p, ok := placementOn(c.cat, s.ID, medium)
		return ok && len(p.Missing(missing)) == 0
	}
	if c.landing != target && covers(c.landing) {
		return c.landing, nil
	}
	var names []string
	for name := range c.cfg.Media {
		if name != target && name != c.landing {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if covers(name) {
			return name, nil
		}
	}
	return "", fmt.Errorf("run %s: %d archive(s) missing on %q, but no other medium holds them all — name a source with --from", s.ID, len(missing), target)
}

// applySelection narrows candidate runs (oldest-first) to the selection window.
func applySelection(runs []*catalog.Run, sel SyncSelection) []*catalog.Run {
	if len(sel.RunIDs) > 0 {
		want := make(map[string]bool, len(sel.RunIDs))
		for _, id := range sel.RunIDs {
			want[id] = true
		}
		kept := runs[:0:0]
		for _, s := range runs {
			if want[s.ID] {
				kept = append(kept, s)
			}
		}
		runs = kept
	}
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
