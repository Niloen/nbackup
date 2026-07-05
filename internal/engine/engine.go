// Package engine is NBackup's orchestrator. It
// wires the planner, archiver, transfer pipeline, media store, catalog, and
// retention together to execute runs, restores, verification, and pruning. It is
// the only place that knows about all the abstractions at once; everything below
// it depends only on interfaces.
//
// The Engine itself is a composition root plus a thin command facade: the
// behavior lives in per-operation lanes (dumper, conductor, restorer, verifier,
// copier, driller, checker, accountant, scheduler), and the config→runtime
// resolution lives in two services the lanes share — the toolchain (hosts,
// executors, archivers, transform options) and the depot (media, volumes,
// librarians, write knobs).
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/accounting"
	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/depot"
	"github.com/Niloen/nbackup/internal/dumper"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/restorer"
	"github.com/Niloen/nbackup/internal/scheduler"

	// Register the bundled media and archiver implementations.
	_ "github.com/Niloen/nbackup/internal/archiver/gnutar"
	_ "github.com/Niloen/nbackup/internal/archiver/pipe"
	_ "github.com/Niloen/nbackup/internal/archiver/postgres"
	_ "github.com/Niloen/nbackup/internal/media/cloud"
	_ "github.com/Niloen/nbackup/internal/media/disk"
	_ "github.com/Niloen/nbackup/internal/media/gdrive"
	_ "github.com/Niloen/nbackup/internal/media/tape"
)

// Logf is an optional progress logger. It is an alias for logf.Logf, which lives in
// a leaf package so the lanes split out of the engine (accounting, scheduler,
// conductor) can all take one without an import cycle through the engine.
type Logf = logf.Logf

// Engine holds the wired-up components for one configuration: the two resolution
// services (toolchain, depot), the catalog cache, the archive data path (the fs),
// and one lane per operation. Its methods are the command surface — thin
// delegations; the behavior is in the lanes.
type Engine struct {
	cfg         *config.Config
	tc          *toolchain   // host/tool resolution: executors, archivers, transform options
	dep         *depot.Depot // medium resolution: volumes, librarians, write knobs
	cat         *catalog.Catalog
	dles        *dleDirectory // DLE slug ↔ host:path identity mapping
	fs          *archivefs.FS // the archive data path (read+write composer)
	landingCost media.Cost    // landing medium's pricing (dollar peer of the depot's profile)

	runSink      progress.Sink // optional: live run-progress sink (nil = status file only)
	estimateSink progress.Sink // optional: live estimate-progress sink (nil = status file only)

	dmp   *dumper.Dumper         // the producer (dump): workers + tar source + encode pipeline
	ver   *verifier              // the verification operation (verify/drill checks)
	cop   *copier                // the copy operation (PlanCopy/CopyRun/sync)
	drl   *driller               // the recovery-drill operation (tiers + WORM probe + posture)
	rst   *restorer.Restorer     // the read-side operation package (restore/recover)
	acct  *accounting.Accountant // capacity/retention arithmetic
	sched *scheduler.Scheduler   // plan/estimate/validate lane
}

// SetOperator attaches an operator so manual single-drive media can prompt for a
// reel swap mid-command. Without one, manual swaps degrade to an actionable error.
func (e *Engine) SetOperator(op librarian.Operator) { e.dep.SetOperator(op) }

// SetRunProgress attaches a live progress sink that receives run snapshots alongside
// the run-status file, so `nb dump` can paint progress to the terminal without an
// operator running `nb status`. Nil (the default) keeps the file-only behaviour.
func (e *Engine) SetRunProgress(sink progress.Sink) { e.runSink = sink }

// SetEstimateProgress attaches a live progress sink for a run's estimate phase —
// the size-everything prelude that precedes any dumping. Painted alongside the
// run-status file, it lets `nb dump` show "Estimating sizes…" before "Dumping…"
// instead of sitting silent while a slow sizing pass runs. Nil (the default) keeps
// the estimate phase file-only.
func (e *Engine) SetEstimateProgress(sink progress.Sink) { e.estimateSink = sink }

