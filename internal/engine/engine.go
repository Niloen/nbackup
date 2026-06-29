// Package engine is NBackup's orchestrator. It
// wires the planner, archiver, transfer pipeline, media store, catalog, and
// retention together to execute runs, restores, verification, and pruning. It is
// the only place that knows about all the abstractions at once; everything below
// it depends only on interfaces.
package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/accounting"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/dumper"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/retention"
	"github.com/Niloen/nbackup/internal/scheduler"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"

	// Register the bundled media and archiver implementations.
	_ "github.com/Niloen/nbackup/internal/archiver/gnutar"
	_ "github.com/Niloen/nbackup/internal/media/cloud"
	_ "github.com/Niloen/nbackup/internal/media/disk"
	_ "github.com/Niloen/nbackup/internal/media/tape"
)

// Logf is an optional progress logger. It is an alias for logf.Logf, which lives in
// a leaf package so the lanes split out of the engine (accounting, scheduler,
// conductor) can all take one without an import cycle through the engine.
type Logf = logf.Logf

// Engine holds the wired-up components for one configuration. It owns the media
// volume; the catalog is a local cache the engine refreshes from the volume.
// Archivers are resolved per dumptype and cached.
type Engine struct {
	cfg            *config.Config
	mediumName     string       // name of the medium new dumps land on
	mediumDef      config.Media // its definition
	vol            media.Volume
	clerk          *clerk.Clerk // the archive data path (read+write composer); the engine implements its Deps
	profile        media.Profile
	landingCost    media.Cost // landing medium's pricing (dollar peer of profile)
	minAge         time.Duration
	cat            *catalog.Catalog
	archivers      map[string]archiver.Archiver  // by cache key (dumptype or "@type")
	compressScheme string                        // compression scheme for new archives
	fopts          compress.Options              // compress invocation options (level/threads/nice)
	dcopts         crypt.Options                 // decrypt key reference for restore (from the default encrypt block)
	op             librarian.Operator            // optional: handles manual tape swaps (nil = unattended)
	runSink        progress.Sink                 // optional: live run-progress sink (nil = status file only)
	estimateSink   progress.Sink                 // optional: live estimate-progress sink (nil = status file only)
	limiters       map[string]*ratelimit.Limiter // per-medium bandwidth cap (nil entry = uncapped); shared so a medium's concurrent streams share one budget
	dec            *decoder                      // the read-side scheme operation (restore/verify/list); shares the engine's resolution + decode opts
	dmp            *dumper.Dumper                // the producer (dump): workers + tar source + encode pipeline; the engine injects its resolution
	ver            *verifier                     // the verification operation (verify/drill checks); shares catalog + data path + decoder
	cop            *copier                       // the copy operation (PlanCopy/CopySlot); shares catalog + data path + write machinery
	rst            *restorer                     // the restore/recover operation; shares catalog + data path + decoder + config
	acct           *accounting.Accountant        // capacity/retention arithmetic; the engine's capacity methods delegate here
	sched          *scheduler.Scheduler          // plan/estimate/validate lane; the engine's plan methods delegate here
}

// SetOperator attaches an operator so manual single-drive media can prompt for a
// reel swap mid-command. Without one, manual swaps degrade to an actionable error.
func (e *Engine) SetOperator(op librarian.Operator) { e.op = op }

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

// librarianFor builds a librarian for a configured medium's open volume. For the
// engine's own medium it wraps the already-open landing handle (own=true), so its
// cached state stays coherent and the catalog — which caches exactly this medium —
// can be rebuilt against it.
// ErrUnknownMedium marks a medium name absent from the current config — a copy
// recorded in the catalog on a medium this config does not define. It is a scoping
// condition, not corruption: verify skips such a copy rather than failing it.
var ErrUnknownMedium = errors.New("unknown medium")

func (e *Engine) librarianFor(name string) (lib *librarian.Librarian, def config.Media, own bool, err error) {
	vol, d, own, err := e.mediumVolume(name)
	if err != nil {
		return nil, config.Media{}, false, err
	}
	return librarian.New(vol, name, e.cat, e.op, e.cfg.AutoLabel, e.cfg.MinAgeFor(d)), d, own, nil
}

