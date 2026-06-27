// Package engine is NBackup's orchestrator. It
// wires the planner, archiver, transfer pipeline, media store, catalog, and
// retention together to execute runs, restores, verification, and pruning. It is
// the only place that knows about all the abstractions at once; everything below
// it depends only on interfaces.
package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/retention"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"

	// Register the bundled media and archiver implementations.
	_ "github.com/Niloen/nbackup/internal/archiver/gnutar"
	_ "github.com/Niloen/nbackup/internal/media/cloud"
	_ "github.com/Niloen/nbackup/internal/media/disk"
	_ "github.com/Niloen/nbackup/internal/media/tape"
)

// Logf is an optional progress logger.
type Logf func(format string, args ...any)

func (l Logf) log(format string, args ...any) {
	if l != nil {
		l(format, args...)
	}
}

// Engine holds the wired-up components for one configuration. It owns the media
// volume; the catalog is a local cache the engine refreshes from the volume.
// Archivers are resolved per dumptype and cached.
type Engine struct {
	cfg         *config.Config
	mediumName  string       // name of the medium new dumps land on
	mediumDef   config.Media // its definition
	vol         media.Volume
	clerk       *clerk.Clerk // the archive data path (read+write composer); the engine implements its Deps
	profile     media.Profile
	landingCost media.Cost // landing medium's pricing (dollar peer of profile)
	minAge      time.Duration
	cat         *catalog.Catalog
	archivers   map[string]archiver.Archiver  // by cache key (dumptype or "@type")
	codec       string                        // compression codec for new archives
	fopts       compress.Options              // codec invocation options (level/threads/nice)
	dcopts      crypt.Options                 // decrypt key reference for restore (from the default encrypt block)
	op          librarian.Operator            // optional: handles manual tape swaps (nil = unattended)
	runSink     progress.Sink                 // optional: live run-progress sink (nil = status file only)
	limiters    map[string]*ratelimit.Limiter // per-medium bandwidth cap (nil entry = uncapped); shared so a medium's concurrent streams share one budget
	dec         *decoder                      // the read-side codec operation (restore/verify/list); shares the engine's resolution + decode opts
	enc         *encoder                      // the write-side codec operation (dump); shares the engine's dumptype-recipe resolution
	ver         *verifier                     // the verification operation (verify/drill checks); shares catalog + data path + decoder
	cop         *copier                       // the copy operation (PlanCopy/CopySlot); shares catalog + data path + write machinery
	rst         *restorer                     // the restore/recover operation; shares catalog + data path + decoder + config
}

// SetOperator attaches an operator so manual single-drive media can prompt for a
// reel swap mid-command. Without one, manual swaps degrade to an actionable error.
func (e *Engine) SetOperator(op librarian.Operator) { e.op = op }

// SetRunProgress attaches a live progress sink that receives run snapshots alongside
// the run-status file, so `nb dump` can paint progress to the terminal without an
// operator running `nb status`. Nil (the default) keeps the file-only behaviour.
func (e *Engine) SetRunProgress(sink progress.Sink) { e.runSink = sink }

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
	return librarian.New(vol, name, e.cat, e.op, e.cfg.AutoLabel), d, own, nil
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
		cfg:         cfg,
		mediumName:  name,
		mediumDef:   mediaDef,
		vol:         vol,
		profile:     profile,
		landingCost: costModel,
		minAge:      minAge,
		cat:         cat,
		archivers:   map[string]archiver.Archiver{},
		codec:       cfg.CompressScheme(),
		fopts:       fopts,
		dcopts:      dcopts,
		limiters:    limiters,
	}
	e.clerk = clerk.New(e, e, catalog.OpenMemberIndex(cfg.WorkdirPath()))
	e.dec = e.newDecoder()
	e.enc = e.newEncoder()
	e.ver = e.newVerifier()
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
func (e *Engine) Capacity() int64 { return e.profile.TotalBytes() }

// CapacityStatus reports whether current usage exceeds capacity and the percent
// used (0 when unbounded).
func (e *Engine) CapacityStatus(current int64) (over bool, pct float64) {
	c := e.profile.TotalBytes()
	if c <= 0 {
		return false, 0
	}
	return current > c, float64(current) / float64(c) * 100
}

// MediumAppendable reports whether a medium packs many runs per volume (the
// default) rather than one run per volume — so inventory can label a written
// non-appendable reel "used" instead of "append".
func (e *Engine) MediumAppendable(name string) bool {
	if m, ok := e.cfg.Media[name]; ok {
		return m.IsAppendable()
	}
	return true
}

// MediumInfo is a per-medium summary for catalog visibility (`nb medium`): what
// the medium is, how much it holds against its capacity, and (for labeled media)
// the volume currently associated with it in the catalog.
type MediumInfo struct {
	Name     string
	Type     string
	Slots    int
	Used     int64
	Capacity int64  // 0 = unbounded
	Volume   string // label name; "" for address-identified media (disk, s3)
	Epoch    int
}