// New constructs an Engine from configuration: it resolves the landing medium's
// capacity profile via the media registry and loads the catalog cache. The landing
// volume itself is opened lazily, the first time a command actually touches the
// medium (see depot.landing) — so a catalog-only command (report, run, dle) never
// lists a bucket or mounts a tape. Archivers are opened lazily per dumptype.
func New(cfg *config.Config) (*Engine, error) {
	e, err := build(cfg)
	if err != nil {
		return nil, err
	}
	// A read-only preview (`nb plan`) never opens the landing volume, so an unknown
	// landing type would otherwise slip through as an "unbounded" plan (exit 0) while
	// `nb dump` fails the moment it opens the volume. Reject it here — the same hard
	// error, surfaced early — so plan and dump agree. (`nb check` uses NewForCheck,
	// which reports an unknown type as a collected finding instead of aborting.)
	if name, lerr := cfg.LandingName(); lerr == nil && !media.KnownVolumeType(cfg.Media[name].Type) {
		return nil, fmt.Errorf("landing medium %q has unknown type %q (known: %v)", name, cfg.Media[name].Type, media.VolumeTypes())
	}
	return e, nil
}

// NewForCheck builds an engine for `nb check`. It is identical to New now that the
// landing volume opens lazily: `nb check` probes the landing explicitly (see
// checkMedia) and reports an open failure as a collected check failure rather than
// aborting — so the rest of the checks still run. Kept as a distinct constructor so
// the intent at the call site stays legible.
func NewForCheck(cfg *config.Config) (*Engine, error) { return build(cfg) }

// build wires an Engine from configuration. The landing volume is not opened here;
// depot.landing opens it on first use.
func build(cfg *config.Config) (*Engine, error) {
	// Validate every medium's inline options against the keys its type accepts, so
	// a typo (e.g. `capcity:`) is a hard error rather than a silently-ignored knob.
	// Done here, where the media registry is loaded, and over all media (not just
	// landing) so an offsite tier's typo is caught too.
	limiters := map[string]*ratelimit.Limiter{}
	for mname, def := range cfg.Media {
		if err := media.ValidateParams(def.Type, def.Params); err != nil {
			return nil, fmt.Errorf("media %s: %w", mname, err)
		}
		// Surface a bad cost override (unknown provider, malformed rate) at load time,
		// like a param typo, rather than at first cost calculation.
		if _, err := media.OpenCost(def.Type, media.Options(def.CostOptions())); err != nil {
			return nil, fmt.Errorf("media %s: %w", mname, err)
		}
		// One shared limiter per medium, built once: the same instance throttles a
		// medium's concurrent worker writes (shared budget) and its read streams. A
		// nil entry (the common, uncapped case) leaves the streams untouched.
		bps, err := def.ThroughputBytes()
		if err != nil {
			return nil, fmt.Errorf("media %s: throughput: %w", mname, err)
		}
		limiters[mname] = ratelimit.NewLimiter(bps)
	}
	// A holding disk buffers the landing's writes: parallel dumpers share its write sink and the
	// drain reclaims each archive as it lands, so its medium type must accept concurrent writes
	// and per-file reclaim (disk, cloud). A serial, whole-volume medium (tape) cannot. Checked
	// here, where the media registry is wired (config validates only the structural rules).
	for _, holding := range cfg.HoldingMedia() {
		if t := cfg.Media[holding].Type; !media.ConcurrentWrite(t) {
			return nil, fmt.Errorf("media %s: holding: true requires a disk or cloud medium (got %q) — the holding disk reclaims per archive and the parallel dumpers need an unbounded write sink", holding, t)
		}
	}
	name, err := cfg.LandingName()
	if err != nil {
		return nil, err
	}
	mediaDef := cfg.Media[name]
	profile, err := media.OpenProfile(mediaDef.Type, media.Options(mediaDef.ProfileOptions()))
	if err != nil {
		return nil, err
	}
	costModel, err := media.OpenCost(mediaDef.Type, media.Options(mediaDef.CostOptions()))
	if err != nil {
		return nil, err
	}
	cat, err := catalog.Open(cfg.WorkdirPath())
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg:         cfg,
		tc:          newToolchain(cfg),
		dep:         depot.New(cfg, cat, name, mediaDef, profile, cfg.MinAgeFor(mediaDef), limiters),
		cat:         cat,
		dles:        &dleDirectory{cfg: cfg, cat: cat},
		landingCost: costModel,
	}
	e.fs = archivefs.New(fsDeps{e}, fsDeps{e}, catalog.OpenMemberIndex(cfg.WorkdirPath()))
	e.rst = restorer.New(restorer.Deps{
		Store:         e.fs,
		Archives:      e.cat.Archives,
		Exec:          e.tc.executorFor,
		ArchiverFor:   e.tc.restoreArchiver,
		EncryptionFor: e.dleEncryption,
		KnownHosts:    e.knownHosts,
		DisplayDLE:    e.DisplayDLE,
		CompressOpts:  e.tc.fopts,
		DecryptOpts:   e.tc.dcopts,
	})
	e.dmp = dumper.New(dumper.Config{
		ArchiverFor: e.tc.archiverFor,
		Exclude:     func(dt string) []string { return e.cfg.ResolveDumpType(dt).Exclude },
		Placement:   e.tc.encodePlacement,
		Threads:     e.tc.fopts.Threads,
		FrameSize:   e.cfg.FrameSizeBytes(),
		AtomCeiling: e.atomCeilingErr,
	})
	e.ver = e.newVerifier()
	e.acct = e.newLedger()
	e.sched = e.newScheduler()
	e.cop = e.newCopier()
	e.drl = e.newDriller()
	return e, nil
}

