package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/accounting"
	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/depot"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/spool"
	"github.com/Niloen/nbackup/internal/xfer"
)

// copy.go is NBackup's copy operation: it re-authors a sealed run
// from one configured medium onto another, recording the new copy as a second placement. The
// bytes are carried raw — no transform — so checksums and members carry over; only the part
// layout changes to fit the target's volumes. It depends on a narrow slice of the orchestrator:
// the catalog (run metadata + where copies live), the fs (the read/write data path), the depot
// (the source medium's read face), and the accountant (force-reclaim + sync's capacity
// projection) — not the whole engine. Writes go through the conductor's spool (spoolCopy);
// newConductor is the one injected seam.
//
// The flow is resolve-then-execute: a front resolves what to carry exactly once — PlanCopy for
// `nb copy`, SyncTo's backlog for `nb sync` — into runCopy sets (run, source, the archives the
// target is missing), and execute carries those sets, so what was priced is what is copied.
type copier struct {
	cfg     *config.Config
	dep     *depot.Depot           // medium resolution: the source's read face (fail fast before reading)
	acct    *accounting.Accountant // force-reclaim of a prior target copy; capacity projection
	cat     *catalog.Catalog       // run metadata + where copies live
	fs      *archivefs.FS          // read endpoints + write session
	landing string                 // default source medium (the landing medium)
	workers int                    // copy concurrency (source reads / target drives)

	newConductor func() *conductor.Conductor // builds the per-run conductor (the spool wiring, shared with dump)
}

// newCopier wires a copier to the engine's catalog, data path, and the conductor's spool machinery
// (shared with dump), so a copy re-authors archives concurrently — one per target drive on a
// multi-drive library.
func (e *Engine) newCopier() *copier {
	return &copier{
		cfg:          e.cfg,
		dep:          e.dep,
		acct:         e.acct,
		cat:          e.cat,
		fs:           e.fs,
		landing:      e.dep.LandingName(),
		workers:      e.cfg.Workers(),
		newConductor: e.newConductor,
	}
}

// knownMedium reports whether the name is a configured medium.
func (c *copier) knownMedium(name string) bool { _, ok := c.cfg.Media[name]; return ok }

// validatePair validates a copy/sync medium pair: both names are configured and they
// differ. It is the one home of these checks, shared by PlanCopy and SyncTo, so the
// two fronts never drift.
func (c *copier) validatePair(from, target string) error {
	if !c.knownMedium(from) {
		return fmt.Errorf("unknown source medium %q %s", from, mediaNamesHint(c.cfg))
	}
	if !c.knownMedium(target) {
		return fmt.Errorf("unknown medium %q %s", target, mediaNamesHint(c.cfg))
	}
	if from == target {
		return fmt.Errorf("source and target are the same medium %q", target)
	}
	return nil
}

// wantArchives is the force-selection rule: normally the archives the target copy is
// still missing; on --force the source copy's whole content (a forced re-copy rewrites
// it all).
func wantArchives(held, missing []record.Archive, force bool) []record.Archive {
	if force {
		return held
	}
	return missing
}

// runCopy is one run's resolved copy work — the currency every front produces and
// execute consumes: the source its archives are read from and exactly the archives
// to transfer. Resolving it once (see PlanCopy, SyncTo) is what keeps the priced
// plan and the executed copy the same computation.
type runCopy struct {
	runID  string
	source string
	want   []record.Archive
}

// CopyPlan is the resolved, validated outcome of a would-be copy, without writing:
// the source/target the rules picked, what would be copied, whether the run is
// already on the target, and the target's capacity projection. It is the value
// Copy executes, so `nb copy` plans exactly once.
type CopyPlan struct {
	RunID           string
	From            string   // resolved source medium (landing when --from is unset)
	To              string   // target medium
	Archives        int      // archives that would be copied
	Bytes           int64    // their total stored bytes
	AlreadyOnTarget bool     // a copy already exists on To (skipped unless force)
	TargetLabels    []string // the tape labels the existing target copy spans (empty for address-identified media)

	// TargetCapacity is the target medium's retainable capacity (0 = unbounded), and
	// ProjectedBytes is what the target would hold once this copy lands — the same
	// pair SyncReport carries, so the CLI's over-capacity warning has one feed.
	TargetCapacity int64
	ProjectedBytes int64

	force bool
	want  []record.Archive // the resolved archives — what Copy transfers
}

// OverCapacity reports whether landing this copy would push the target past its
// capacity (false for an unbounded target).
func (p CopyPlan) OverCapacity() bool {
	return p.TargetCapacity > 0 && p.ProjectedBytes > p.TargetCapacity
}