// New constructs an Engine from configuration: it opens the landing volume and
// its capacity profile via the media registry, and loads the catalog cache
// (refreshing it from the volume the first time it is needed). Archivers are
// opened lazily per dumptype.
func New(cfg *config.Config) (*Engine, error) {
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
	vol, err := media.OpenVolume(mediaDef.Type, media.Options(mediaDef.Params))
	if err != nil {
		// Opening a cloud volume lists the bucket, so this is where absent SDK
		// credentials or an unreachable store first surface. Name the medium and
		// point at the credential source rather than leaking the raw provider error.
		if mediaDef.Type == "cloud" {
			return nil, fmt.Errorf("cannot reach landing medium %q: %w\n(a cloud store reads its credentials from the SDK environment: AWS_*, GOOGLE_APPLICATION_CREDENTIALS, or AZURE_*)", name, err)
		}
		return nil, fmt.Errorf("cannot open landing medium %q: %w", name, err)
	}
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
	if err := cat.EnsureFresh(name, vol); err != nil {
		// The one-time bootstrap scan indexes whatever the landing medium already
		// holds; once the local catalog cache exists, planning/listing is fully
		// offline. A cloud store fails here only when its SDK credentials are absent
		// or it is unreachable — surface that legibly with the medium named, rather
		// than the raw provider SDK error.
		hint := ""
		if mediaDef.Type == "cloud" {
			hint = " — a cloud store reads its credentials from the SDK environment (AWS_*, GOOGLE_APPLICATION_CREDENTIALS, or AZURE_*); set them, or run where the catalog cache already exists"
		}
		return nil, fmt.Errorf("cannot reach landing medium %q to index existing backups: %w%s", name, err, hint)
	}
	minAge := cfg.MinAgeFor(mediaDef)
	fopts := compress.Options{
		Program: cfg.Compress.Program,
		Level:   cfg.Compress.Level,
		Threads: cfg.Compress.Threads,
		Nice:    cfg.Nice,
	}
	// Decrypt options for restore come from the default encrypt block: the scheme to
	// reverse is recorded per-archive, but the key reference (passphrase file, binary
	// override) is supplied by the operator here; public-key schemes need none.
	dcopts := crypt.Options{
		Program:        cfg.Encrypt.Program,
		PassphraseFile: cfg.Encrypt.PassphraseFile,
		Nice:           cfg.Nice,
	}
	e := &Engine{
		cfg:            cfg,
		mediumName:     name,
		mediumDef:      mediaDef,
		vol:            vol,
		profile:        profile,
		landingCost:    costModel,
		minAge:         minAge,
		cat:            cat,
		archivers:      map[string]archiver.Archiver{},
		compressScheme: cfg.CompressScheme(),
		fopts:          fopts,
		dcopts:         dcopts,
		limiters:       limiters,
	}
	e.clerk = clerk.New(e, e, catalog.OpenMemberIndex(cfg.WorkdirPath()))
	e.dec = e.newDecoder()
	e.dmp = dumper.New(dumper.Config{
		ArchiverFor: e.archiverFor,
		Exclude:     func(dt string) []string { return e.cfg.ResolveDumpType(dt).Exclude },
		Placement:   e.encodePlacement,
		Threads:     e.fopts.Threads,
	})
	e.ver = e.newVerifier()
	e.acct = e.newLedger()
	e.sched = e.newScheduler()
	e.cop = e.newCopier()
	e.rst = e.newRestorer()
	return e, nil
}

// encryptionFor resolves the encryption scheme and encryptor options for a
// dumptype's dumps: the dumptype's own `encrypt` block, else the config default.
// A plaintext dump returns "" (not "none") so the scheme is omitted from the
// recorded header rather than written as noise.
func (e *Engine) encryptionFor(dtName string) (scheme string, opts crypt.Options) {
	ec := e.cfg.EncryptionFor(dtName)
	scheme = ec.Scheme
	if scheme == "none" {
		scheme = ""
	}
	return scheme, crypt.Options{
		Program:        ec.Program,
		Recipient:      ec.Recipient,
		PassphraseFile: ec.PassphraseFile,
		Nice:           e.cfg.Nice,
	}
}