// Capacity returns the landing medium's total retainable bytes (0 = unbounded).
func (e *Engine) Capacity() int64 { return e.acct.Capacity() }

// CapacityStatus reports whether current usage exceeds capacity and the percent
// used (0 when unbounded).
func (e *Engine) CapacityStatus(current int64) (over bool, pct float64) {
	return e.acct.CapacityStatus(current)
}

// MediumAppendable reports whether a medium packs many runs per volume (the
// default) rather than one run per volume — so inventory can label a written
// non-appendable reel "used" instead of "append".
func (e *Engine) MediumAppendable(name string) bool { return e.acct.MediumAppendable(name) }

// MediumInfo is a per-medium summary for catalog visibility (`nb medium`). It is an
// alias for accounting.MediumInfo (which now owns the type) so callers — including
// internal/cli — are unaffected.
type MediumInfo = accounting.MediumInfo

// MediumStats is a medium's usage picture — composition, the catalog's recorded
// used-over-time ledger, and growth statistics (`nb medium <name>`, the webui medium
// page); MediumInfo's richer sibling. Aliased like MediumInfo so callers name the
// engine type.
type MediumStats = accounting.MediumStats

// UsagePoint is one run's step of a medium's retained-bytes composition (see
// MediumStats.ByRun).
type UsagePoint = accounting.UsagePoint

// UsageStats summarizes a medium's recorded usage curve (see MediumStats.Growth).
type UsageStats = accounting.UsageStats

// Media returns a summary of every configured medium, sorted by name.
func (e *Engine) Media() []MediumInfo { return e.acct.Media() }

// Medium returns the summary for one configured medium; ok is false if the name
// is unknown.
func (e *Engine) Medium(name string) (MediumInfo, bool) { return e.acct.Medium(name) }

// MediumStats returns a medium's usage history and derived statistics (used-capacity
// over time, full/incremental split, growth projection); ok is false for an unknown
// medium.
func (e *Engine) MediumStats(name string) (MediumStats, bool) { return e.acct.MediumStats(name) }

// DLESummaries returns the per-DLE catalog rollup behind `nb dle` — a thin facade
// over catalog.DLESummaries, which owns the computation.
func (e *Engine) DLESummaries() []catalog.DLESummary { return e.cat.DLESummaries() }

// MediumMinAge returns a medium's effective retention floor — its configured
// minimum_age, or the dump cycle when unset — the same value pruning enforces
// before retiring a run. An unknown name yields the default floor.
func (e *Engine) MediumMinAge(name string) time.Duration {
	return e.cfg.MinAgeFor(e.cfg.Media[name])
}

