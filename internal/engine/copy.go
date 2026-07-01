package engine

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// copy.go is NBackup's copy operation: it re-authors a sealed run
// from one configured medium onto another, recording the new copy as a second placement. The
// bytes are carried raw — no transform — so checksums and members carry over; only the part
// layout changes to fit the target's volumes. It depends on a narrow slice of the orchestrator:
// the catalog (run metadata + where copies live), the clerk (the read/write data path), and the
// shared write machinery (prepareWriter) — not the whole engine.
type copier struct {
	cat           *catalog.Catalog                                     // run metadata
	clerk         *clerk.Clerk                                         // read endpoints + write session
	landing       string                                               // default source medium (the landing medium)
	knownMedium   func(name string) bool                               // target is a configured medium
	placementOn   func(runID, medium string) (catalog.Placement, bool) // a run's copy on a medium
	openCheck     func(medium string) error                            // the source medium opens (fail fast before reading)
	prepareWriter func(medium string, spec archiveio.RunSpec, now time.Time, logf Logf) (*writeTarget, error)
	reclaimCopy   func(runID, medium string) error // drop a prior copy on a removable target before a forced re-copy
}

// newCopier wires a copier to the engine's catalog, data path, and write machinery.
func (e *Engine) newCopier() *copier {
	return &copier{
		cat:         e.cat,
		clerk:       e.clerk,
		landing:     e.mediumName,
		knownMedium: func(name string) bool { _, ok := e.cfg.Media[name]; return ok },
		placementOn: e.placementOn,
		openCheck: func(medium string) error {
			_, _, _, err := e.librarianFor(medium)
			return err
		},
		prepareWriter: e.prepareWriter,
		reclaimCopy:   e.acct.ReclaimCopy,
	}
}

// CopyPlan is the resolved, validated outcome of a would-be copy, without writing:
// the source/target the rules picked and whether the run is already on the target.
type CopyPlan struct {
	RunID           string
	From            string   // resolved source medium (landing when --from is unset)
	To              string   // target medium
	Archives        int      // archives in the run
	Bytes           int64    // the run's total bytes
	AlreadyOnTarget bool     // a copy already exists on To (skipped unless force)
	TargetLabels    []string // the tape labels the existing target copy spans (empty for address-identified media)
}

// PlanCopy resolves and validates a copy the way CopyRun would, without writing —
// the single source of the copy-eligibility rules, shared by CopyRun and the
// `nb copy` dry-run so the two never drift. It errors on the same unrunnable cases
// (unknown run, unknown target, source == target) and reports whether the run is
// already on the target (force plans the re-copy anyway).
func (c *copier) PlanCopy(runID, fromMedia, targetMedia string, force bool) (CopyPlan, error) {
	s, err := c.cat.ReadRun(runID)
	if err != nil {
		return CopyPlan{}, err
	}
	if fromMedia == "" {
		fromMedia = c.landing
	}
	// Validate the source name up front, like `nb sync` does, so an unknown --from
	// fails with "unknown source medium" instead of slipping through to the
	// already-on-target short-circuit and reporting a misleading no-copy-on-source.
	if !c.knownMedium(fromMedia) {
		return CopyPlan{}, fmt.Errorf("unknown source medium %q", fromMedia)
	}
	if !c.knownMedium(targetMedia) {
		return CopyPlan{}, fmt.Errorf("unknown medium %q", targetMedia)
	}
	if fromMedia == targetMedia {
		return CopyPlan{}, fmt.Errorf("copy source and target are the same medium %q", targetMedia)
	}
	plan := CopyPlan{RunID: runID, From: fromMedia, To: targetMedia, Archives: len(s.Archives), Bytes: s.TotalBytes()}
	if !force {
		if p, ok := c.placementOn(runID, targetMedia); ok {
			plan.AlreadyOnTarget = true
			plan.TargetLabels = p.Labels()
		}
	}
	return plan, nil
}

