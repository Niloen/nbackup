package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/spool"
	"github.com/Niloen/nbackup/internal/xfer"
)

// copy.go is NBackup's copy operation: it re-authors a sealed run
// from one configured medium onto another, recording the new copy as a second placement. The
// bytes are carried raw — no transform — so checksums and members carry over; only the part
// layout changes to fit the target's volumes. It depends on a narrow slice of the orchestrator:
// the catalog (run metadata + where copies live), the clerk (the read/write data path), and the
// shared write machinery (prepareWriter) — not the whole engine.
type copier struct {
	cat         *catalog.Catalog                                     // run metadata
	clerk       *clerk.Clerk                                         // read endpoints + write session
	landing     string                                               // default source medium (the landing medium)
	knownMedium func(name string) bool                               // target is a configured medium
	placementOn func(runID, medium string) (catalog.Placement, bool) // a run's copy on a medium
	openCheck   func(medium string) error                            // the source medium opens (fail fast before reading)
	reclaimCopy func(runID, medium string) error                     // drop a prior copy on a removable target before a forced re-copy

	newConductor   func() *conductor.Conductor // builds the per-run conductor (for CopyRun's spool wiring)
	workers        int                         // copy concurrency (source reads / target drives)
	concurrentRead func(medium string) bool    // whether a medium's archives can be read concurrently (disk/cloud yes, tape no)
}

// newCopier wires a copier to the engine's catalog, data path, and the conductor's spool machinery
// (shared with dump), so a copy re-authors archives concurrently — one per target drive on a
// multi-drive library.
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
		reclaimCopy:    e.acct.ReclaimCopy,
		newConductor:   e.newConductor,
		workers:        e.cfg.Workers(),
		concurrentRead: func(medium string) bool { return media.ConcurrentWrite(e.cfg.Media[medium].Type) },
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
	jobs, err := c.prepareRun(runID, fromMedia, targetMedia, force)
	if err != nil {
		return err
	}
	logf.Log("copying %s from %q to %q", runID, fromMedia, targetMedia)
	// Re-author under the source's identity so each copied archive's footer names the same logical run
	// (NewCopy preserves each archive's run, CreatedAt, checksum, and members); the spec id here just
	// tags the run.
	spec := archiveio.RunSpec{ID: runID, CreatedAt: time.Now().UTC()}
	if err := c.runCopy(targetMedia, fromMedia, spec, jobs, logf); err != nil {
		return err
	}
	// No seal: each archive's copy recorded its placement on the target as it committed
	// (NewCopy's Commit), so the copy is complete once every archive has landed.
	logf.Log("copied %s (%d archive(s)) to %q", runID, len(jobs), targetMedia)
	return nil
}

// CopyRuns copies several sealed runs onto the target in one spool run, so a multi-drive library
// stays saturated across run boundaries rather than draining between runs. Each run is validated and
// its archives gathered up front; then every archive is re-authored through one spool, one per drive.
// It is the bulk path `nb sync` uses (which has already filtered out runs already on the target). A
// per-run failure aborts the whole run.
func (c *copier) CopyRuns(runIDs []string, fromMedia, targetMedia string, force bool, logf Logf) error {
	if fromMedia == "" {
		fromMedia = c.landing
	}
	var jobs []copyJob
	for _, id := range runIDs {
		js, err := c.prepareRun(id, fromMedia, targetMedia, force)
		if err != nil {
			return err
		}
		jobs = append(jobs, js...)
	}
	if len(jobs) == 0 {
		return nil
	}
	logf.Log("copying %d run(s) from %q to %q", len(runIDs), fromMedia, targetMedia)
	// One spool spans every run. Each archive records under its own run (the member index is keyed on
	// arch.Run and the placement on the archive), so the run's spec id is synthetic.
	spec := archiveio.RunSpec{ID: "sync", CreatedAt: time.Now().UTC()}
	return c.runCopy(targetMedia, fromMedia, spec, jobs, logf)
}