// Media returns a summary of every configured medium, sorted by name.
func (e *Engine) Media() []MediumInfo {
	names := make([]string, 0, len(e.cfg.Media))
	for n := range e.cfg.Media {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]MediumInfo, 0, len(names))
	for _, n := range names {
		info, _ := e.Medium(n)
		out = append(out, info)
	}
	return out
}

// Medium returns the summary for one configured medium; ok is false if the name
// is unknown.
func (e *Engine) Medium(name string) (MediumInfo, bool) {
	d, ok := e.cfg.Media[name]
	if !ok {
		return MediumInfo{}, false
	}
	info := MediumInfo{
		Name:  name,
		Type:  d.Type,
		Slots: len(e.cat.SlotsOn(name)),
		Used:  e.cat.MediumBytes(name),
	}
	if prof, err := media.OpenProfile(d.Type, media.Options(d.ProfileOptions())); err == nil {
		info.Capacity = prof.TotalBytes()
	}
	// Summarize the medium's labeled volumes from the catalog (no medium type
	// special-casing): address-identified media (disk, s3) carry no label so the
	// pool is empty and Volume stays ""; a single labeled volume shows its name and
	// epoch; a pool of several (a tape library/station) shows the count, with the
	// per-volume detail in `nb medium <name>`.
	switch pool := e.volumesInPool(name); len(pool) {
	case 0:
		// nothing labeled (address-identified, or a still-blank changer)
	case 1:
		info.Volume, info.Epoch = pool[0].Label.Name, pool[0].Label.Epoch
	default:
		info.Volume = fmt.Sprintf("%d volume(s)", len(pool))
	}
	return info, true
}

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
// and the state_dir default injected. A remote host yields an SSH executor (so tar runs
// on the client) and a client-side state_dir; a local/unlisted host yields the local
// executor — byte-for-byte today's behavior.
func (e *Engine) archiverFor(dtName, host string) (archiver.Archiver, error) {
	dt := e.cfg.ResolveDumpType(dtName)
	def := e.cfg.ResolveArchiver(dt.Archiver)
	return e.openArchiver(dtName+"\x00"+host, def.Type, def.Options, host)
}