// mediumVolume returns a Volume for the named medium. For the engine's own
// medium it returns the already-open handle (own=true) so that handle's cached
// state stays coherent and the catalog — which caches exactly this medium — can be
// rebuilt against it; any other medium is opened as a fresh handle. This is the
// single place that distinguishes "my medium" from the rest, so the rest of the
// engine never compares medium names itself.
func (e *Engine) mediumVolume(name string) (vol media.Volume, def config.Media, own bool, err error) {
	if name == e.mediumName {
		return e.vol, e.mediumDef, true, nil
	}
	d, ok := e.cfg.Media[name]
	if !ok {
		return nil, config.Media{}, false, fmt.Errorf("%w %q", ErrUnknownMedium, name)
	}
	v, err := media.OpenVolume(d.Type, media.Options(d.Params))
	return v, d, false, err
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

// Media returns a summary of every configured medium, sorted by name.
func (e *Engine) Media() []MediumInfo { return e.acct.Media() }

// Medium returns the summary for one configured medium; ok is false if the name
// is unknown.
func (e *Engine) Medium(name string) (MediumInfo, bool) { return e.acct.Medium(name) }

// volumesInPool returns the labeled volumes the catalog tracks for a medium
// (matched by the label pool == medium name), sorted by name.
func (e *Engine) volumesInPool(medium string) []catalog.VolumeRecord {
	var out []catalog.VolumeRecord
	for _, v := range e.cat.Volumes() { // already sorted by name
		if v.Label.Pool == medium {
			out = append(out, v)
		}
	}
	return out
}

// archiverFor resolves and caches the archiver for a (dumptype, host): the dumptype's
// named archiver definition (its type + options), with the executor for the DLE's host
// and that host's incremental-state root. A remote host yields an SSH executor (so tar
// runs on the client) and a client-side state root; a local/unlisted host yields the
// local executor and the server-side state root.
func (e *Engine) archiverFor(dtName, host string) (archiver.Archiver, error) {
	dt := e.cfg.ResolveDumpType(dtName)
	def := e.cfg.ResolveArchiver(dt.Archiver)
	return e.openArchiver(dtName+"\x00"+host, def.Type, def.Options, host)
}

// openArchiver returns the cached archiver for key, or opens one of typeName for the host
// (with that host's executor, per-type option overrides, and incremental-state root) and
// caches it. It is the shared get-or-open the dump-side archiverFor and read-side
// restoreArchiver both use; they differ only in the cache key and whether a definition's
// options apply.
func (e *Engine) openArchiver(key, typeName string, options map[string]string, host string) (archiver.Archiver, error) {
	if d, ok := e.archivers[key]; ok {
		return d, nil
	}
	ex := e.executorFor(host)
	opts := e.archiverOptions(typeName, options, host)
	// The host's state_dir is shared by every archiver on it; give this one a private
	// subtree named by its type (e.g. <state_dir>/gnutar) so two archivers can't collide.
	stateRoot := filepath.Join(e.cfg.StateDirFor(host), typeName)
	d, err := archiver.Open(typeName, opts, ex, stateRoot)
	if err != nil {
		return nil, err
	}
	e.archivers[key] = d
	return d, nil
}

// preflightDumptype validates one dumptype's pipeline tools before a dump: it resolves
// the archiver for (dumptype, host) — and runs its readiness Check when checkArchiver is
// set (the real dump does; plan validation only resolves) — and validates the dumptype's
// encryptor once. checked memoizes the per-dumptype encryption check across a plan's many
// DLEs. It is the shared pre-flight Run and ValidatePlan both run.
func (e *Engine) preflightDumptype(dt, host string, checkArchiver bool, checked map[string]bool) error {
	arch, err := e.archiverFor(dt, host)
	if err != nil {
		return fmt.Errorf("dumptype %q: %w", dt, err)
	}
	if checkArchiver {
		if err := arch.Check(); err != nil {
			return err
		}
	}
	if !checked[dt] {
		scheme, opts := e.encryptionFor(dt)
		if err := crypt.Check(scheme, opts); err != nil {
			return err
		}
		checked[dt] = true
	}
	return nil
}

// restoreArchiver resolves and caches an archiver by its type name for reading, built
// with the executor for the DLE's host: a remote DLE extracts on the client (tar runs
// there, the destination path is on the client — restore/recover land back where the
// data came from), a local/unlisted host extracts server-side exactly as before. The
// archive records its producing archiver's type; restore reverses it.
func (e *Engine) restoreArchiver(typeName, host string) (archiver.Archiver, error) {
	return e.openArchiver("@"+typeName+"\x00"+host, typeName, nil, host)
}

// executorFor returns the executor a DLE's host runs its tools on — the local machine for
// an empty or unlisted host, or an SSH executor for a host configured in the hosts: map.
// This is the one place "ssh" enters the engine; the archiver never learns it. Per-host
// state_dir and archiver-option overrides are resolved separately (see openArchiver), so
// a client's tar path and .snar root no longer ride on the executor.
func (e *Engine) executorFor(host string) programs.Executor {
	hc, ok := e.cfg.RemoteHost(host)
	if !ok {
		return programs.Local()
	}
	return programs.SSH(programs.Params{
		User:         hc.User,
		Host:         host,
		Port:         hc.Port,
		IdentityFile: hc.IdentityFile,
		Options:      hc.Options,
	})
}

// probeReachable verifies a remote source host answers over SSH before a dump
// touches it, so an unreachable client surfaces as a transport error (as `nb check`
// reports it) rather than the misleading "GNU tar is required" the tar probe would
// emit when it runs over the dead connection. Local hosts are always reachable.
func (e *Engine) probeReachable(host string) error {
	ssh, remote := e.cfg.RemoteHost(host)
	if !remote {
		return nil
	}
	ex := e.executorFor(host)
	if err := ex.Command("true").Run(); err != nil {
		return fmt.Errorf("source host %q unreachable over SSH (%s): %w — run `nb check` to diagnose", host, sshTarget(host, ssh), err)
	}
	return nil
}

// archiverOptions copies an archiver definition's options and merges this host's per-type
// overrides (`hosts.<host>.archivers.<typeName>`) over them — a client whose tar binary
// lives off the default PATH sets it there. The incremental-state root is not here: it is
// a host-level location passed to archiver.Open as stateRoot (see openArchiver).
func (e *Engine) archiverOptions(typeName string, options map[string]string, host string) archiver.Options {
	opts := archiver.Options{}
	for k, v := range options {
		opts[k] = v
	}
	for k, v := range e.cfg.ArchiverOverrides(host, typeName) {
		opts[k] = v
	}
	return opts
}

// RebuildCatalog rescans every configured medium that can be opened and rewrites
// the local cache, returning the number of distinct slots indexed. Media that
// can't be opened (e.g. an offline tape) are skipped with a warning.
func (e *Engine) RebuildCatalog(logf Logf) (int, error) {
	vols := map[string]media.Volume{e.mediumName: e.vol}
	for name := range e.cfg.Media {
		if name == e.mediumName {
			continue
		}
		vol, _, _, err := e.mediumVolume(name)
		if err != nil {
			logf.Log("WARNING: skipping medium %q: %v", name, err)
			continue
		}
		vols[name] = vol
	}
	return e.cat.Rebuild(vols)
}

// writeTarget bundles a medium prepared for writing: a librarian whose first volume
// is mounted and label-verified, the archiveio writer streaming the slot onto it, and the
// medium's part_size (so the caller can decide parallelism via lib.CanSpan).
type writeTarget struct {
	lib      *librarian.Librarian
	w        *archiveio.Writer
	partSize int64
}

// prepareWriter resolves a medium, enforces the label protocol on its loaded volume
// (prompting a swap on a manual single drive), and builds a archiveio writer that
// authors the slot described by spec onto it. It is the one place the PrepareWrite
// -> WriteSink -> NewWriter contract lives, shared by a dump (Run) and a copy/sync
// (CopySlot).
func (e *Engine) prepareWriter(medium string, spec archiveio.SlotSpec, now time.Time, logf Logf) (*writeTarget, error) {
	return e.prepareWriterWith(medium, spec, now, logf, nil)
}

// prepareWriterWith is prepareWriter with a seam to wrap the medium's volume sink before the
// Writer is built over it (wrap nil is the identity). The drain uses it to substitute an
// orchestrator-client proxy sink for the librarian's control calls — NextPart/PlaceRecord, where a
// volume roll touches the catalog — routing them to the orchestrator, while the byte-copy runs on the
// spool's copy goroutine. Wrapping here keeps the single PrepareWrite→WriteSink→NewWriter contract in one place.
func (e *Engine) prepareWriterWith(medium string, spec archiveio.SlotSpec, now time.Time, logf Logf, wrap func(archiveio.VolumeSink) archiveio.VolumeSink) (*writeTarget, error) {
	lib, def, _, err := e.librarianFor(medium)
	if err != nil {
		return nil, err
	}
	partSize, err := e.partSizeFor(medium)
	if err != nil {
		return nil, err
	}
	appendable := def.IsAppendable()
	exp := e.expectedVolumeFor(medium, now)
	announceExpectation(medium, exp, logf)
	volName, epoch, err := lib.PrepareWrite(appendable, exp.Label, now, librarian.Logf(logf))
	if err != nil {
		return nil, err
	}
	var sink archiveio.VolumeSink = lib.WriteSink(volName, epoch, appendable, partSize, now, librarian.Logf(logf))
	if wrap != nil {
		sink = wrap(sink)
	}
	w := archiveio.NewWriter(sink, spec, e.limiters[medium], func() time.Time { return now })
	return &writeTarget{lib: lib, w: w, partSize: partSize}, nil
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
		logf.Log("medium %q: this run needs a fresh/blank volume (no reusable tape in the pool)", medium)
	case exp.Recycles > 0:
		logf.Log("medium %q: this run expects volume %q — recycling %d aged-out run(s) past retention", medium, exp.Label, exp.Recycles)
	default:
		logf.Log("medium %q: this run expects volume %q", medium, exp.Label)
	}
}