// TargetWhere renders the existing target copy's whereabouts for messages —
// " (volume(s) [A B])" when it spans labeled volumes, "" for address-identified
// media — shared by Copy's refusal and the CLI's no-op notice.
func (p CopyPlan) TargetWhere() string {
	if len(p.TargetLabels) == 0 {
		return ""
	}
	return fmt.Sprintf(" (volume(s) %v)", p.TargetLabels)
}

// PlanCopy resolves and validates a copy without writing — the single source of the
// copy-eligibility rules, shared by Copy and the `nb copy` dry-run so the two never
// drift. It errors on the unrunnable cases (unknown run, unknown source/target,
// source == target, no copy on the source) and reports whether the run is already
// on the target (force plans the re-copy anyway). Presence is archive-granular: the
// run is "already on the target" only when the target copy holds every archive the
// source copy holds — a partial copy (an interrupted earlier run) plans the missing
// remainder, so Archives/Bytes are what WOULD be copied. The plan carries those
// resolved archives, so Copy transfers exactly what its plan priced.
func (c *copier) PlanCopy(runID, fromMedia, targetMedia string, force bool) (CopyPlan, error) {
	if _, err := c.cat.ReadRun(runID); err != nil {
		return CopyPlan{}, err
	}
	if fromMedia == "" {
		fromMedia = c.landing
	}
	// Validate the medium names up front, so an unknown --from fails with "unknown
	// source medium" instead of slipping through to the already-on-target
	// short-circuit and reporting a misleading no-copy-on-source.
	if err := c.validatePair(fromMedia, targetMedia); err != nil {
		return CopyPlan{}, err
	}
	held, missing, err := c.copySets(runID, fromMedia, targetMedia)
	if err != nil {
		return CopyPlan{}, err
	}
	if len(held) == 0 {
		// The source holds none of the run's archives — surface that now, before the
		// already-on-target check below, which only looks at the target and would
		// otherwise misreport a valid-but-wrong --from as a harmless no-op.
		return CopyPlan{}, fmt.Errorf("run %s has no copy on source medium %q", runID, fromMedia)
	}
	want := wantArchives(held, missing, force)
	plan := CopyPlan{
		RunID: runID, From: fromMedia, To: targetMedia,
		Archives: len(want), Bytes: archivesBytes(want),
		force: force, want: want,
	}
	if !force && len(missing) == 0 {
		if p, ok := placementOn(c.cat, runID, targetMedia); ok {
			plan.AlreadyOnTarget = true
			plan.TargetLabels = p.Labels()
		}
	}
	if _, projected, capacity, perr := c.acct.ProjectedOverCapacity(targetMedia, plan.Bytes); perr == nil {
		plan.ProjectedBytes, plan.TargetCapacity = projected, capacity
	}
	return plan, nil
}

// Copy executes a resolved plan: it streams the planned archives from the plan's
// source onto its target, then records the new copy in the catalog (a second
// placement). Reading the source mounts the volume that holds the run (on a
// changer); the write to the target runs the same label verification as a dump.
// A run already recorded whole on the target is refused: on append-only media a
// second copy would orphan the first (unreferenced files, reclaimable only by
// relabel); a plan made with force re-copies deliberately.
func (c *copier) Copy(plan CopyPlan, logf Logf) error {
	if plan.AlreadyOnTarget {
		return fmt.Errorf("run %s is already on medium %q%s; use --force to copy again", plan.RunID, plan.To, plan.TargetWhere())
	}
	if err := c.execute([]runCopy{{runID: plan.RunID, source: plan.From, want: plan.want}}, plan.To, plan.force, logf); err != nil {
		return err
	}
	// No seal: each archive's copy recorded its placement on the target as it committed
	// (NewCopy's Commit), so the copy is complete once every archive has landed.
	logf.Log("copied %s (%d archive(s)) to %q", plan.RunID, len(plan.want), plan.To)
	return nil
}