// prepareRun validates one run's copy (its source copy exists; on --force a prior target copy is
// reclaimed first) and returns the per-archive jobs to transfer. Reclaiming a prior copy before
// re-authoring keeps a forced re-copy from orphaning the old files (on removable media it deletes them;
// on tape it is a no-op — orphan-until-relabel).
func (c *copier) prepareRun(runID, fromMedia, targetMedia string, force bool) ([]copyJob, error) {
	s, err := c.cat.ReadRun(runID)
	if err != nil {
		return nil, err
	}
	if err := c.copySource(runID, fromMedia); err != nil {
		return nil, err
	}
	if force {
		if _, ok := c.placementOn(runID, targetMedia); ok {
			if err := c.reclaimCopy(runID, targetMedia); err != nil {
				return nil, err
			}
		}
	}
	return c.jobsForRun(runID, s.Archives), nil
}

// runCopy drives a set of copy jobs onto the target through the spool (shared with dump): one archive
// per target drive, up to `workers`, so a multi-drive library re-authors several at once. Source reads
// run concurrently only when the source medium allows it (disk/cloud); a tape source stays serial.
func (c *copier) runCopy(targetMedia, fromMedia string, spec archiveio.RunSpec, jobs []copyJob, logf Logf) error {
	return c.newConductor().CopyRun(context.Background(), targetMedia, spec, c.workers, spec.CreatedAt, logf, func(sp *spool.Spool) error {
		return c.transfer(context.Background(), jobs, fromMedia, targetMedia, sp.Ingest(targetMedia), logf)
	})
}

// copyJob is one archive to re-author onto the target: its read ref, its metadata (identity, checksum,
// members — preserved by NewCopy), and its compressed size (the spool's back-pressure estimate).
type copyJob struct {
	ref  clerk.Ref
	meta record.Archive
	est  int64
}

// jobsForRun builds the copy jobs for a run's archives, loading each archive's member list so the
// target writes a self-describing member index.
func (c *copier) jobsForRun(runID string, archives []record.Archive) []copyJob {
	jobs := make([]copyJob, 0, len(archives))
	for _, a := range archives {
		ref := clerk.Ref{Run: runID, DLE: a.DLE, Level: a.Level}
		a.Members, _ = c.clerk.Members(ref)
		jobs = append(jobs, copyJob{ref: ref, meta: a, est: a.Compressed})
	}
	return jobs
}

// transfer re-authors each job onto the target through the spool's Ingest, up to `workers` at once —
// clamped to serial when the source cannot be read concurrently (a tape's one drive). Each transfer
// opens the archive raw, leases a target drive via NewCopy, and streams it in; the spool's drive
// semaphore bounds the target side, so the effective width is min(source reads, target drives).
func (c *copier) transfer(ctx context.Context, jobs []copyJob, fromMedia, targetMedia string, ingest archiveio.Ingest, logf Logf) error {
	workers := c.workers
	if !c.concurrentRead(fromMedia) {
		workers = 1 // a serial source (tape) is read one archive at a time
	}
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		fst error
	)
	for _, job := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func(job copyJob) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.transferOne(ctx, job, fromMedia, targetMedia, ingest); err != nil {
				mu.Lock()
				fst = errors.Join(fst, err)
				mu.Unlock()
			}
		}(job)
	}
	wg.Wait()
	return fst
}

// transferOne opens one source archive raw and re-authors it onto the target via NewCopy (which leases
// a drive, preserves the source's identity/checksum/members, and records the new placement on Commit).
// Transfer drives and closes the writer, so the drive is released whether or not the copy commits.
func (c *copier) transferOne(ctx context.Context, job copyJob, fromMedia, targetMedia string, ingest archiveio.Ingest) error {
	rc, err := c.clerk.Open(job.ref, fromMedia)
	if err != nil {
		return fmt.Errorf("copy %s L%d from %q: %w", job.ref.DLE, job.ref.Level, fromMedia, err)
	}
	cw, err := ingest.NewCopy(job.meta, job.est)
	if err != nil {
		rc.Close()
		return fmt.Errorf("copy %s L%d to %q: %w", job.ref.DLE, job.ref.Level, targetMedia, err)
	}
	// Transfer commits the writer (footer + routed Record) on a clean stream but does not close it;
	// Close is the symmetric counterpart to NewCopy's acquire — it releases the leased drive whether or
	// not the copy committed. (Transfer closes the source reader.)
	defer cw.Close()
	if _, err := xfer.Transfer(ctx, xfer.Reader(rc), xfer.NewFilters(), cw); err != nil {
		return fmt.Errorf("copy %s L%d to %q: %w", job.ref.DLE, job.ref.Level, targetMedia, err)
	}
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