// PlanCopy resolves and validates a copy without writing (the `nb copy` dry-run); see copier.
func (e *Engine) PlanCopy(slotID, fromMedia, targetMedia string, force bool) (CopyPlan, error) {
	return e.cop.PlanCopy(slotID, fromMedia, targetMedia, force)
}

// CopySlot streams a sealed slot from one configured medium to another; see copier.
func (e *Engine) CopySlot(slotID, fromMedia, targetMedia string, force bool, logf Logf) error {
	return e.cop.CopySlot(slotID, fromMedia, targetMedia, force, logf)
}

// partSizeFor reads a medium's optional part_size parameter (the deliberate per-part
// chunk bound). It must be at least two header blocks so a part can carry payload.
func (e *Engine) partSizeFor(medium string) (int64, error) {
	d, ok := e.cfg.Media[medium]
	if !ok {
		return 0, fmt.Errorf("unknown medium %q", medium)
	}
	s := d.Params["part_size"]
	if s == "" {
		return 0, nil
	}
	n, err := sizeutil.ParseBytes(s)
	if err != nil {
		return 0, fmt.Errorf("medium %q part_size: %w", medium, err)
	}
	if n < 2*record.HeaderBlock {
		return 0, fmt.Errorf("medium %q part_size %s is too small; use at least %s", medium, sizeutil.FormatBytes(n), sizeutil.FormatBytes(2*record.HeaderBlock))
	}
	return n, nil
}