// execute carries resolved copy sets onto the target: one spool pass per distinct
// source medium (first-seen order), so a multi-drive library stays saturated across
// run boundaries rather than draining between runs. Per source it probes the read
// side once (fail fast before any bytes flow), reclaims a prior target copy for each
// forced run (so a re-copy does not orphan the old files — on removable media it
// deletes them; on tape it is a no-op, orphan-until-relabel), and streams every
// archive through one spool, one per target drive. A failure aborts the remaining
// sources — a hard target fault won't fix itself — but each committed archive
// already recorded its placement, so re-resolving copies exactly what is missing.
func (c *copier) execute(sets []runCopy, target string, force bool, logf Logf) error {
	var sources []string
	seen := map[string]bool{}
	for _, rc := range sets {
		if !seen[rc.source] {
			seen[rc.source] = true
			sources = append(sources, rc.source)
		}
	}
	for _, source := range sources {
		if err := c.probeSource(source); err != nil {
			return err
		}
		var jobs []copyJob
		var runs []string
		for _, rc := range sets {
			if rc.source != source {
				continue
			}
			if force {
				// A forced re-copy rewrites the whole source copy; the reclaim (where the
				// medium supports it) drops the prior target files so they are not orphaned.
				if _, ok := placementOn(c.cat, rc.runID, target); ok {
					if err := c.acct.ReclaimCopy(rc.runID, target); err != nil {
						return err
					}
				}
			}
			jobs = append(jobs, c.jobsForRun(rc.runID, rc.want)...)
			runs = append(runs, rc.runID)
		}
		if len(jobs) == 0 {
			continue
		}
		// Re-author under each archive's own identity (NewCopy preserves its run,
		// CreatedAt, checksum, and members; the member index is keyed on arch.Run and
		// the placement on the archive), so the spec id here just tags the spool pass.
		id := "sync"
		if len(runs) == 1 {
			id = runs[0]
			logf.Log("copying %s from %q to %q", runs[0], source, target)
		} else {
			logf.Log("copying %d run(s) from %q to %q", len(runs), source, target)
		}
		spec := archiveio.RunSpec{ID: id, CreatedAt: time.Now().UTC()}
		if err := c.spoolCopy(target, source, spec, jobs, logf); err != nil {
			return err
		}
	}
	return nil
}

// copySets resolves a copy archive-granularly: held is the archives the run's copy on
// `from` actually holds (a per-archive prune may have reclaimed some of the run's
// content there), and missing is the subset its copy on `target` does not hold yet —
// the resume set of an interrupted copy. A run counts as present on the target only
// when missing is empty; mere placement existence is not enough, because each archive
// records its placement as it commits, so a copy that fails mid-run leaves a partial
// placement behind. Sync's backlog and copy's already-on-target check both judge
// presence through this one function, and the retry copies exactly `missing`.
func (c *copier) copySets(runID, from, target string) (held, missing []record.Archive, err error) {
	s, err := c.cat.ReadRun(runID)
	if err != nil {
		return nil, nil, err
	}
	src, srcOK := placementOn(c.cat, runID, from)
	if !srcOK {
		return nil, nil, nil
	}
	tgt, _ := placementOn(c.cat, runID, target) // a zero Placement holds nothing
	for _, a := range s.Archives {
		if src.Holds(a.DLE, a.Level) {
			held = append(held, a)
		}
	}
	return held, tgt.Missing(held), nil
}

// archivesBytes sums the archives' stored (compressed) sizes.
func archivesBytes(archives []record.Archive) int64 {
	var n int64
	for _, a := range archives {
		n += a.Compressed
	}
	return n
}

// spoolCopy drives a set of copy jobs onto the target through the spool (shared with dump): one
// archive per target drive, up to `workers`, so a multi-drive library re-authors several at once.
// Source reads run concurrently only when the source medium allows it (disk/cloud); a tape source
// stays serial.
func (c *copier) spoolCopy(targetMedia, fromMedia string, spec archiveio.RunSpec, jobs []copyJob, logf Logf) error {
	// Make room on a bounded target BEFORE the copy lands — capacity as a promise,
	// and here with EXACT incoming bytes (the archives being copied are known), so
	// no estimate risk. The one choke point both `nb copy` and `nb sync` cross;
	// fails loud pre-write when the target's protected set cannot absorb the copy.
	var incoming int64
	for _, j := range jobs {
		incoming += j.est
	}
	freed, err := c.acct.MakeRoom(targetMedia, incoming, spec.CreatedAt, logf)
	if err != nil {
		return fmt.Errorf("make room on %q for %s: %w", targetMedia, sizeutil.FormatBytes(incoming), err)
	}
	if freed > 0 {
		logf.Log("made room on %q: reclaimed %s to fit the %s copy", targetMedia, sizeutil.FormatBytes(freed), sizeutil.FormatBytes(incoming))
	}
	return c.newConductor().CopyRun(context.Background(), targetMedia, fromMedia, spec, c.workers, spec.CreatedAt, logf, func(sp *spool.Spool, ro archivefs.ReadStore) error {
		return c.transfer(context.Background(), jobs, fromMedia, targetMedia, sp.Ingest(targetMedia), ro, logf)
	})
}

// copyJob is one archive to re-author onto the target: its read ref, its metadata (identity, checksum,
// members — preserved by NewCopy), and its compressed size (the spool's back-pressure estimate).
type copyJob struct {
	ref  archiveio.Ref
	meta record.Archive
	est  int64
}