// CopyRun streams a sealed run from one configured medium to another, then
// records the new copy in the catalog (a second placement). The source defaults to
// the landing medium when fromMedia is ""; any other medium holding the run is
// allowed (e.g. un-vaulting tape -> disk). Reading the source mounts the volume
// that holds the run (on a changer); the write to the target runs the same label
// verification as a dump.
func (c *copier) CopyRun(runID, fromMedia, targetMedia string, force bool, logf Logf) error {
	plan, err := c.PlanCopy(runID, fromMedia, targetMedia, force)
	if err != nil {
		return err
	}
	if plan.AlreadyOnTarget {
		// Idempotency: a run already recorded on the target is not re-copied. On
		// append-only media a second copy would orphan the first (unreferenced files,
		// reclaimable only by relabel); --force overrides for a deliberate re-copy.
		where := ""
		if len(plan.TargetLabels) > 0 {
			where = fmt.Sprintf(" (volume(s) %v)", plan.TargetLabels)
		}
		return fmt.Errorf("run %s is already on medium %q%s; use --force to copy again", runID, targetMedia, where)
	}
	fromMedia = plan.From
	s, err := c.cat.ReadRun(runID)
	if err != nil {
		return err
	}
	// Validate the source copy exists on fromMedia up front (a clear error before reading).
	if err := c.copySource(runID, fromMedia); err != nil {
		return err
	}
	// A forced re-copy onto a target that already holds the run must reclaim the
	// prior copy first; otherwise re-authoring lands new files and the catalog
	// placement is overwritten, orphaning the old files (lost capacity). On removable
	// media this deletes them; on tape it is a no-op (orphan-until-relabel, as documented).
	if force {
		if _, ok := c.placementOn(runID, targetMedia); ok {
			if err := c.reclaimCopy(runID, targetMedia); err != nil {
				return err
			}
		}
	}
	// Re-author the run onto the target: each archive's already-compressed payload
	// (the source copy's parts concatenated) is re-split into parts sized to the
	// target's volumes, rolling onto a fresh volume mid-archive when one fills. The
	// bytes are unchanged, so checksums and members carry over; only the part layout
	// is new.
	now := time.Now().UTC()
	// Re-author under the source's identity so each copied archive's footer names the same
	// logical run; each archive keeps its own CreatedAt (NewCopy preserves it), so the header
	// stamp here is just the run's last-activity time.
	spec := archiveio.RunSpec{ID: s.ID, CreatedAt: s.LastArchiveAt()}
	wt, err := c.prepareWriter(targetMedia, spec, now, logf)
	if err != nil {
		return err
	}
	logf.Log("copying %s from %q to %q", runID, fromMedia, targetMedia)
	// Open the source copy's archives as a one-pass read (the clerk resolves their positions),
	// then re-author each onto the target. Copy order is immaterial — archives are keyed by
	// (dle, level) — so the physical ordering is a free win.
	refs := make([]clerk.Ref, 0, len(s.Archives))
	metaByRef := map[clerk.Ref]record.Archive{}
	for _, a := range s.Archives {
		ref := clerk.Ref{Run: runID, DLE: a.DLE, Level: a.Level}
		refs = append(refs, ref)
		metaByRef[ref] = a
	}
	missing, err := c.clerk.ReadArchives(refs, fromMedia, func(ref clerk.Ref, open func() (io.ReadCloser, error)) error {
		rc, serr := open()
		if serr != nil {
			return fmt.Errorf("copy %s L%d to %q: %w", ref.DLE, ref.Level, targetMedia, serr)
		}
		// Re-author the archive raw (no transform) onto the target's volumes. Load the members
		// so the target writes a real member index (keeping that copy self-describing). NewCopy
		// verifies the bytes against the recorded checksum and preserves the source's identity;
		// its Commit records the new placement on the target.
		meta := metaByRef[ref]
		meta.Members, _ = c.clerk.Members(ref)
		cw := wt.writer.NewCopy(meta)
		if _, werr := xfer.Transfer(context.Background(), xfer.Reader(rc), xfer.NewFilters(), cw); werr != nil {
			return fmt.Errorf("copy %s L%d to %q: %w", ref.DLE, ref.Level, targetMedia, werr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("source copy of %s on %q is missing one or more archives", runID, fromMedia)
	}
	// No seal: each archive's copy recorded its placement on the target as it committed
	// (NewCopy's Commit), so the copy is complete once every archive has landed.
	logf.Log("copied %s (%d archive(s)) to %q", runID, len(s.Archives), targetMedia)
	return nil
}

// copySource validates the read side of a copy: the run has a copy on the source medium and
// that medium opens, so an unrunnable copy fails with a clear error before any bytes flow.
func (c *copier) copySource(runID, fromMedia string) error {
	if _, ok := c.placementOn(runID, fromMedia); !ok {
		return fmt.Errorf("run %s has no copy on source medium %q", runID, fromMedia)
	}
	if err := c.openCheck(fromMedia); err != nil {
		return err
	}
	return nil
}