// LabelVolume writes (or rewrites) the identity label of a medium's volume — the
// deliberate operator act that makes a tape writable.
func (e *Engine) LabelVolume(mediumName, name string, relabel, force bool, now time.Time, logf Logf) error {
	lib, def, _, err := e.librarianFor(mediumName)
	if err != nil {
		return err
	}
	minAge, _ := def.MinAge()
	return lib.Label(name, relabel, force, minAge, now, librarian.Logf(logf))
}

// ChangerView inventories a changer medium for `nb medium <name>`.
func (e *Engine) ChangerView(mediumName string) (librarian.View, error) {
	lib, _, _, err := e.librarianFor(mediumName)
	if err != nil {
		return librarian.View{}, err
	}
	return lib.View()
}

// LoadVolume mounts a volume on a changer medium, by bay/reel id or (byLabel) label.
func (e *Engine) LoadVolume(mediumName, target string, byLabel bool, logf Logf) error {
	lib, _, _, err := e.librarianFor(mediumName)
	if err != nil {
		return err
	}
	return lib.Load(target, byLabel, librarian.Logf(logf))
}

// Catalog exposes the catalog for read-only commands.
func (e *Engine) Catalog() *catalog.Catalog { return e.cat }

// placementOn returns the slot's copy on the named medium, if any. It is the single
// "does this slot have a copy here, and where" lookup shared by copy planning, the copy
// read side, and sync's skip check.
func (e *Engine) placementOn(slotID, medium string) (catalog.Placement, bool) {
	for _, p := range e.cat.Placements(slotID) {
		if p.Medium == medium {
			return p, true
		}
	}
	return catalog.Placement{}, false
}

// placementsFor returns a slot's copies ordered for reading: the engine's own
// medium first (online/fast), then the rest.
func (e *Engine) placementsFor(slotID string) []catalog.Placement {
	ps := e.cat.Placements(slotID)
	sort.SliceStable(ps, func(i, j int) bool {
		return ps[i].Medium == e.mediumName && ps[j].Medium != e.mediumName
	})
	return ps
}

// StoredBytes is the bytes currently stored on the engine's own medium.
func (e *Engine) StoredBytes() int64 { return e.acct.StoredBytes() }

// Landing is the resolved name of the medium new dumps land on. Unlike the raw
// config field it is never empty — it reflects the sole-medium fallback New applied.
func (e *Engine) Landing() string { return e.mediumName }