// openArchiver returns the cached archiver for key, or opens one of typeName for the host
// (injecting that host's executor + state_dir overrides) and caches it. It is the shared
// get-or-open the dump-side archiverFor and read-side restoreArchiver both use; they
// differ only in the cache key and whether a definition's options apply.
func (e *Engine) openArchiver(key, typeName string, options map[string]string, host string) (archiver.Archiver, error) {
	if d, ok := e.archivers[key]; ok {
		return d, nil
	}
	ex, overrides := e.executorFor(host)
	d, err := archiver.Open(typeName, e.archiverOptions(options, overrides), ex)
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
// an empty or unlisted host, or an SSH executor for a host configured in the hosts: map —
// plus the gnutar option overrides that host implies (a client-side state_dir and
// tar_path). This is the one place "ssh" enters the engine; the archiver never learns it.
func (e *Engine) executorFor(host string) (programs.Executor, map[string]string) {
	hc, ok := e.cfg.RemoteHost(host)
	if !ok {
		return programs.Local(), nil
	}
	ex := programs.SSH(programs.Params{
		User:         hc.User,
		Host:         host,
		Port:         hc.Port,
		IdentityFile: hc.IdentityFile,
		Options:      hc.Options,
	})
	// A remote host always gets a client-side state_dir — the configured one, or the
	// relative default under the backup user's home — so the server's workdir path (the
	// archiverOptions fallback) never leaks onto a client.
	stateDir := hc.StateDir
	if stateDir == "" {
		stateDir = config.DefaultClientStateDir
	}
	over := map[string]string{"state_dir": stateDir}
	if hc.TarPath != "" {
		over["tar_path"] = hc.TarPath
	}
	return ex, over
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
	ex, _ := e.executorFor(host)
	if err := ex.Command("true").Run(); err != nil {
		return fmt.Errorf("source host %q unreachable over SSH (%s): %w — run `nb check` to diagnose", host, sshTarget(host, ssh), err)
	}
	return nil
}

// archiverOptions copies an archiver definition's options, applies host overrides (a
// remote host's client-side state_dir / tar_path), and injects the state_dir default
// (the per-DLE/per-level incremental-state library, beneath the workdir) when neither
// set one. The location is the orchestrator's to default — an archiver cannot know the
// workdir — which is why it is injected here.
func (e *Engine) archiverOptions(options, overrides map[string]string) archiver.Options {
	opts := archiver.Options{}
	for k, v := range options {
		opts[k] = v
	}
	for k, v := range overrides {
		opts[k] = v
	}
	if _, ok := opts["state_dir"]; !ok {
		opts["state_dir"] = filepath.Join(e.cfg.WorkdirPath(), "snapshots")
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
			logf.log("WARNING: skipping medium %q: %v", name, err)
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
	lib, def, _, err := e.librarianFor(medium)
	if err != nil {
		return nil, err
	}
	partSize, err := e.partSizeFor(medium)
	if err != nil {
		return nil, err
	}
	appendable := def.IsAppendable()
	expect := e.expectedVolumeFor(medium, now).Label
	volName, epoch, err := lib.PrepareWrite(appendable, expect, now, librarian.Logf(logf))
	if err != nil {
		return nil, err
	}
	sink := lib.WriteSink(volName, epoch, appendable, partSize, now, librarian.Logf(logf))
	w := archiveio.NewWriter(sink, spec, e.limiters[medium])
	return &writeTarget{lib: lib, w: w, partSize: partSize}, nil
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
func (e *Engine) StoredBytes() int64 { return e.cat.MediumBytes(e.mediumName) }

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
			exp.UsedBytes += s.TotalBytes
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
	floor := retention.Compute(e.cat.SlotsOn(medium), minAge, now)
	for _, v := range pool {
		held := e.cat.SlotsOnLabel(v.Label.Name)
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
	return e.planWith(date, nil)
}

// PlanWithProgress is Plan with a live sink for the estimate phase, which can be
// slow: every DLE is sized by an archiver pass, so a long preview is otherwise
// silent. sink (nil to disable) receives a snapshot as each DLE's estimate starts
// and finishes.
func (e *Engine) PlanWithProgress(date time.Time, sink progress.Sink) *planner.Plan {
	return e.planWith(date, sink)
}

func (e *Engine) planWith(date time.Time, sink progress.Sink) *planner.Plan {
	dles := e.cfg.DLEs()
	return planner.Build(dles, e.cat.History(), e.estimates(dles, sink), e.plannerParams(date), date)
}

// ValidatePlan checks each DLE the way a real run would resolve it, so a preview
// (`nb plan` / `nb dump --dry-run`) surfaces problems the size estimates would
// otherwise swallow into a misleading ~0 B. It runs the same pre-flight a real run
// does — the compression codec and every dumptype's method and encryption scheme —
// returning a fatal error for an unrunnable config (an unknown codec/method/scheme,
// a missing required key reference, or a codec/gpg binary not on PATH), so a preview
// no longer gives a green light to a run that `nb dump` will reject. Source paths
// that are missing or unreadable right now are non-fatal warnings (they may be an
// unmounted volume the real run will mount).
func (e *Engine) ValidatePlan() (warnings []string, err error) {
	if err := compress.Check(e.codec, e.fopts); err != nil {
		return nil, err
	}
	checkedEnc := map[string]bool{}
	hostProbed := map[string]bool{}
	for _, d := range e.cfg.DLEs() {
		if err := e.preflightDumptype(d.DumpTypeName(), d.Host, false, checkedEnc); err != nil {
			return nil, err
		}
		// Only a local source can be stat'd here; a remote DLE's path lives on the
		// client. A remote host is probed over SSH (once per host) so an unreachable
		// client warns here rather than silently estimating ~0 B — the misleading
		// "healthy" plan `nb check` would otherwise be the only thing to catch.
		if _, remote := e.cfg.RemoteHost(d.Host); !remote {
			if _, err := os.Stat(d.Path); err != nil {
				warnings = append(warnings, fmt.Sprintf("DLE %s: source path %s is missing or unreadable (%v) — the real run will fail unless it becomes available", d.ID(), d.Path, err))
			}
		} else if !hostProbed[d.Host] {
			hostProbed[d.Host] = true
			if err := e.probeReachable(d.Host); err != nil {
				warnings = append(warnings, fmt.Sprintf("%v — its DLEs cannot be estimated until it is reachable (shown as ~0 B)", err))
			}
		}
	}
	return warnings, nil
}

// Simulate forecasts the next `days` daily runs from `start` without writing
// anything: it plans each day and advances a cloned history between them, so the
// level schedule — when each DLE's full next lands, how its incrementals climb — is
// projected forward. Estimates and the capacity ceiling are sampled once at `start`
// and held constant, so this is a schedule forecast, not a capacity timeline.
func (e *Engine) Simulate(start time.Time, days int) []*planner.Plan {
	dles := e.cfg.DLEs()
	return planner.Simulate(dles, e.cat.History(), e.estimates(dles, nil), e.plannerParams(start), start, days)
}

// plannerParams derives the planner's tuning inputs from config and the medium for
// a run date. Shared by Plan and Simulate so a single-day plan and the forward
// forecast use identical balancing rules.
func (e *Engine) plannerParams(date time.Time) planner.Params {
	return planner.Params{
		CycleDays:     e.cfg.CycleDays(),
		CapacityBytes: e.profile.TotalBytes(),
		RoomBytes:     e.capacityRoom(date),
		BumpPercent:   e.cfg.BumpPercent(),
	}
}

// estimates predicts, for each DLE, the size of a full and of the incremental at
// its current level and the next (the inputs the planner's bump decision needs),
// by asking the archiver. For gnutar this is a
// fast metadata-only tar pass; see gnutar.Estimate. Sizes are uncompressed — an
// upper bound on the compressed bytes finally stored.
// Estimates run in parallel, bounded by parallelism.workers:
// each DLE's estimate is an independent archiver pass, and on a host with many DLEs
// the serial sum dominates a preview. When sink is non-nil the work is tracked so a
// caller can paint live progress. Archivers are resolved serially first because
// archiverFor writes a shared cache the workers must only read.
func (e *Engine) estimates(dles []config.DLE, sink progress.Sink) map[string]planner.Estimate {
	hist := e.cat.History()
	out := make(map[string]planner.Estimate, len(dles))
	states := make([]*catalog.DLEState, len(dles))
	for i, d := range dles {
		_, _ = e.archiverFor(d.DumpTypeName(), d.Host) // warm the cache; errors resurface per-DLE below
		states[i] = hist.DLE(d.Name())                 // History.DLE memoizes; resolve serially before the workers read it
	}

	workers := e.cfg.Workers()
	var tr *progress.Tracker
	if sink != nil {
		rows := make([]progress.Plan, len(dles))
		for i, d := range dles {
			rows[i] = progress.Plan{Name: d.ID()}
		}
		tr = progress.NewTracker("estimate", workers, rows, time.Now, sink)
	}

	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, workers)
		mu  sync.Mutex
	)
	for i, d := range dles {
		wg.Add(1)
		sem <- struct{}{}
		go func(d config.DLE, st *catalog.DLEState) {
			defer wg.Done()
			defer func() { <-sem }()
			name := d.Name() // internal slug: archiver request + planner estimate key
			if tr != nil {
				tr.StartDLE(d.ID()) // progress display keys by host:path, matching the dump phase
			}
			est := e.estimateDLE(d, name, st)
			mu.Lock()
			out[name] = est
			mu.Unlock()
			if tr != nil {
				tr.FinishDLE(d.ID(), 0, est.Full, 0, nil)
			}
		}(d, states[i])
	}
	wg.Wait()
	if tr != nil {
		tr.SetPhase(progress.PhaseDone)
	}
	return out
}

func (e *Engine) estimateDLE(d config.DLE, name string, st *catalog.DLEState) planner.Estimate {
	arch, err := e.archiverFor(d.DumpTypeName(), d.Host)
	if err != nil || arch.Check() != nil {
		return planner.Estimate{} // no estimator available (e.g. tar missing)
	}
	excl := e.cfg.ResolveDumpType(d.DumpTypeName()).Exclude
	full, _ := arch.Estimate(archiver.BackupRequest{DLE: name, SourcePath: d.Path, Level: 0, BaseLevel: -1, Exclude: excl})
	if st.LastFullDate == "" {
		return planner.Estimate{Full: full} // never fulled: only a full is possible
	}

	// The DLE sits at level L — 1 right after a full, otherwise its last level. We
	// estimate that level and the next so the planner can judge whether climbing to
	// L+1 saves enough to be worth it (see planner.chooseIncrLevel). L+1 is only
	// estimable once an L dump exists to base it on; until then IncrNext stays 0.
	lvl := st.LastLevel()
	if lvl < 1 {
		lvl = 1
	}
	if lvl > planner.MaxLevel {
		lvl = planner.MaxLevel
	}
	est := planner.Estimate{Full: full}
	if arch.HasBase(name, lvl-1) {
		est.Incr, _ = arch.Estimate(archiver.BackupRequest{
			DLE: name, SourcePath: d.Path, Level: lvl, BaseLevel: lvl - 1, Exclude: excl,
		})
	}
	if lvl < planner.MaxLevel && arch.HasBase(name, lvl) {
		est.IncrNext, _ = arch.Estimate(archiver.BackupRequest{
			DLE: name, SourcePath: d.Path, Level: lvl + 1, BaseLevel: lvl, Exclude: excl,
		})
	}
	return est
}

// capacityRoom is the hard per-run write ceiling fed to the planner: the most a
// single run may write. It is the tighter of two independent bounds — the pool's
// free room (retention: capacity minus the protected set, the bytes pruning
// cannot reclaim) and the landing volume's remaining room (physical: a run fills
// the reel it appends to before spilling to the next). Either is unbounded (-1)
// on media that lack it — object stores have no reel, a bare drive has no bounded
// pool — and the result is unbounded only when both are.
func (e *Engine) capacityRoom(now time.Time) int64 {
	return minRoom(e.poolRoom(now), e.volumeRoom(now))
}

// poolRoom is the retention bound: capacity minus the bytes pruning cannot
// reclaim (the protected set). Negative = unbounded (no pool budget).
func (e *Engine) poolRoom(now time.Time) int64 {
	capacity := e.profile.TotalBytes()
	if capacity <= 0 {
		return -1
	}
	slots := e.cat.SlotsOn(e.mediumName)
	floor := retention.Compute(slots, e.minAge, now)
	var keptBytes int64
	for _, s := range slots {
		if floor.Keeps(s.ID) {
			keptBytes += s.TotalBytes
		}
	}
	if room := capacity - keptBytes; room > 0 {
		return room
	}
	return 0
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

// Run executes the plan for a date, producing one sealed slot.
func (e *Engine) Run(date time.Time, logf Logf) (*record.Slot, error) {
	// Guard the restore-order invariant: restore replays a DLE's slots in date order,
	// but the archiver's incremental snapshots advance in dump (wall-clock) order. A
	// run dated earlier than a slot already sealed would splice an out-of-order
	// archive into the chain whose snapshot has already moved past it — silently
	// dropping files at restore. Reject it (a same-day rerun, equal date, is fine and
	// takes the next .N). Backdating before today is already caught at the CLI.
	if latest, ok := e.latestSlotDate(); ok && record.DateString(date) < latest {
		return nil, fmt.Errorf("cannot dump for %s: slot(s) dated %s already exist; an earlier-dated run would corrupt the incremental restore order (snapshots have advanced past it) — dump on or after %s", record.DateString(date), latest, latest)
	}
	plan := e.Plan(date)
	for _, w := range plan.Warnings {
		logf.log("WARNING: %s", w)
	}

	// Pre-flight before creating a slot: the codec binary and every archiver.
	// Resolving every archiver here also populates the archiver cache, so the parallel
	// workers below only read it (no concurrent writes).
	if err := compress.Check(e.codec, e.fopts); err != nil {
		return nil, err
	}
	checkedEnc := map[string]bool{}
	checkedHost := map[string]bool{}
	for _, item := range plan.Items {
		if !checkedHost[item.DLE.Host] {
			if err := e.probeReachable(item.DLE.Host); err != nil {
				return nil, err
			}
			checkedHost[item.DLE.Host] = true
		}
		if err := e.preflightDumptype(item.DLE.DumpTypeName(), item.DLE.Host, true, checkedEnc); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	slotID, seq, err := e.allocSlotID(date)
	if err != nil {
		return nil, err
	}
	spec := archiveio.SlotSpec{ID: slotID, Date: record.DateString(date), Sequence: seq, Generator: "nbdump", CreatedAt: now}
	wt, err := e.prepareWriter(e.mediumName, spec, now, logf)
	if err != nil {
		return nil, err
	}
	w := wt.w

	// A spanning-capable landing (a finite tape changer, or part_size set) writes one
	// drive serially: it cannot interleave two archives' parts and roll mid-write, so
	// workers are clamped to 1. Disk (unbounded) keeps the configured parallelism.
	workers := e.cfg.Workers()
	if workers > 1 && wt.lib.CanSpan(wt.partSize) {
		logf.log("medium %q can span volumes; running 1 worker (a single drive writes serially)", e.mediumName)
		workers = 1
	}

	// Track live progress to the run-status file so `nb status` can watch a
	// detached run. Progress reporting never blocks or fails the backup. A live
	// sink (when attached) paints the same snapshots to the terminal; in that mode
	// the per-DLE log lines are suppressed so they don't scribble over the in-place
	// region (warnings above were already printed before the region opened).
	sink := progress.Sink(progress.NewFileSink(e.cfg.WorkdirPath(), time.Now))
	runLogf := logf
	if e.runSink != nil {
		sink = progress.MultiSink(sink, e.runSink)
		runLogf = nil
	}
	tr := progress.NewTracker(slotID, workers, planProgress(plan.Items), time.Now, sink)

	session := e.clerk.OpenSlot(w, e.mediumName)
	if err := e.runWorkers(plan.Items, workers, session, tr, runLogf); err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, err
	}

	tr.SetPhase(progress.PhaseSealing)
	sealed, err := session.Finish(time.Now().UTC())
	if err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, err
	}
	tr.SetPhase(progress.PhaseDone)
	return sealed, nil
}

// planProgress projects planner items onto the progress package's seed type,
// keeping progress unaware of the planner.
func planProgress(items []planner.Item) []progress.Plan {
	out := make([]progress.Plan, len(items))
	for i, it := range items {
		out[i] = progress.Plan{Name: it.DLE.ID(), Level: it.Level, EstBytes: it.EstBytes}
	}
	return out
}

// runWorkers backs up every planned item into the slot. With parallelism.workers
// > 1 it runs that many workers concurrently, bounded by a
// semaphore; the first error stops scheduling further items and is returned. Each
// worker writes a distinct object into the slot, which the medium must allow
// concurrently (disk does) and the slot Writer serializes its bookkeeping.
func (e *Engine) runWorkers(items []planner.Item, workers int, session *clerk.Session, tr *progress.Tracker, logf Logf) error {
	if workers <= 1 || len(items) <= 1 {
		for _, item := range items {
			if err := e.backupItem(session, item, tr, logf); err != nil {
				return err
			}
		}
		return nil
	}

	threads := e.fopts.Threads
	if threads < 1 {
		threads = 1
	}
	if cores := runtime.GOMAXPROCS(0); workers*threads > cores {
		logf.log("WARNING: %d workers x %d compressor thread(s) = %d exceeds %d cores; CPU may be oversubscribed",
			workers, threads, workers*threads, cores)
	}

	var (
		wg       sync.WaitGroup
		sem      = make(chan struct{}, workers)
		mu       sync.Mutex
		firstErr error
	)
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	for _, item := range items {
		if failed() {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it planner.Item) {
			defer wg.Done()
			defer func() { <-sem }()
			if failed() {
				return
			}
			if err := e.backupItem(session, it, tr, logf); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(item)
	}
	wg.Wait()
	return firstErr
}

// allocSlotID picks the slot ID for a run on the given date: the first run of
// the day is "slot-DATE", later runs get the next free ".N". A leftover unsealed
// slot from a failed attempt is reclaimed. This consults the volume (the write
// path may touch media) so it is robust to a stale cache.
// latestSlotDate returns the most recent slot date (YYYY-MM-DD) across the whole
// catalog, or ("", false) when no slots exist. Dates are lexically comparable.
func (e *Engine) latestSlotDate() (string, bool) {
	latest := ""
	for _, s := range e.cat.Slots() {
		if s.Date > latest {
			latest = s.Date
		}
	}
	return latest, latest != ""
}

// PlannedSlotID returns the slot id a real dump on date would seal next: the next
// free same-day sequence given the sealed slots already in the catalog. It is the
// preview peer of allocSlotID (which additionally reclaims an unsealed orphan on the
// loaded volume) and exists so `nb dump --dry-run` names the slot a real run would
// produce — not always `.1` — when the date is already sealed.
func (e *Engine) PlannedSlotID(date time.Time) string {
	have := map[string]bool{}
	for _, s := range e.cat.Slots() {
		have[s.ID] = true
	}
	ds := record.DateString(date)
	for seq := 1; ; seq++ {
		id := record.IDFromParts(ds, seq)
		if !have[id] {
			return id
		}
	}
}

func (e *Engine) allocSlotID(date time.Time) (id string, seq int, err error) {
	files, err := e.vol.Files()
	if err != nil {
		// A changer with nothing loaded yet (a fresh library before its first mount,
		// e.g. auto_label on a blank pool) has no files to scan for orphans. The
		// catalog still seeds every known slot id pool-globally below, so treat an
		// empty drive as "no extra files" rather than a hard failure — letting a
		// first dump proceed to PrepareWrite, which mounts and auto-labels a bay.
		if !errors.Is(err, media.ErrNoVolume) {
			return "", 0, err
		}
		files = nil
	}
	present := map[string]bool{} // slot id -> exists (catalog or loaded volume)
	sealed := map[string]bool{}  // slot id -> sealed (immutable; never reuse the id)
	// Seed from the catalog, which indexes every sealed slot across the whole pool.
	// A slot id is pool-global, so a same-day rerun must take the next free .N even
	// when an earlier run sealed onto a different volume (or medium) than the one now
	// loaded — scanning only the loaded volume's Files() would miss it and reuse the
	// id, shadowing that earlier run in the catalog. Catalog slots are sealed by
	// construction (Record runs only after Seal).
	for _, s := range e.cat.Slots() {
		present[s.ID] = true
		sealed[s.ID] = true
	}
	// The loaded volume may also carry an orphan from a failed attempt that the catalog
	// never recorded; note it so its id can be reclaimed below. A slot with any committed
	// archive (a commit footer) is a real recovery point — its id is never reused; one with
	// only uncommitted parts is a reclaimable orphan.
	for _, f := range files {
		present[f.Header.Slot] = true
		if f.Header.Kind == record.KindCommit {
			sealed[f.Header.Slot] = true
		}
	}
	day := record.DateString(date)
	for seq = 1; ; seq++ {
		id = record.IDFromParts(day, seq)
		if !present[id] {
			return id, seq, nil
		}
		if sealed[id] {
			continue // a sealed slot occupies this id; try the next sequence
		}
		// Unsealed leftover from a failed attempt: reclaim its files. A medium that
		// cannot remove individual files (tape — space is reclaimed by relabeling the
		// whole volume) leaves the orphan in place; a scan ignores it (it has no seal),
		// and it is reclaimed on the next relabel. Take the next id rather than failing.
		removed := true
		for _, f := range files {
			if f.Header.Slot != id {
				continue
			}
			if err := e.vol.RemoveFile(f.Pos); err != nil {
				if errors.Is(err, media.ErrNoFileRemoval) {
					removed = false
					break
				}
				return "", 0, err
			}
		}
		if !removed {
			continue
		}
		return id, seq, nil
	}
}

// backupItem archives a single DLE into the open slot session. The engine owns
// orchestration — the run tracker lifecycle, resolving the archiver and describing the
// backup (the request + incremental base requirement) — and the session moves and records
// the bytes; the engine never sees the storage record or its parts.
func (e *Engine) backupItem(session *clerk.Session, item planner.Item, tr *progress.Tracker, logf Logf) (err error) {
	// The progress tracker keys and displays DLEs by their host:path identity; the
	// seal and filenames keep the internal slug.
	pname := item.DLE.ID()
	tr.StartDLE(pname)
	var sum clerk.Summary
	defer func() {
		if err != nil {
			tr.FinishDLE(pname, 0, 0, 0, err)
		} else {
			tr.FinishDLE(pname, sum.FileCount, sum.Uncompressed, sum.Compressed, nil)
		}
	}()

	spec, err := e.backupSpec(item)
	if err != nil {
		return err
	}

	logf.log("archiving %s (L%d)", item.DLE.ID(), item.Level)
	sum, err = e.enc.dumpArchive(session, spec, func(uncompressed, compressed int64) { tr.AddBytes(pname, uncompressed, compressed) })
	if err != nil {
		return fmt.Errorf("archive %s: %w", item.Name, err)
	}

	sizeLabel := "compressed"
	if sum.Codec == "none" {
		sizeLabel = "stored" // no compressor in the pipe; "compressed" would be a lie
	}
	if sum.FileCount == 0 {
		// An incremental with nothing changed still writes tar's structural overhead
		// (archive header/footer + directory census); say so rather than the puzzling
		// "0 file(s), 10.24 kB stored".
		logf.log("  no changed files (%s of tar metadata)", sizeutil.FormatBytes(sum.Compressed))
	} else {
		logf.log("  %d file(s), %s %s", sum.FileCount, sizeutil.FormatBytes(sum.Compressed), sizeLabel)
	}
	return nil
}

// backupSpec describes the backup of one planned item: it resolves the archiver and builds
// the request (with the dumptype's excludes), and for an incremental requires the base
// incremental state to be present. It is pure intent — the schemes, transform placement, and
// storage record are the session's to derive.
func (e *Engine) backupSpec(item planner.Item) (BackupSpec, error) {
	ar, err := e.archiverFor(item.DLE.DumpTypeName(), item.DLE.Host)
	if err != nil {
		return BackupSpec{}, err
	}
	req := archiver.BackupRequest{
		DLE:        item.Name,
		SourcePath: item.DLE.Path,
		Level:      item.Level,
		BaseLevel:  -1,
		Exclude:    e.cfg.ResolveDumpType(item.DLE.DumpTypeName()).Exclude,
	}
	if item.Level >= 1 {
		req.BaseLevel = item.BaseLevel
		if !ar.HasBase(item.Name, item.BaseLevel) {
			return BackupSpec{}, fmt.Errorf("DLE %s: incremental L%d needs the L%d incremental state but it is missing",
				item.Name, item.Level, item.BaseLevel)
		}
	}
	return BackupSpec{
		Archiver: ar,
		Request:  req,
		Host:     item.DLE.Host,
		BaseSlot: item.BaseSlot,
		DumpType: item.DLE.DumpTypeName(),
	}, nil
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
// Record records a run's placement — together they are the clerk's Map role (the engine keeps
// the catalog store + the directory/retention slices).
func (e *Engine) PlacementsFor(slotID string) []catalog.Placement     { return e.placementsFor(slotID) }
func (e *Engine) Record(slot *record.Slot, p catalog.Placement) error { return e.cat.Record(slot, p) }

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
	ex, _ := e.executorFor(host)
	return ex
}

// RestoreAsOf reconstructs a whole DLE as of a date into destDir; see restorer.
func (e *Engine) RestoreAsOf(dle, asOf, destDir string, force bool, logf Logf) error {
	return e.rst.RestoreAsOf(dle, asOf, destDir, force, logf)
}

// RestoreAsOfTo is RestoreAsOf onto a remote client over SSH; see restorer.
func (e *Engine) RestoreAsOfTo(dle, asOf, destHost, destPath string, logf Logf) error {
	return e.rst.RestoreAsOfTo(dle, asOf, destHost, destPath, logf)
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

// profileFor returns the capacity/reclamation profile for a named medium: the
// landing medium's cached profile, or one opened on demand for any other medium.
func (e *Engine) profileFor(name string) (media.Profile, error) {
	if name == e.mediumName {
		return e.profile, nil
	}
	d, ok := e.cfg.Media[name]
	if !ok {
		return nil, fmt.Errorf("unknown medium %q", name)
	}
	return media.OpenProfile(d.Type, media.Options(d.ProfileOptions()))
}

// Prune reconciles a named medium to its own retention model: it computes that
// medium's protected slots (its own minimum_age and last-recovery-path floor) and
// asks its retention strategy which non-protected slots to reclaim to fit its
// capacity. Retention is per-medium, so each store is pruned against its own slots
// — pruning one medium never touches a copy on another. Any configured medium can
// be pruned (not only the landing one), so an offsite tier can be trimmed too.
func (e *Engine) Prune(mediumName string, now time.Time, apply bool, logf Logf) (eligible int, freed int64, err error) {
	def, ok := e.cfg.Media[mediumName]
	if !ok {
		return 0, 0, fmt.Errorf("unknown medium %q", mediumName)
	}
	profile, err := e.profileFor(mediumName)
	if err != nil {
		return 0, 0, err
	}
	minAge := e.cfg.MinAgeFor(def)
	slots := e.cat.SlotsOn(mediumName)
	floor := retention.Compute(slots, minAge, now)

	// Reclamation is per archive (slot+DLE): a medium's Reclaim walks the oldest
	// non-protected archives, so an old slot can lose one DLE's image while keeping
	// another the chain still needs.
	type archiveRef struct{ slot, dle string }
	reclaim := map[archiveRef]media.Reclamation{}
	for _, r := range profile.Reclaim(slots, floor, now) {
		reclaim[archiveRef{r.SlotID, r.DLE}] = r
	}

	for _, s := range slots {
		for _, a := range s.Archives {
			if _, ok := reclaim[archiveRef{s.ID, a.DLE}]; ok {
				continue // reported below
			}
			if reason, ok := floor.ReasonArchive(s.ID, a.DLE); ok {
				logf.log("keep   %s %s  (%s)", s.ID, e.DisplayDLE(a.DLE), reason)
			} else {
				logf.log("keep   %s %s  (fits capacity)", s.ID, e.DisplayDLE(a.DLE))
			}
		}
	}

	// Open the medium's volume only when there is something to actually delete.
	var vol media.Volume
	if apply && len(reclaim) > 0 {
		if vol, _, _, err = e.mediumVolume(mediumName); err != nil {
			return eligible, freed, err
		}
	}
	for _, s := range slots {
		for _, a := range s.Archives {
			r, ok := reclaim[archiveRef{s.ID, a.DLE}]
			if !ok {
				continue
			}
			eligible++
			if apply {
				// Reclaim this archive's copy on this medium only — its files, one
				// position at a time; the slot (and the archive's copies elsewhere)
				// survives in the catalog.
				for _, pos := range archivePositions(e.cat.Placements(s.ID), mediumName, a.DLE) {
					if err := vol.RemoveFile(pos); err != nil {
						return eligible, freed, fmt.Errorf("delete %s %s: %w", s.ID, a.DLE, err)
					}
				}
				if _, _, err := e.cat.RemoveArchive(s.ID, mediumName, a.DLE); err != nil {
					return eligible, freed, fmt.Errorf("update catalog cache: %w", err)
				}
				freed += r.Bytes
				logf.log("DELETE %s %s  (%s freed, %s)", s.ID, e.DisplayDLE(a.DLE), sizeutil.FormatBytes(r.Bytes), r.Note)
			} else {
				logf.log("would delete %s %s  (%s, %s)", s.ID, e.DisplayDLE(a.DLE), sizeutil.FormatBytes(r.Bytes), r.Note)
			}
		}
	}
	return eligible, freed, nil
}

// archivePositions gathers the volume file positions of one archive (a DLE's image)
// in the copy of a slot on medium, in safe removal order: commit footer first, then
// the member index, then the parts.
//
// The order is crash-safety-critical and mirrors the write order in reverse. An
// archive is made durable by its commit footer, written LAST (after its parts and
// index); the footer's presence is what proves the whole archive landed, and a
// catalog rebuild assembles only archives that have a footer (assemble iterates the
// commits — parts without one are orphans it ignores). So removing the footer FIRST
// "un-commits" the archive: a crash mid-prune then leaves parts/index as orphans with
// no footer, which a rebuild skips. Removing parts first would leave a footer whose
// parts are gone — which a rebuild would resurrect into the catalog as a committed-
// but-unreadable archive (the exact "we think it's committed but it's only partly
// there" hazard). Removal is one os.Remove per file, so the ordering holds at the same
// level the write path relies on (no fsync either side).
func archivePositions(ps []catalog.Placement, medium, dle string) []int {
	for _, p := range ps {
		if p.Medium != medium {
			continue
		}
		for _, a := range p.Archives {
			if a.DLE != dle {
				continue
			}
			pos := make([]int, 0, len(a.Parts)+2)
			pos = append(pos, a.Commit.Pos) // the marker: un-commit first
			if a.Index != (record.FilePos{}) {
				pos = append(pos, a.Index.Pos)
			}
			for _, pt := range a.Parts {
				pos = append(pos, pt.Pos)
			}
			return pos
		}
	}
	return nil
}