// RebuildCatalog rescans every configured medium that can be opened and rewrites
// the local cache, returning the number of distinct runs indexed. Media that
// can't be opened (e.g. an offline tape) are skipped with a warning.
func (e *Engine) RebuildCatalog(logf Logf) (int, error) {
	vol, err := e.dep.Landing()
	if err != nil {
		return 0, err
	}
	vols := map[string]media.Volume{e.dep.LandingName(): vol}
	for name := range e.cfg.Media {
		if name == e.dep.LandingName() {
			continue
		}
		vol, _, _, err := e.dep.MediumVolume(name)
		if err != nil {
			logf.Log("WARNING: skipping medium %q: %v", name, err)
			continue
		}
		vols[name] = vol
	}
	return e.cat.Rebuild(vols)
}

// writeTarget bundles a medium prepared for writing: the opened write face (whose Close
// releases the window's claim), its drive-bound part allocator, the fs Session (the
// run's archivefs.WriteStore — the fs write handle), and a serial writer bound to the two (for
// the direct CopyRun/Flush paths; the spool binds its own over routed seams).
type writeTarget struct {
	wm      depot.WriteMedium
	alloc   *librarian.Allocator
	session *archivefs.Session
	writer  *archiveio.Writer
}

// prepareWriterOn authors over an already-opened write face. It has two callers: the
// conductor's serial landing seam (landingSeams in conduct.go, when the medium has no
// parallel drives) and Flush (which keeps one open handle per landing across the
// crashed runs it drains, building a fresh Session per run over it). It is the one
// place the PrepareWrite -> Allocator -> OpenRun contract lives. The writer is bound
// to the medium's allocator and the Session, so each committed archive reports
// straight to the catalog; the spool later routes both seams onto its orchestrator.
func (e *Engine) prepareWriterOn(wm depot.WriteMedium, def config.Media, spec archiveio.RunSpec, now time.Time, logf Logf) (*writeTarget, error) {
	medium := wm.Name()
	partSize, exp, err := e.writePrelude(medium, now, logf)
	if err != nil {
		return nil, err
	}
	appendable := def.IsAppendable()
	volName, epoch, err := wm.PrepareWrite(appendable, exp.Label, now, librarian.Logf(logf))
	if err != nil {
		return nil, err
	}
	alloc := wm.Allocator(volName, epoch, appendable, partSize, now, librarian.Logf(logf))
	session := e.fs.OpenRun(e.cat, wm)
	writer := archiveio.NewWriter(alloc, session, spec, e.dep.Limiter(medium), func() time.Time { return now })
	return &writeTarget{wm: wm, alloc: alloc, session: session, writer: writer}, nil
}

// writePrelude resolves the shared prelude of every write path onto a medium: its
// per-part chunk bound and the volume the accountant expects the write to use,
// announced to the operator (see announceExpectation). Shared by prepareWriterOn and
// the conductor's parallel landing seam (landingSeams in conduct.go).
func (e *Engine) writePrelude(medium string, now time.Time, logf Logf) (partSize int64, exp VolumeExpectation, err error) {
	partSize, err = e.dep.PartSizeFor(medium)
	if err != nil {
		return 0, VolumeExpectation{}, err
	}
	exp = e.acct.ExpectedVolumeFor(medium, now)
	announceExpectation(medium, exp, logf)
	return partSize, exp, nil
}

// announceExpectation logs which labeled volume a write will use before it starts —
// the Amanda "amdump will expect tape X" cue, so an operator sees the named tape in run
// output, not only in `nb plan`. It is operator-facing identity (the Label name only)
// and a no-op for an appendable medium or an address-identified one (nothing to expect).
func announceExpectation(medium string, exp VolumeExpectation, logf Logf) {
	switch {
	case exp.Appendable || (exp.Label == "" && !exp.FreshVolume):
		// appendable extends in place; address-identified media carry no label.
	case exp.FreshVolume:
		logf.Log("medium %q: this run needs a fresh/blank volume (no reusable volume in the pool)", medium)
	case exp.Recycles > 0:
		logf.Log("medium %q: this run expects volume %q — recycling %d aged-out run(s) past retention", medium, exp.Label, exp.Recycles)
	default:
		logf.Log("medium %q: this run expects volume %q", medium, exp.Label)
	}
}

// PlanCopy resolves and validates a copy without writing (the `nb copy` dry-run); see copier.
func (e *Engine) PlanCopy(runID, fromMedia, targetMedia string, force bool) (CopyPlan, error) {
	return e.cop.PlanCopy(runID, fromMedia, targetMedia, force)
}