// VolumeExpectation describes the volume the next run on a labeled medium will
// write to. It is derived from the catalog's volume registry and the retention
// policy, never from a physical scan: for a one-run-per-tape (non-appendable) medium it names the
// oldest reusable volume the run would recycle, or a fresh tape when none is
// reusable; for an appendable medium it names the current volume the run extends.
type VolumeExpectation struct {
	Medium      string    // the labeled medium this expectation is for
	Appendable  bool      // true: extend a volume; false: one run per tape (recycle/fresh)
	Label       string    // the expected volume's label; "" when a fresh tape is expected
	WrittenAt   time.Time // when that volume was last labeled (zero for a fresh tape)
	Recycles    int       // runs on it the next run would overwrite (non-appendable reuse)
	FreshVolume bool      // no reusable volume exists — a fresh/blank tape is expected
	VolumeBytes int64     // the reel's physical capacity (volume_size); 0 = unknown/unsized
	UsedBytes   int64     // bytes already on the expected reel (0 for a fresh/recycled reel)
}

// ExpectedVolume reports the tape the next run on the landing medium will write to,
// or ok=false for address-identified media (disk, s3) that carry no label and so
// have no tape to expect.
func (e *Engine) ExpectedVolume(now time.Time) (VolumeExpectation, bool) {
	lib, _, _, err := e.librarianFor(e.mediumName)
	if err != nil || !lib.Labeled() {
		return VolumeExpectation{}, false
	}
	exp := e.expectedVolumeFor(e.mediumName, now)
	// The reel's capacity and current fill bound this run physically: an appendable
	// run extends the latest reel (room = size - used), a fresh or recycled reel
	// offers a whole reel (used stays 0).
	exp.VolumeBytes = e.profile.VolumeSize()
	if exp.Appendable && !exp.FreshVolume {
		for _, s := range e.cat.SlotsOnLabel(exp.Label) {
			exp.UsedBytes += s.TotalBytes()
		}
	}
	return exp, true
}