// jobsForRun builds the copy jobs for a run's archives, loading each archive's member list so the
// target writes a self-describing member index.
func (c *copier) jobsForRun(runID string, archives []record.Archive) []copyJob {
	jobs := make([]copyJob, 0, len(archives))
	for _, a := range archives {
		ref := archiveio.Ref{Run: runID, DLE: a.DLE, Level: a.Level}
		idx, _ := c.fs.Index(ref)
		a.Members, a.Frames = idx.Members, idx.Frames
		if !a.Shape.Resplittable() {
			// A non-resplittable copy needs the source's per-part seals: they drive
			// the 1:1 atom cut and carry the RawSize map. Seals are archive-invariant
			// for atoms, so any placement with an aligned set serves.
			a.PartSeals = atomSeals(c.cat.Placements(runID), a.DLE, a.Level, a.Parts)
		}
		jobs = append(jobs, copyJob{ref: ref, meta: a, est: a.Compressed})
	}
	return jobs
}

// atomSeals returns an archive's per-part seals from the first placement carrying an
// aligned set, or nil (the copy then refuses rather than re-splitting an atom).
func atomSeals(placements []catalog.Placement, dle string, level, parts int) []record.PartSeal {
	for _, p := range placements {
		if pa, ok := p.Placed(dle, level); ok && len(pa.Seals) == parts && parts > 0 {
			return pa.Seals
		}
	}
	return nil
}

// transfer re-authors each job onto the target through the spool's Ingest, up to `workers` at once —
// clamped to serial when the source cannot be read concurrently (a tape's one drive). Each transfer
// opens the archive raw, leases a target drive via NewCopy, and streams it in; the spool's drive
// semaphore bounds the target side, so the effective width is min(source reads, target drives).
//
// Source opens go through ro — the window's catalog snapshot — never the live fs: the workers run
// concurrently with the spool's orchestrator, which owns the live catalog for the window's duration.
func (c *copier) transfer(ctx context.Context, jobs []copyJob, fromMedia, targetMedia string, ingest archivefs.Ingest, ro archivefs.ReadStore, logf Logf) error {
	workers := c.workers
	if !media.ConcurrentWrite(c.cfg.Media[fromMedia].Type) {
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
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fst != nil
	}
	for _, job := range jobs {
		sem <- struct{}{}
		// Stop scheduling after the first error — checked after the semaphore, so a
		// serial lane has seen its previous transfer finish: a hard sink fault (target
		// full or offline) will not fix itself for the next archive, and pressing on
		// would only pile the same failure onto every remaining job (and, on tape, keep
		// prodding a drive in a failed state). In-flight transfers finish; each
		// committed archive already recorded its placement, so the retry resumes from
		// what landed.
		if failed() {
			<-sem
			break
		}
		wg.Add(1)
		go func(job copyJob) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.transferOne(ctx, job, fromMedia, targetMedia, ingest, ro); err != nil {
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
func (c *copier) transferOne(ctx context.Context, job copyJob, fromMedia, targetMedia string, ingest archivefs.Ingest, ro archivefs.ReadStore) error {
	// Sync-time half of the atom validation ladder: a sealed atom cannot be re-cut
	// without the key, so an archive whose atoms cannot be carried whole is refused
	// per-archive (everything that fits is still carried by its own job).
	if !job.meta.Shape.Resplittable() {
		if len(job.meta.PartSeals) != job.meta.Parts || job.meta.Parts == 0 {
			return fmt.Errorf("copy %s L%d: atomic archive records no aligned per-part seals on any copy, so its atoms cannot be carried 1:1 — run `nb rebuild`, or re-dump", job.ref.DLE, job.ref.Level)
		}
		if ceiling := media.PartSizeFor(c.cfg.Media[targetMedia].Type).Max; ceiling > 0 {
			for i, s := range job.meta.PartSeals {
				if s.Size > ceiling {
					return fmt.Errorf("copy %s L%d to %q refused: atom %d is %s, over the medium's %s part ceiling, and a sealed atom cannot shrink without the key — lower the dumptype's part_size for future dumps (retention ages this archive out) or target a medium with a higher ceiling",
						job.ref.DLE, job.ref.Level, targetMedia, i, sizeutil.FormatBytes(s.Size), sizeutil.FormatBytes(ceiling))
				}
			}
		}
	}
	rc, err := ro.OpenArchive(job.ref, fromMedia)
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

// probeSource validates the read side of a copy — the source medium opens — so an
// unrunnable copy fails with a clear error before any bytes flow. (That the run HAS
// a copy there is the resolver's job: PlanCopy errors, sync's backlog only includes
// runs whose source copy covers what the target is missing.)
func (c *copier) probeSource(fromMedia string) error {
	rm, err := c.dep.OpenForRead(fromMedia)
	if err != nil {
		return err
	}
	return rm.Close()
}