// CopyRun streams a sealed run from one configured medium to another; see copier.
func (e *Engine) CopyRun(runID, fromMedia, targetMedia string, force bool, logf Logf) error {
	return e.cop.CopyRun(runID, fromMedia, targetMedia, force, logf)
}

// CopyRuns streams several sealed runs onto a target in one spool run (drives stay saturated across
// runs); see copier. Used by sync.
func (e *Engine) CopyRuns(runIDs []string, fromMedia, targetMedia string, force bool, logf Logf) error {
	return e.cop.CopyRuns(runIDs, fromMedia, targetMedia, force, logf)
}

// LabelVolume writes (or rewrites) the identity label of a medium's volume — the
// deliberate operator act that makes a tape writable.
func (e *Engine) LabelVolume(mediumName, name string, relabel, force bool, now time.Time, logf Logf) error {
	am, _, err := e.dep.OpenAdmin(mediumName)
	if err != nil {
		return err
	}
	defer am.Close()
	return am.Label(name, relabel, force, now, librarian.Logf(logf))
}

// ChangerView inventories a changer medium for `nb medium <name>`.
func (e *Engine) ChangerView(mediumName string) (librarian.View, error) {
	am, _, err := e.dep.OpenAdmin(mediumName)
	if err != nil {
		return librarian.View{}, err
	}
	defer am.Close()
	return am.View()
}

// LoadVolume mounts a volume on a changer medium, by bay id or (byLabel) volume label.
func (e *Engine) LoadVolume(mediumName, target string, byLabel bool, logf Logf) error {
	am, _, err := e.dep.OpenAdmin(mediumName)
	if err != nil {
		return err
	}
	defer am.Close()
	return am.Load(target, byLabel, librarian.Logf(logf))
}

// Catalog exposes the catalog for read-only commands.
func (e *Engine) Catalog() *catalog.Catalog { return e.cat }

// placementOn returns the run's copy on the named medium, if any. It is the single
// "does this run have a copy here, and where" lookup shared by copy planning, the copy
// read side, and sync's skip check.
func (e *Engine) placementOn(runID, medium string) (catalog.Placement, bool) {
	return placementOn(e.cat, runID, medium)
}

// placementOn is the catalog lookup behind Engine.placementOn, shared with the copier
// (which holds the catalog directly).
func placementOn(cat *catalog.Catalog, runID, medium string) (catalog.Placement, bool) {
	for _, p := range cat.Placements(runID) {
		if p.Medium == medium {
			return p, true
		}
	}
	return catalog.Placement{}, false
}

// placedOn reports whether a run already has a copy recorded on the medium.
func (e *Engine) placedOn(runID, medium string) bool {
	_, ok := e.placementOn(runID, medium)
	return ok
}

// placementsFor returns a run's copies ordered for reading: the engine's own
// medium first (online/fast), then the rest.
func (e *Engine) placementsFor(runID string) []catalog.Placement {
	ps := e.cat.Placements(runID)
	sort.SliceStable(ps, func(i, j int) bool {
		return ps[i].Medium == e.dep.LandingName() && ps[j].Medium != e.dep.LandingName()
	})
	return ps
}

// StoredBytes is the bytes currently stored on the engine's own medium.
func (e *Engine) StoredBytes() int64 { return e.acct.StoredBytes() }

// Landing is the resolved name of the medium new dumps land on. Unlike the raw
// config field it is never empty — it reflects the sole-medium fallback New applied.
func (e *Engine) Landing() string { return e.dep.LandingName() }

// VolumeExpectation describes the volume the next run on a labeled medium will
// write to; see accounting (which owns the arithmetic).
type VolumeExpectation = accounting.VolumeExpectation

// ExpectedVolume reports the volume the next run on the landing medium will write to,
// or ok=false for address-identified media (disk, s3) that carry no label and so
// have no volume to expect; see accounting.
func (e *Engine) ExpectedVolume(now time.Time) (VolumeExpectation, bool) {
	return e.acct.ExpectedVolume(now)
}