// expectedVolumeFor computes the expected volume for a labeled medium from the
// catalog's volume registry ordered oldest-written-first. A
// non-appendable run reuses the oldest volume whose every run is unprotected (the
// retention safety floor: past minimum age, with a newer recovery path); an
// appendable run extends the most recently written volume in the pool.
func (e *Engine) expectedVolumeFor(medium string, now time.Time) VolumeExpectation {
	def := e.cfg.Media[medium]
	exp := VolumeExpectation{Medium: medium, Appendable: def.IsAppendable()}

	// volumesInPool returns the same pool sorted by name; this expectation wants
	// oldest-written-first, so copy and re-sort rather than duplicate the compress.
	pool := append([]catalog.VolumeRecord(nil), e.volumesInPool(medium)...)
	sort.Slice(pool, func(i, j int) bool { return pool[i].Label.WrittenAt.Before(pool[j].Label.WrittenAt) })

	if exp.Appendable {
		if n := len(pool); n > 0 {
			exp.Label, exp.WrittenAt = pool[n-1].Label.Name, pool[n-1].Label.WrittenAt
		} else {
			exp.FreshVolume = true
		}
		return exp
	}

	minAge := e.cfg.MinAgeFor(def)
	// Retention is per-medium: a volume is reusable only when this medium no
	// longer needs its runs, so protection is computed over this medium's own
	// slots. Scoping to e.cat.Slots() (all media) would recycle a tape merely
	// because a newer full landed on disk — discarding the offsite copy and the
	// redundancy double storage exists to provide.
	floor := retention.Compute(e.cat.ArchivesOn(medium), minAge, now)
	for _, v := range pool {
		held := e.cat.SlotIDsOnLabel(v.Label.Name)
		if _, _, ok := floor.First(held); ok {
			continue // some slot on this tape is still kept — not reusable
		}
		exp.Label, exp.WrittenAt, exp.Recycles = v.Label.Name, v.Label.WrittenAt, len(held)
		return exp
	}
	exp.FreshVolume = true // nothing reusable — the run needs a fresh tape
	return exp
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

// capacityRoom is the hard per-run write ceiling fed to the planner: the most a
// single run may write. It is the tighter of two independent bounds — the pool's
// free room (retention: capacity minus the protected set, the bytes pruning
// cannot reclaim) and the landing volume's remaining room (physical: a run fills
// the reel it appends to before spilling to the next). Either is unbounded (-1)
// on media that lack it — object stores have no reel, a bare drive has no bounded
// pool — and the result is unbounded only when both are.
func (e *Engine) capacityRoom(now time.Time) int64 {
	return minRoom(e.acct.PoolRoom(now), e.volumeRoom(now))
}

// volumeRoom is the physical bound: the bytes left on the reel the run lands on
// before it spills to the next. An appendable run extends the latest reel, so its
// room is volume_size minus what is already on it; a fresh or recycled reel
// offers a whole volume_size. Negative = unbounded (the medium has no reel size).
func (e *Engine) volumeRoom(now time.Time) int64 {
	exp, ok := e.ExpectedVolume(now)
	if !ok || exp.VolumeBytes <= 0 {
		return -1
	}
	if room := exp.VolumeBytes - exp.UsedBytes; room > 0 {
		return room
	}
	return 0
}

// minRoom returns the tighter of two per-run ceilings, treating negative as
// unbounded (no bound from that source); the result is unbounded only when both
// inputs are.
func minRoom(a, b int64) int64 {
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// Run executes the plan for a date, producing one sealed slot. It delegates to a
// per-run conductor.Conductor (see internal/conductor and newConductor); the engine
// just builds the run lane's dependency slice.
func (e *Engine) Run(now time.Time, logf Logf) (*catalog.Slot, error) {
	return e.newConductor().Run(now, logf)
}

// PlannedSlotID returns the slot id a real dump on date would seal next. Like Run, it
// delegates to the per-run conductor.Conductor.
func (e *Engine) PlannedSlotID(date time.Time) string {
	return e.newConductor().PlannedSlotID(date)
}

// Restore reconstructs a DLE as of a slot into destDir; see restorer.
func (e *Engine) Restore(slotID, dleName, destDir string, force bool, logf Logf) error {
	return e.rst.Restore(slotID, dleName, destDir, force, logf)
}

// RestoreTo restores a DLE onto a remote client over SSH; see restorer.
func (e *Engine) RestoreTo(slotID, dleName, destHost, destPath string, logf Logf) error {
	return e.rst.RestoreTo(slotID, dleName, destHost, destPath, logf)
}

// The engine implements clerk.Deps: the data path's view of the orchestrator's
// services (catalog placement, librarian mounting, executor/archiver resolution, and the
// config-derived transform options/placement).

// PlacementsFor returns a slot's copies in read-preference order (own medium first), and
// AddArchive records a run's archives — together they are the clerk's Map role (the
// engine keeps the catalog store + the directory/retention slices).
func (e *Engine) PlacementsFor(slotID string) []catalog.Placement { return e.placementsFor(slotID) }
func (e *Engine) AddArchive(arch record.Archive, medium string, pos record.ArchivePos) error {
	return e.cat.AddArchive(arch, medium, pos)
}
func (e *Engine) RemoveArchive(slotID, medium, dle string) (placementGone, entryGone bool, err error) {
	return e.cat.RemoveArchive(slotID, medium, dle)
}

// MounterFor returns a read-mount onto a medium's volumes — the clerk's Mounter role, served
// by the medium's librarian (whose admin face stays with the label/load operations).
func (e *Engine) MounterFor(medium string) (clerk.Mounter, error) {
	lib, _, _, err := e.librarianFor(medium)
	return lib, err
}

// Limiter returns a medium's shared bandwidth cap (nil = uncapped).
func (e *Engine) Limiter(medium string) *ratelimit.Limiter { return e.limiters[medium] }

// Executor returns the transport that runs programs on a host — used by the engine's own
// restore composition (transfer.go).
func (e *Engine) Executor(host string) programs.Executor {
	return e.executorFor(host)
}

// RestoreAsOf reconstructs a whole DLE as of a date into destDir; see restorer. A
// non-empty from pins the read to that medium's copy (else any copy, with fail-over).
func (e *Engine) RestoreAsOf(dle, asOf, destDir, from string, force bool, logf Logf) error {
	return e.rst.RestoreAsOf(dle, asOf, destDir, from, force, logf)
}

// RestoreAsOfTo is RestoreAsOf onto a remote client over SSH; see restorer.
func (e *Engine) RestoreAsOfTo(dle, asOf, destHost, destPath, from string, logf Logf) error {
	return e.rst.RestoreAsOfTo(dle, asOf, destHost, destPath, from, logf)
}

// decryptHint augments an extraction failure on an encrypted archive with the
// actionable cause restore-time decryption needs. gpg's raw "No secret key" is
// misleading for a symmetric (passphrase) dump — the real fix is to supply the
// passphrase the run had — so name both possibilities rather than leaving the
// operator with gpg's message alone. A nil error or a plaintext archive pass through.
func decryptHint(scheme string, err error) error {
	if err == nil || scheme == "" {
		return err
	}
	return fmt.Errorf("%w\n(this archive is %s-encrypted, so extraction needs the key: for a passphrase/symmetric dump add an `encrypt:` block with the same passphrase_file; for a public-key dump ensure its private key is in the gpg keyring)", err, scheme)
}

// OpenRecover builds a browsable filesystem of a DLE as of a date; see restorer.
func (e *Engine) OpenRecover(dle, asOf string) (*recovery.Tree, error) {
	return e.rst.OpenRecover(dle, asOf)
}

// ExtractSelection extracts a selected set of files into destDir; see restorer.
func (e *Engine) ExtractSelection(steps []recovery.ExtractStep, destDir string, logf Logf) (int, error) {
	return e.rst.ExtractSelection(steps, destDir, logf)
}

// DLENames returns the distinct DLE names recorded across all catalog slots,
// sorted — the DLEs a recovery session can choose from.
func (e *Engine) DLENames() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range e.cat.Slots() {
		for _, a := range s.Archives {
			if !seen[a.DLE] {
				seen[a.DLE] = true
				out = append(out, a.DLE)
			}
		}
	}
	sort.Strings(out)
	return out
}

// dleDisplayMap maps each internal DLE slug to its host:path identity,
// drawing on both the config and the catalog (so a DLE no longer in the config still
// shows its real identity from the seal). The slug stays the internal key; host:path
// is the user-facing form.
func (e *Engine) dleDisplayMap() map[string]string {
	m := map[string]string{}
	for _, d := range e.cfg.DLEs() {
		m[d.Name()] = d.ID()
	}
	for _, s := range e.cat.Slots() {
		for _, a := range s.Archives {
			if a.Host == "" && a.Path == "" {
				continue
			}
			if _, ok := m[a.DLE]; !ok {
				m[a.DLE] = a.Host + ":" + a.Path
			}
		}
	}
	return m
}

// DisplayDLE maps an internal DLE slug to its host:path identity for messages,
// falling back to the slug when host/path are unknown.
func (e *Engine) DisplayDLE(slug string) string {
	if id, ok := e.dleDisplayMap()[slug]; ok {
		return id
	}
	return slug
}

// DLEDisplay returns the host:path identities of the DLEs a recovery session can
// choose from, sorted — the user-facing peer of DLENames.
func (e *Engine) DLEDisplay() []string {
	disp := e.dleDisplayMap()
	var out []string
	for _, slug := range e.DLENames() {
		if id, ok := disp[slug]; ok {
			out = append(out, id)
		} else {
			out = append(out, slug)
		}
	}
	sort.Strings(out)
	return out
}

// ResolveDLE maps a user-supplied DLE reference — a host:path identity or the raw
// internal slug — to the internal slug, or ("", false) if no catalog DLE matches.
func (e *Engine) ResolveDLE(arg string) (string, bool) {
	disp := e.dleDisplayMap()
	for _, slug := range e.DLENames() {
		if slug == arg || disp[slug] == arg {
			return slug, true
		}
	}
	return "", false
}

// ForceFull schedules a configured DLE for a full on its next run, the archiver-independent
// `nb reset`: it records a force-full directive the planner honors (a mandatory L0),
// rather than reaching into and deleting the archiver's incremental state. The forced full
// reseeds that state itself when it runs, and — with commit-bound promotion — the old
// chain stays intact until the new full actually commits. arg is a host:path identity or
// the internal slug; it returns the DLE's display identity. The DLE must be configured,
// since forcing a full only makes sense to re-dump it.
func (e *Engine) ForceFull(arg string) (string, error) {
	for _, d := range e.cfg.DLEs() {
		if d.Name() == arg || d.ID() == arg {
			if err := e.cat.SetForceFull(d.Name()); err != nil {
				return "", fmt.Errorf("force full %s: %w", d.ID(), err)
			}
			return d.ID(), nil
		}
	}
	return "", fmt.Errorf("no DLE %q in the configuration", arg)
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
// medium's protected slots (its own minimum_age and last-recovery-path floor) and
// asks its retention strategy which non-protected slots to reclaim to fit its
// capacity. Retention is per-medium, so each store is pruned against its own slots
// — pruning one medium never touches a copy on another. Any configured medium can
// be pruned (not only the landing one), so an offsite tier can be trimmed too.
func (e *Engine) Prune(mediumName string, now time.Time, apply bool, logf Logf) (eligible int, freed int64, err error) {
	return e.acct.Prune(mediumName, now, apply, logf)
}