// Plan builds the plan for a run date: it estimates every DLE, fulls the ones
// due by the cycle deadline, and promotes future fulls forward to level light
// runs (bounded by the per-run capacity room).
func (e *Engine) Plan(date time.Time) *planner.Plan {
	return e.sched.Plan(date, nil)
}

// PlanWithProgress is Plan with a live sink for the estimate phase, which can be
// slow: every DLE is sized by an archiver pass, so a long preview is otherwise
// silent. sink (nil to disable) receives a snapshot as each DLE's estimate starts
// and finishes.
func (e *Engine) PlanWithProgress(date time.Time, sink progress.Sink) *planner.Plan {
	return e.sched.Plan(date, sink)
}

// ValidatePlan checks each DLE the way a real run would resolve it, so a preview
// (`nb plan` / `nb dump --dry-run`) surfaces problems the size estimates would
// otherwise swallow into a misleading ~0 B. It runs the same pre-flight a real run
// does — the compression scheme and every dumptype's method and encryption scheme —
// returning a fatal error for an unrunnable config (an unknown compression/method/encryption scheme,
// a missing required key reference, or a scheme/gpg binary not on PATH), so a preview
// no longer gives a green light to a run that `nb dump` will reject. Source paths
// that are missing or unreadable right now are non-fatal warnings (they may be an
// unmounted volume the real run will mount).
func (e *Engine) ValidatePlan() (warnings []string, err error) {
	return e.sched.Validate()
}

// Simulate forecasts the next `days` daily runs from `start` without writing
// anything: it plans each day and advances a cloned history between them, so the
// level schedule — when each DLE's full next lands, how its incrementals climb — is
// projected forward. Estimates and the capacity ceiling are sampled once at `start`
// and held constant, so this is a schedule forecast, not a capacity timeline.
func (e *Engine) Simulate(start time.Time, days int) []*planner.Plan {
	return e.sched.Simulate(start, days)
}

// Run executes the plan for a date, producing one sealed run. It delegates to a
// per-run conductor.Conductor (see internal/conductor and newConductor); the engine
// just builds the run lane's dependency slice.
func (e *Engine) Run(ctx context.Context, now time.Time, logf Logf) (*catalog.Run, error) {
	// Open the landing now so a landing that won't open fails the run here rather
	// than mid-stream. The dry-run peer (PlannedRunID) reads only the catalog, so it
	// deliberately does not open the medium.
	if _, err := e.dep.Landing(); err != nil {
		return nil, err
	}
	return e.newConductor().Run(ctx, now, logf)
}

// PlannedRunID returns the run id a real dump at instant would mint. Like Run, it
// delegates to the per-run conductor.Conductor.
func (e *Engine) PlannedRunID(instant time.Time) string {
	return e.newConductor().PlannedRunID(instant)
}

// Restore reconstructs a DLE as of a run into destDir; see restorer.Extract.
func (e *Engine) Restore(runID, dleName, destDir string, force bool, logf Logf) error {
	return e.rst.Extract(restorer.Request{DLE: dleName, RunID: runID, Dest: destDir, Force: force}, logf)
}

// RestoreTo restores a DLE onto a remote client over SSH; see restorer.Extract.
func (e *Engine) RestoreTo(runID, dleName, destHost, destPath string, logf Logf) error {
	return e.rst.Extract(restorer.Request{DLE: dleName, RunID: runID, Dest: destPath, Host: destHost}, logf)
}

// fsDeps adapts the engine's services to the fs's ReadMap and Depot roles — the data
// path's view of the orchestrator (catalog placement, librarian read-mounts, bandwidth
// caps) — so that contract stays off the Engine's public API. The write face is the
// catalog itself, passed to OpenRun as the archivefs.WriteMap.
type fsDeps struct{ e *Engine }

// PlacementsFor returns a run's copies in read-preference order (own medium first) — the
// fs's ReadMap role (the engine keeps the catalog store + the directory/retention slices).
func (c fsDeps) PlacementsFor(runID string) []catalog.Placement {
	return c.e.placementsFor(runID)
}

// MounterFor returns a read-mount onto a medium's volumes — the fs's Mounter role,
// served by the depot's read face. A medium the open run window is writing is refused at
// the open: the window owns its drives, so a reader fails over to another copy
// (openRef treats this like any unavailable copy) instead of mounting mid-write.
// Outside a window nothing is held and every medium opens.
func (c fsDeps) MounterFor(medium string) (archivefs.Mounter, error) {
	return c.e.dep.OpenForRead(medium)
}

// Limiter returns a medium's shared bandwidth cap (nil = uncapped).
func (c fsDeps) Limiter(medium string) *ratelimit.Limiter { return c.e.dep.Limiter(medium) }

// RestoreAsOf reconstructs a whole DLE as of a date into destDir; see restorer.Extract.
// A non-empty from pins the read to that medium's copy (else any copy, with fail-over).
func (e *Engine) RestoreAsOf(dle, asOf, destDir, from string, force bool, logf Logf) error {
	if err := e.checkFromMedium(from); err != nil {
		return err
	}
	return e.rst.Extract(restorer.Request{DLE: dle, AsOf: asOf, Dest: destDir, Medium: from, Force: force}, logf)
}

// RestoreAsOfTo is RestoreAsOf onto a remote client over SSH; see restorer.Extract.
func (e *Engine) RestoreAsOfTo(dle, asOf, destHost, destPath, from string, logf Logf) error {
	if err := e.checkFromMedium(from); err != nil {
		return err
	}
	return e.rst.Extract(restorer.Request{DLE: dle, AsOf: asOf, Dest: destPath, Host: destHost, Medium: from}, logf)
}

// mediaNamesHint renders "(configured: a, b, c)" for an unknown-medium error, so a
// typo shows the operator the names they can actually use (mirroring `nb prune`).
func mediaNamesHint(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.Media))
	for n := range cfg.Media {
		names = append(names, n)
	}
	sort.Strings(names)
	return "(configured: " + strings.Join(names, ", ") + ")"
}

// checkFromMedium validates a `--from` medium pin against the config ("" is the
// default: any copy).
func (e *Engine) checkFromMedium(from string) error {
	if from != "" {
		if _, ok := e.cfg.Media[from]; !ok {
			return fmt.Errorf("unknown medium %q %s (--from)", from, mediaNamesHint(e.cfg))
		}
	}
	return nil
}

// dleEncryption resolves a DLE's configured encryption posture (the restorer's
// EncryptionFor dep); ok is false when the DLE is no longer in the config.
func (e *Engine) dleEncryption(name string) (config.EncryptConfig, bool) {
	for _, d := range e.cfg.DLEs() {
		if d.Name() == name {
			return e.cfg.EncryptionFor(d.DumpTypeName()), true
		}
	}
	return config.EncryptConfig{}, false
}

// knownHosts are the names a `--to` restore may target: hosts: entries plus
// configured source hosts.
func (e *Engine) knownHosts() []string {
	seen := map[string]bool{}
	for h := range e.cfg.Hosts {
		seen[h] = true
	}
	for _, d := range e.cfg.DLEs() {
		if d.Host != "" && d.Host != "localhost" {
			seen[d.Host] = true
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	return out
}

// OpenRecover builds a browsable filesystem of a DLE as of a date; see restorer.
func (e *Engine) OpenRecover(dle, asOf string) (*recovery.Tree, error) {
	return e.rst.OpenRecover(dle, asOf)
}

// OpenRecoverRun builds a browsable filesystem of a DLE as of an exact run — the
// mount's per-run snapshot view; see restorer.
func (e *Engine) OpenRecoverRun(dle, runID string) (*recovery.Tree, error) {
	return e.rst.OpenRecoverRun(dle, runID)
}

// ExtractSelection extracts a selected set of files (plus any delta-tipped
// assemblies) into destDir, reporting media reads to prog for a live progress
// view (nil disables it); see restorer.
func (e *Engine) ExtractSelection(steps []recovery.ExtractStep, asms []recovery.Assembly, destDir string, logf Logf, prog restorer.ReadProgress) (int, int, error) {
	return e.rst.ExtractSelection(steps, asms, destDir, logf, prog)
}

// DLENames returns the distinct DLE names recorded across all catalog runs,
// sorted — the DLEs a recovery session can choose from; see dleDirectory.
func (e *Engine) DLENames() []string { return e.dles.names() }

// DisplayDLE maps an internal DLE slug to its host:path identity for messages,
// falling back to the slug when host/path are unknown; see dleDirectory.
func (e *Engine) DisplayDLE(slug string) string { return e.dles.display(slug) }

// DLEDisplay returns the host:path identities of the DLEs a recovery session can
// choose from, sorted — the user-facing peer of DLENames; see dleDirectory.
func (e *Engine) DLEDisplay() []string { return e.dles.displayAll() }

// ResolveDLE maps a user-supplied DLE reference — a host:path identity or the raw
// internal slug — to the internal slug, or ("", false) if no catalog DLE matches;
// see dleDirectory (which also forgives a trailing slash from tab completion).
func (e *Engine) ResolveDLE(arg string) (string, bool) { return e.dles.resolve(arg) }

// ForceFull schedules a configured DLE for a full on its next run, the archiver-independent
// `nb reset`: it records a force-full directive the planner honors (a mandatory L0),
// rather than reaching into and deleting the archiver's incremental state. The forced full
// reseeds that state itself when it runs, and — with commit-bound promotion — the old
// chain stays intact until the new full actually commits. arg is a host:path identity or
// the internal slug; it returns the DLE's display identity. The DLE must be configured,
// since forcing a full only makes sense to re-dump it.
func (e *Engine) ForceFull(arg string) (string, error) {
	d, ok := e.dles.resolveConfigured(arg)
	if !ok {
		return "", fmt.Errorf("no DLE %q in the configuration", arg)
	}
	if err := e.cat.SetForceFull(d.Name()); err != nil {
		return "", fmt.Errorf("force full %s: %w", d.ID(), err)
	}
	return d.ID(), nil
}

// MediumOverCapacity reports whether the named medium still holds more than its
// capacity (a 0 capacity means unbounded). used and capacity are returned for
// messaging — used after a prune to tell the operator that reclaiming every dead
// archive was not enough because the protected recovery set alone exceeds capacity.
func (e *Engine) MediumOverCapacity(name string) (over bool, used, capacity int64, err error) {
	return e.acct.MediumOverCapacity(name)
}

// MediumProtectedOverCapacity reports whether the bytes a prune *cannot* reclaim —
// the protected recovery set — still exceed the medium's capacity. It subtracts
// everything Reclaim would free from the current total, so the answer is the same
// whether or not a real prune has run: a dry-run still sees the would-delete archives
// in the catalog while a completed prune has already removed them, but
// `residual = current − reclaimable` is identical either way (after a real prune the
// reclaimable set is empty and the current total is already the residual). This is
// what `nb prune` warns on, so its preview and its real run agree.
func (e *Engine) MediumProtectedOverCapacity(name string, now time.Time) (over bool, residual, capacity int64, err error) {
	return e.acct.MediumProtectedOverCapacity(name, now)
}

// MediumProtectionIsAgeBound reports whether every archive pinning the medium over
// capacity is held by the minimum_age floor (vs a live recovery chain). When false,
// advising the operator to shorten minimum_age is useless — a DLE's last full and its
// later incrementals are pinned regardless of age — so the remedy text drops it.
func (e *Engine) MediumProtectionIsAgeBound(name string, now time.Time) bool {
	return e.acct.MediumProtectionIsAgeBound(name, now)
}

// ProjectedOverCapacity reports whether the named medium would exceed its capacity
// after add more bytes land on it (a 0 capacity means unbounded) — the check
// `nb copy` runs before/after a copy so it warns about overshooting a target's
// budget the way `nb sync` already does.
func (e *Engine) ProjectedOverCapacity(name string, add int64) (over bool, projected, capacity int64, err error) {
	return e.acct.ProjectedOverCapacity(name, add)
}

// Prune reconciles a named medium to its own retention model: it computes that
// medium's protected runs (its own minimum_age and last-recovery-path floor) and
// asks its retention strategy which non-protected runs to reclaim to fit its
// capacity. Retention is per-medium, so each store is pruned against its own runs
// — pruning one medium never touches a copy on another. Any configured medium can
// be pruned (not only the landing one), so an offsite tier can be trimmed too.
func (e *Engine) Prune(mediumName string, now time.Time, apply bool, logf Logf) (eligible int, swept int, freed int64, err error) {
	return e.acct.Prune(mediumName, now, apply, logf)
}
