// Package engine is NBackup's orchestrator, analogous to Amanda's driver. It
// wires the planner, archiver, transfer pipeline, media store, catalog, and
// retention together to execute runs, restores, verification, and pruning. It is
// the only place that knows about all the abstractions at once; everything below
// it depends only on interfaces.
package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/crypt"
	"github.com/Niloen/nbackup/internal/filter"
	"github.com/Niloen/nbackup/internal/format"
	"github.com/Niloen/nbackup/internal/hostexec"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/restore"
	"github.com/Niloen/nbackup/internal/retention"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/slotio"
	"github.com/Niloen/nbackup/internal/xfer"

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
	reader      *slotio.Reader
	profile     media.Profile
	landingCost media.Cost // landing medium's pricing (dollar peer of profile)
	minAge      time.Duration
	cat         *catalog.Catalog
	archivers   map[string]archiver.Archiver // by cache key (dumptype or "@type")
	codec       string                       // compression codec for new archives
	fopts       filter.Options               // codec invocation options (level/threads/nice)
	dcopts      crypt.Options                // decrypt key reference for restore (from the default encrypt block)
	op          librarian.Operator           // optional: handles manual tape swaps (nil = unattended)
	limiters    map[string]*xfer.Limiter     // per-medium bandwidth cap (nil entry = uncapped); shared so a medium's concurrent streams share one budget
}

// SetOperator attaches an operator so manual single-drive media can prompt for a
// reel swap mid-command. Without one, manual swaps degrade to an actionable error.
func (e *Engine) SetOperator(op librarian.Operator) { e.op = op }

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
	limiters := map[string]*xfer.Limiter{}
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
		limiters[mname] = xfer.NewLimiter(bps)
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
	fopts := filter.Options{
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
	return &Engine{
		cfg:         cfg,
		mediumName:  name,
		mediumDef:   mediaDef,
		vol:         vol,
		reader:      slotio.NewReader(fopts, dcopts),
		profile:     profile,
		landingCost: costModel,
		minAge:      minAge,
		cat:         cat,
		archivers:   map[string]archiver.Archiver{},
		codec:       cfg.CompressCodec(),
		fopts:       fopts,
		dcopts:      dcopts,
		limiters:    limiters,
	}, nil
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
func (e *Engine) executorFor(host string) (hostexec.Executor, map[string]string) {
	hc, ok := e.cfg.RemoteHost(host)
	if !ok {
		return hostexec.Local(), nil
	}
	ex := hostexec.SSH(hostexec.Params{
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
// workdir — which is why it is injected here (Amanda's compile-time GNUTAR-LISTDIR).
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
// is mounted and label-verified, the slotio writer streaming the slot onto it, and the
// medium's part_size (so the caller can decide parallelism via lib.CanSpan).
type writeTarget struct {
	lib      *librarian.Librarian
	w        *slotio.Writer
	partSize int64
}

// prepareWriter resolves a medium, enforces the label protocol on its loaded volume
// (prompting a swap on a manual single drive), and builds a slotio writer that
// authors the slot described by spec onto it. It is the one place the PrepareWrite
// -> WriteSink -> NewWriter contract lives, shared by a dump (Run) and a copy/sync
// (CopySlot).
func (e *Engine) prepareWriter(medium string, spec slotio.SlotSpec, now time.Time, logf Logf) (*writeTarget, error) {
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
	w, err := slotio.NewWriter(sink, spec, e.codec, e.fopts, e.limiters[medium])
	if err != nil {
		return nil, err
	}
	return &writeTarget{lib: lib, w: w, partSize: partSize}, nil
}

// CopyPlan is the resolved, validated outcome of a would-be copy, without writing:
// the source/target the rules picked and whether the slot is already on the target.
type CopyPlan struct {
	SlotID          string
	From            string   // resolved source medium (landing when --from is unset)
	To              string   // target medium
	Archives        int      // archives in the slot
	Bytes           int64    // the slot's total bytes
	AlreadyOnTarget bool     // a copy already exists on To (skipped unless force)
	TargetLabels    []string // the tape labels the existing target copy spans (empty for address-identified media)
}

// PlanCopy resolves and validates a copy the way CopySlot would, without writing —
// the single source of the copy-eligibility rules, shared by CopySlot and the
// `nb copy` dry-run so the two never drift. It errors on the same unrunnable cases
// (unknown slot, unknown target, source == target) and reports whether the slot is
// already on the target (force plans the re-copy anyway).
func (e *Engine) PlanCopy(slotID, fromMedia, targetMedia string, force bool) (CopyPlan, error) {
	s, err := e.cat.ReadSlot(slotID)
	if err != nil {
		return CopyPlan{}, err
	}
	if fromMedia == "" {
		fromMedia = e.mediumName
	}
	if _, ok := e.cfg.Media[targetMedia]; !ok {
		return CopyPlan{}, fmt.Errorf("unknown medium %q", targetMedia)
	}
	if fromMedia == targetMedia {
		return CopyPlan{}, fmt.Errorf("copy source and target are the same medium %q", targetMedia)
	}
	plan := CopyPlan{SlotID: slotID, From: fromMedia, To: targetMedia, Archives: len(s.Archives), Bytes: s.TotalBytes}
	if !force {
		if p, ok := e.placementOn(slotID, targetMedia); ok {
			plan.AlreadyOnTarget = true
			plan.TargetLabels = p.Labels()
		}
	}
	return plan, nil
}

// CopySlot streams a sealed slot from one configured medium to another, then
// records the new copy in the catalog (a second placement). The source defaults to
// the landing medium when fromMedia is ""; any other medium holding the slot is
// allowed (e.g. un-vaulting tape -> disk). Reading the source mounts the volume
// that holds the slot (on a changer); the write to the target runs the same label
// verification as a dump.
func (e *Engine) CopySlot(slotID, fromMedia, targetMedia string, force bool, logf Logf) error {
	plan, err := e.PlanCopy(slotID, fromMedia, targetMedia, force)
	if err != nil {
		return err
	}
	if plan.AlreadyOnTarget {
		// Idempotency: a slot already recorded on the target is not re-copied. On
		// append-only media a second copy would orphan the first (unreferenced files,
		// reclaimable only by relabel); --force overrides for a deliberate re-copy.
		where := ""
		if len(plan.TargetLabels) > 0 {
			where = fmt.Sprintf(" (volume(s) %v)", plan.TargetLabels)
		}
		return fmt.Errorf("slot %s is already on medium %q%s; use --force to copy again", slotID, targetMedia, where)
	}
	fromMedia = plan.From
	s, err := e.cat.ReadSlot(slotID)
	if err != nil {
		return err
	}
	srcLib, srcPlacement, err := e.copySource(slotID, fromMedia)
	if err != nil {
		return err
	}
	// Re-author the slot onto the target: each archive's already-compressed payload
	// (the source copy's parts concatenated) is re-split into parts sized to the
	// target's volumes, rolling onto a fresh volume mid-archive when one fills. The
	// bytes are unchanged, so checksums and members carry over; only the part layout
	// is new. The slot's logical content (the source seal) is what the catalog keeps.
	now := time.Now().UTC()
	// Re-author under the source's identity (CreatedAt and all) so the copy's seal
	// record names the same logical slot; the catalog still keeps the source seal.
	spec := slotio.SlotSpec{ID: s.ID, Date: s.Date, Sequence: s.Sequence, Generator: s.Generator, CreatedAt: s.CreatedAt}
	wt, err := e.prepareWriter(targetMedia, spec, now, logf)
	if err != nil {
		return err
	}
	w := wt.w
	logf.log("copying %s from %q to %q", slotID, fromMedia, targetMedia)
	srcOpen := e.partOpener(srcLib, fromMedia)
	for _, a := range s.Archives {
		parts, ok := srcPlacement.Parts(a.DLE, a.Level)
		if !ok {
			return fmt.Errorf("source copy of %s on %q is missing %s L%d", slotID, fromMedia, a.DLE, a.Level)
		}
		raw, err := e.reader.OpenRawParts(parts, slotio.Expect{Slot: slotID, DLE: a.DLE, Level: a.Level}, srcOpen)
		if err != nil {
			return fmt.Errorf("copy %s L%d to %q: %w", a.DLE, a.Level, targetMedia, err)
		}
		_, werr := w.CopyArchive(a, raw)
		raw.Close()
		if werr != nil {
			return fmt.Errorf("copy %s L%d to %q: %w", a.DLE, a.Level, targetMedia, werr)
		}
	}
	if _, err := w.Seal(now); err != nil {
		return fmt.Errorf("seal copy on %q: %w", targetMedia, err)
	}
	if err := e.cat.Record(s, placementFrom(targetMedia, w)); err != nil {
		return fmt.Errorf("record copy in catalog: %w", err)
	}
	logf.log("copied %s (%d archive(s)) to %q", slotID, len(s.Archives), targetMedia)
	return nil
}

// copySource resolves the placement that holds a slot on the source medium and a
// librarian over that medium, for the read side of a copy. It errors if the slot has
// no copy on fromMedia (the catalog knows of none to read).
func (e *Engine) copySource(slotID, fromMedia string) (*librarian.Librarian, catalog.Placement, error) {
	src, ok := e.placementOn(slotID, fromMedia)
	if !ok {
		return nil, catalog.Placement{}, fmt.Errorf("slot %s has no copy on source medium %q", slotID, fromMedia)
	}
	lib, _, _, err := e.librarianFor(fromMedia)
	if err != nil {
		return nil, catalog.Placement{}, err
	}
	return lib, src, nil
}

// partOpener builds a slotio.PartOpener over a librarian: it mounts the volume each
// part lives on (and verifies its identity), then opens the part's file. Reading a
// spanned archive calls it once per part, in order — a single drive holds one tape.
// The part stream is paced to the source medium's bandwidth cap (the read peer of
// the write throttle), so a restore/un-vault/drill download honors the same uplink
// budget; an uncapped medium leaves the stream untouched.
func (e *Engine) partOpener(lib *librarian.Librarian, medium string) slotio.PartOpener {
	lim := e.limiters[medium]
	return func(p format.FilePos) (format.Header, io.ReadCloser, error) {
		h, rc, err := lib.ReadFileAt(p.Label, p.Epoch, p.Pos)
		if err != nil {
			return h, rc, err
		}
		return h, lim.ReadCloser(rc), nil
	}
}

// placementFrom builds a catalog placement from a sealed writer's recorded part
// positions and seal location. The writer emits the same format.FilePos/ArchivePos the
// catalog persists, so a placement is the positions verbatim — no field conversion.
func placementFrom(medium string, w *slotio.Writer) catalog.Placement {
	return catalog.Placement{Medium: medium, Archives: w.Positions(), Seal: w.SealPosition()}
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
	if n < 2*format.HeaderBlock {
		return 0, fmt.Errorf("medium %q part_size %s is too small; use at least %s", medium, sizeutil.FormatBytes(n), sizeutil.FormatBytes(2*format.HeaderBlock))
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
// write to — NBackup's analogue of Amanda's "amdump will expect tape X". It is
// derived from the catalog (the tapelist) and the retention policy, never from a
// physical scan: for a one-run-per-tape (non-appendable) medium it names the
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
// catalog's volume registry (the tapelist) ordered oldest-written-first. A
// non-appendable run reuses the oldest volume whose every run is unprotected (the
// retention safety floor: past minimum age, with a newer recovery path), matching
// Amanda's taper picking the oldest reusable tape; an appendable run extends the
// most recently written volume in the pool.
func (e *Engine) expectedVolumeFor(medium string, now time.Time) VolumeExpectation {
	def := e.cfg.Media[medium]
	exp := VolumeExpectation{Medium: medium, Appendable: def.IsAppendable()}

	// volumesInPool returns the same pool sorted by name; this expectation wants
	// oldest-written-first, so copy and re-sort rather than duplicate the filter.
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
	dles := e.cfg.DLEs()
	return planner.Build(dles, e.cat.History(), e.estimates(dles), e.plannerParams(date), date)
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
	if err := filter.Check(e.codec, e.fopts); err != nil {
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
	return planner.Simulate(dles, e.cat.History(), e.estimates(dles), e.plannerParams(start), start, days)
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
// by asking the archiver (Amanda's "client" estimate). For gnutar this is a
// fast metadata-only tar pass; see gnutar.Estimate. Sizes are uncompressed — an
// upper bound on the compressed bytes finally stored.
func (e *Engine) estimates(dles []config.DLE) map[string]planner.Estimate {
	hist := e.cat.History()
	out := make(map[string]planner.Estimate, len(dles))
	for _, d := range dles {
		name := d.Name()
		st := hist.DLE(name)
		out[name] = e.estimateDLE(d, name, st)
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
func (e *Engine) Run(date time.Time, logf Logf) (*format.Slot, error) {
	// Guard the restore-order invariant: restore replays a DLE's slots in date order,
	// but the archiver's incremental snapshots advance in dump (wall-clock) order. A
	// run dated earlier than a slot already sealed would splice an out-of-order
	// archive into the chain whose snapshot has already moved past it — silently
	// dropping files at restore. Reject it (a same-day rerun, equal date, is fine and
	// takes the next .N). Backdating before today is already caught at the CLI.
	if latest, ok := e.latestSlotDate(); ok && format.DateString(date) < latest {
		return nil, fmt.Errorf("cannot dump for %s: slot(s) dated %s already exist; an earlier-dated run would corrupt the incremental restore order (snapshots have advanced past it) — dump on or after %s", format.DateString(date), latest, latest)
	}
	plan := e.Plan(date)
	for _, w := range plan.Warnings {
		logf.log("WARNING: %s", w)
	}

	// Pre-flight before creating a slot: the codec binary and every archiver.
	// Resolving every archiver here also populates the archiver cache, so the parallel
	// workers below only read it (no concurrent writes).
	if err := filter.Check(e.codec, e.fopts); err != nil {
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
	spec := slotio.SlotSpec{ID: slotID, Date: format.DateString(date), Sequence: seq, Generator: "nbdump", CreatedAt: now}
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
	// detached run. Progress reporting never blocks or fails the backup.
	tr := progress.NewTracker(slotID, workers, planProgress(plan.Items), time.Now,
		progress.NewFileSink(e.cfg.WorkdirPath(), time.Now))

	if err := e.runWorkers(plan.Items, workers, w, tr, logf); err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, err
	}

	tr.SetPhase(progress.PhaseSealing)
	logf.log("verifying archive checksum(s)")
	sealed, err := w.Seal(time.Now().UTC())
	if err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, err
	}
	if err := e.cat.Record(sealed, placementFrom(e.mediumName, w)); err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, fmt.Errorf("update catalog cache: %w", err)
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
// > 1 it runs that many workers concurrently (Amanda's inparallel), bounded by a
// semaphore; the first error stops scheduling further items and is returned. Each
// worker writes a distinct object into the slot, which the medium must allow
// concurrently (disk does) and the slot Writer serializes its bookkeeping.
func (e *Engine) runWorkers(items []planner.Item, workers int, w *slotio.Writer, tr *progress.Tracker, logf Logf) error {
	if workers <= 1 || len(items) <= 1 {
		for _, item := range items {
			if err := e.backupItem(w, item, tr, logf); err != nil {
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
			if err := e.backupItem(w, it, tr, logf); err != nil {
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
	// The loaded volume may also carry an unsealed orphan from a failed attempt that
	// the catalog never recorded; note it so its id can be reclaimed below.
	for _, f := range files {
		present[f.Header.Slot] = true
		if f.Header.Kind == format.KindSeal {
			sealed[f.Header.Slot] = true
		}
	}
	day := format.DateString(date)
	for seq = 1; ; seq++ {
		id = format.IDFromParts(day, seq)
		if !present[id] {
			return id, seq, nil
		}
		if sealed[id] {
			continue // a sealed slot occupies this id; try the next sequence
		}
		// Unsealed leftover from a failed attempt: reclaim it. A medium that cannot
		// remove a single slot (tape — space is reclaimed by relabeling the whole
		// volume) leaves the orphan in place; a scan ignores it (it has no seal), and
		// it is reclaimed on the next relabel. Take the next id rather than failing.
		if err := e.vol.RemoveSlot(id); err != nil {
			if errors.Is(err, media.ErrNoPerSlotRemoval) {
				continue
			}
			return "", 0, err
		}
		return id, seq, nil
	}
}

// backupItem archives a single DLE into the slot via the writer. It owns the
// archiver side (resolving the archiver, building the request, requiring the base
// incremental state for incrementals); the writer owns the on-media side. It reports
// the DLE's lifecycle (start, live bytes, finish/fail) to the run tracker.
func (e *Engine) backupItem(w *slotio.Writer, item planner.Item, tr *progress.Tracker, logf Logf) (err error) {
	// The progress tracker keys and displays DLEs by their host:path identity; the
	// seal and filenames keep the internal slug (spec.DLE below).
	pname := item.DLE.ID()
	tr.StartDLE(pname)
	var arch format.Archive
	defer func() {
		if err != nil {
			tr.FinishDLE(pname, 0, 0, 0, err)
		} else {
			tr.FinishDLE(pname, arch.FileCount, arch.Uncompressed, arch.Compressed, nil)
		}
	}()

	ar, err := e.archiverFor(item.DLE.DumpTypeName(), item.DLE.Host)
	if err != nil {
		return err
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
			return fmt.Errorf("DLE %s: incremental L%d needs the L%d incremental state but it is missing",
				item.Name, item.Level, item.BaseLevel)
		}
	}

	logf.log("archiving %s (L%d)", item.DLE.ID(), item.Level)

	encScheme, encOpts := e.encryptionFor(item.DLE.DumpTypeName())
	spec := slotio.ArchiveSpec{
		DLE:      item.Name,
		Host:     item.DLE.Host,
		Path:     item.DLE.Path,
		Archiver: ar.Name(),
		Level:    item.Level,
		BaseSlot: item.BaseSlot,
		Encrypt:  encScheme,
		EncOpts:  encOpts,
	}
	// Configurable point: when a transform runs `at: client`, it runs on the DLE's host
	// executor (the same one tar uses), so it fuses with tar on the client and plaintext
	// never leaves it. Otherwise it runs server-side (nil = Local). The meter that follows
	// is always server-side, so the seal still covers the bytes that land.
	hostExec, _ := e.executorFor(item.DLE.Host)
	if e.cfg.ResolveDumpType(item.DLE.DumpTypeName()).Compress == "client" {
		spec.CompressExec = hostExec
	}
	if e.cfg.EncryptionFor(item.DLE.DumpTypeName()).At == "client" {
		spec.EncryptExec = hostExec
	}

	bs, berr := ar.BackupSource(req)
	if berr != nil {
		return fmt.Errorf("archive %s: %w", item.Name, berr)
	}
	src := slotio.Source{
		Stage:   bs.Stage,
		Exec:    bs.Exec,
		Cleanup: bs.Cleanup,
		Finish: func() (slotio.Produced, error) {
			res, ferr := bs.Finish()
			if ferr != nil {
				return slotio.Produced{}, ferr
			}
			return slotio.Produced{Uncompressed: res.Uncompressed, FileCount: res.FileCount, Members: res.Members}, nil
		},
	}
	progressFn := func(uncompressed, compressed int64) { tr.AddBytes(pname, uncompressed, compressed) }
	arch, err = w.WriteArchive(spec, progressFn, src)
	if err != nil {
		return fmt.Errorf("archive %s: %w", item.Name, err)
	}

	sizeLabel := "compressed"
	if arch.Codec == "none" {
		sizeLabel = "stored" // no compressor in the pipe; "compressed" would be a lie
	}
	if arch.FileCount == 0 {
		// An incremental with nothing changed still writes tar's structural overhead
		// (archive header/footer + directory census); say so rather than the puzzling
		// "0 file(s), 10.24 kB stored".
		logf.log("  no changed files (%s of tar metadata)", sizeutil.FormatBytes(arch.Compressed))
	} else {
		logf.log("  %d file(s), %s %s", arch.FileCount, sizeutil.FormatBytes(arch.Compressed), sizeLabel)
	}
	return nil
}

// Restore reconstructs a DLE as of a slot into destDir. A whole-DLE restore
// replays the chain with GNU tar's listed-incremental extraction, which makes
// each restored directory match the archive's census — deleting anything on disk
// not in it. Pointed at a populated destDir that prunes unrelated files, so unless
// force is set Restore refuses a non-empty destination rather than silently
// destroying its contents. It reads from any available copy (own medium first).
func (e *Engine) Restore(slotID, dleName, destDir string, force bool, logf Logf) error {
	if !force {
		if err := errNonEmptyDest(destDir); err != nil {
			return err
		}
	}
	return e.restoreFrom(slotID, dleName, destDir, "", logf)
}

// RestoreTo restores a DLE onto a remote client (Amanda's recover to a different host):
// extraction runs on destHost over SSH and destPath is a path on that client, so the
// data lands back where it came from. Decode stays server-side, covering the server-side
// and asymmetric (private-key-on-server) postures; an untrusted-server client-only key
// restores with the documented stock one-liner. destHost must be configured under hosts:.
// The non-empty-destination guard is the operator's to honor here (the path is remote),
// so a whole-DLE restore over --to assumes an empty/new client directory.
func (e *Engine) RestoreTo(slotID, dleName, destHost, destPath string, logf Logf) error {
	if _, ok := e.cfg.RemoteHost(destHost); !ok {
		return fmt.Errorf("--to host %q is not configured under hosts:", destHost)
	}
	return e.restoreFrom(slotID, dleName, destPath, destHost, logf)
}

// restoreFrom replays a DLE's restore chain into destDir. targetHost "" extracts
// server-side (the default); a `--to host:path` restore sets it so tar runs on that
// client and destDir is a client path — and, for a client-held key, decode runs there
// too. The exported Restore/RestoreTo are thin wrappers; reads fail over across copies
// (medium-scoped reads are the drill's own path, drillChain).
func (e *Engine) restoreFrom(slotID, dleName, destDir, targetHost string, logf Logf) error {
	steps, err := restore.Chain(e.cat.Slots(), dleName, slotID)
	if err != nil {
		return err
	}
	// Decrypt placement: a `--to` restore of a DLE that keeps its key on the client decrypts
	// on that client — the only way to read an untrusted-server / client-symmetric archive
	// (the server has no key). Every other restore decrypts server-side, which must be
	// feasible (else fail fast). When decrypt is on the client there is nothing the server
	// needs the key for, so the feasibility gate is skipped.
	ec := config.EncryptConfig{}
	if d, ok := e.dleByName(dleName); ok {
		ec = e.cfg.EncryptionFor(d.DumpTypeName())
	}
	decryptOnClient := targetHost != "" && ec.At == "client" && ec.SchemeName() != "none"
	if decryptOnClient {
		logf.log("decrypting on %s (encrypt.at: client) — only ciphertext leaves the server", targetHost)
	} else if err := e.ensureServerCanDecode(steps, logf); err != nil {
		return err
	}
	for _, step := range steps {
		logf.log("extracting %s %s L%d -> %s", step.SlotID, e.DisplayDLE(step.DLE), step.Level, destDir)
		if err := e.extractStep(step, destDir, targetHost, ec); err != nil {
			return fmt.Errorf("extract %s %s L%d: %w", step.SlotID, step.DLE, step.Level, err)
		}
	}
	return nil
}

// extractStep replays one archive step into destDir as a decode→extract pipeline whose
// stages each run on the host that should run them: decrypt where the key lives (the client
// for a client-held key reached over `--to`, otherwise the server) and decompress + tar
// extraction on the target host. RunGrouped fuses the same-host runs and crosses the wire
// at most once between them — so a client-held key decrypts on the client (only ciphertext
// leaves the server), while a server-held key decrypts on the server and ships compressed
// plaintext (never inflated) to a remote target. targetHost "" extracts server-side.
func (e *Engine) extractStep(step restore.Step, destDir, targetHost string, ec config.EncryptConfig) error {
	return e.extractInto(step.SlotID, step.DLE, step.Level, step.Codec, step.Encrypt, step.Archiver, destDir, targetHost, ec, nil)
}

// extractInto streams an archive's raw parts through the decode→extract pipeline into
// destDir on the target host. With members it extracts only those entries in plain mode
// (selected-file recovery, which never deletes); without, a whole-archive listed-incremental
// chain restore. It is the engine's one extraction path — whole-DLE restore, `--to` client
// restore, and file-level recover all run through it.
func (e *Engine) extractInto(slotID, dle string, level int, codec, encrypt, archiverType, destDir, targetHost string, ec config.EncryptConfig, members []string) error {
	target, _ := e.executorFor(targetHost)
	if err := target.MkdirAll(destDir); err != nil {
		return err
	}
	arch, err := e.restoreArchiver(archiverType, targetHost)
	if err != nil {
		return err
	}
	raw, err := e.openRawFrom(slotID, dle, level, "")
	if err != nil {
		return err
	}
	stages, err := e.decodeStages(encrypt, codec, ec, targetHost, target)
	if err != nil {
		raw.Close()
		return err
	}
	stages = append(stages, hostexec.Stage{Cmd: arch.RestoreStage(destDir, members), Exec: target})
	return decryptHint(encrypt, runDecodePipeline(raw, stages...))
}

// decodeStages returns the decrypt and decompress stages of a restore pipeline, each placed
// on the host that should run it. Decrypt — the only stage that needs the key — runs on the
// client (with the client's key reference) for a client-held key reached over `--to`, and on
// the server (with the server's) otherwise. Decompress always runs on the target host, so a
// remote restore ships compressed bytes over the wire rather than inflating them first.
func (e *Engine) decodeStages(encrypt, codec string, ec config.EncryptConfig, targetHost string, target hostexec.Executor) ([]hostexec.Stage, error) {
	decExec, copts := hostexec.Local(), e.dcopts
	if ec.At == "client" && targetHost != "" {
		decExec = target
		copts = crypt.Options{Program: ec.Program, PassphraseFile: ec.PassphraseFile}
	}
	var stages []hostexec.Stage
	if cmd, ok, err := crypt.DecryptCmd(encrypt, copts); err != nil {
		return nil, err
	} else if ok {
		stages = append(stages, hostexec.Stage{Cmd: cmd, Exec: decExec})
	}
	if cmd, ok, err := filter.DecompressCmd(codec, e.fopts); err != nil {
		return nil, err
	} else if ok {
		stages = append(stages, hostexec.Stage{Cmd: cmd, Exec: target})
	}
	return stages, nil
}

// runDecodePipeline runs a restore's decode→extract stages, draining tar's (empty) stdout
// and reaping every stage. Stages are reaped in pipeline order, so when an upstream child
// fails (a wrong key, a codec drift) its error — not the downstream "truncated input"
// symptom it causes in tar — is the one returned.
func runDecodePipeline(raw io.ReadCloser, stages ...hostexec.Stage) error {
	out, wait, err := hostexec.RunGrouped(raw, stages...)
	if err != nil {
		raw.Close()
		return err
	}
	_, copyErr := io.Copy(io.Discard, out) // tar -x writes to the fs; its stdout is empty
	out.Close()
	werr := wait()
	cerr := raw.Close() // a media-read fault on the ciphertext parts surfaces here
	if werr == nil {
		werr = copyErr
	}
	if werr == nil {
		werr = cerr
	}
	return werr
}

// errMissingCopy marks a read failure where the catalog knows of no available copy of
// the requested slot/archive (a missing slot, a missing copy on the named medium, or a
// placement that does not carry the archive). The drill classifies it as ClassMissing
// via errors.Is, so the classification does not depend on the message wording.
var errMissingCopy = errors.New("no available copy")

// eachPlacement resolves the placements holding a slot (all copies, or only those on
// `medium` when set), then tries each that carries the requested archive — opening its
// parts via open — until one succeeds, so a read fails over to another copy. It is the
// one place the raw and decoded read paths share: the copy selection, the missing-copy
// errors (errMissingCopy), and the fail-over loop.
func (e *Engine) eachPlacement(slotID, dle string, level int, medium string,
	open func(parts []format.FilePos, lib *librarian.Librarian, p catalog.Placement) (io.ReadCloser, error)) (io.ReadCloser, error) {
	placements := e.placementsFor(slotID)
	if medium != "" {
		placements = placementsOnMedium(placements, medium)
	}
	if len(placements) == 0 {
		if medium != "" {
			return nil, fmt.Errorf("%w: slot %s has no copy on medium %q", errMissingCopy, slotID, medium)
		}
		return nil, fmt.Errorf("%w: slot %s not in catalog (run `nb rebuild`)", errMissingCopy, slotID)
	}
	var lastErr error
	for _, p := range placements {
		parts, ok := p.Parts(dle, level)
		if !ok {
			continue
		}
		lib, _, _, err := e.librarianFor(p.Medium)
		if err != nil {
			lastErr = err
			continue
		}
		rc, err := open(parts, lib, p)
		if err != nil {
			lastErr = err
			continue
		}
		return rc, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w of %s %s L%d in the catalog", errMissingCopy, slotID, dle, level)
	}
	return nil, lastErr
}

// openRawFrom opens an archive's raw (undecoded, ciphertext) part stream — the input the
// restore feeds into its host-placed decode→extract pipeline (extractInto), with the same
// copy selection and fail-over as openArchiveFrom but without reversing any transform.
func (e *Engine) openRawFrom(slotID, dle string, level int, medium string) (io.ReadCloser, error) {
	return e.eachPlacement(slotID, dle, level, medium, func(parts []format.FilePos, lib *librarian.Librarian, p catalog.Placement) (io.ReadCloser, error) {
		return e.reader.OpenRawParts(parts, slotio.Expect{Slot: slotID, DLE: dle, Level: level}, e.partOpener(lib, p.Medium))
	})
}

// ensureServerCanDecode fails fast when a restore would have the server decrypt an
// archive whose key cannot be there. Decode is server-side for both a plain restore and
// `--to` (only tar extraction moves to the client), so an archive encrypted on the client
// is the infeasible combination. The only case the engine can decide for certain is a
// **client-side symmetric** dump (`encrypt.at: client`, a passphrase, no recipient): a
// symmetric key has no escrow path, so the passphrase stays on the client and the server
// provably cannot decrypt — that is a hard error pointing at the stock one-liner. A
// client-side **public-key** dump might have its private key escrowed in this server's
// keyring (a supported posture), so that is a warning, not a failure; a genuinely missing
// key still surfaces (with decryptHint) when gpg runs. A DLE no longer in the config is
// skipped — we cannot know its posture.
func (e *Engine) ensureServerCanDecode(steps []restore.Step, logf Logf) error {
	warned := map[string]bool{}
	for _, s := range steps {
		if s.Encrypt == "" || s.Encrypt == "none" {
			continue
		}
		d, ok := e.dleByName(s.DLE)
		if !ok {
			continue
		}
		hardErr, warn := clientSideKeyRestore(e.cfg.EncryptionFor(d.DumpTypeName()), s.DLE)
		if hardErr != nil {
			return hardErr
		}
		if warn && !warned[s.DLE] {
			warned[s.DLE] = true
			logf.log("WARNING: DLE %s is encrypted on the client (encrypt.at: client); a server-side restore can only decrypt it if its private key is escrowed in this server's gpg keyring — otherwise restore it on the client", s.DLE)
		}
	}
	return nil
}

// clientSideKeyRestore classifies a server-side decode of an archive by the DLE's
// configured encryption posture (pure, so it is unit-tested without a live SSH dump): a
// hard error for client-side **symmetric** (a passphrase has no escrow path, so the server
// provably cannot decrypt), warn=true for client-side **public-key** (the private key may
// be escrowed on the server), or neither for server-side / plaintext.
func clientSideKeyRestore(ec config.EncryptConfig, dleName string) (hardErr error, warn bool) {
	if ec.At != "client" || ec.SchemeName() == "none" {
		return nil, false
	}
	if ec.Recipient == "" && ec.PassphraseFile != "" { // symmetric: no escrow path
		return fmt.Errorf("DLE %s is encrypted on the client with a passphrase (encrypt.at: client, symmetric): the passphrase never leaves the client, so a server-side restore cannot decrypt it — restore onto the client with `nb recover --all --to <client>:<path>` (decryption then runs there), or use the stock one-liner on the client", dleName), false
	}
	return nil, true
}

// dleByName returns the configured DLE with the given catalog name, if it is still in the
// config (an old slot's DLE may have been removed).
func (e *Engine) dleByName(name string) (config.DLE, bool) {
	for _, d := range e.cfg.DLEs() {
		if d.Name() == name {
			return d, true
		}
	}
	return config.DLE{}, false
}

// RestoreAsOf reconstructs a whole DLE as of a date (YYYY-MM-DD) into destDir —
// the deletion-accurate, whole-DLE counterpart to file-level recover. It resolves
// the date to the most recent slot on or before it (the same resolution recover's
// browse uses), then replays that DLE's chain. So a bare date means the same slot
// for both the browse view and a full restore.
func (e *Engine) RestoreAsOf(dle, asOf, destDir string, force bool, logf Logf) error {
	target, err := recovery.AsOf(e.cat.Slots(), asOf)
	if err != nil {
		return err
	}
	return e.Restore(target, dle, destDir, force, logf)
}

// RestoreAsOfTo is RestoreAsOf onto a remote client: it resolves the date to a slot and
// restores the DLE's chain to destPath on destHost over SSH (see RestoreTo).
func (e *Engine) RestoreAsOfTo(dle, asOf, destHost, destPath string, logf Logf) error {
	target, err := recovery.AsOf(e.cat.Slots(), asOf)
	if err != nil {
		return err
	}
	return e.RestoreTo(target, dle, destHost, destPath, logf)
}

// errNonEmptyDest refuses a whole-DLE restore into a destination that already
// holds files. Listed-incremental extraction prunes the destination to match the
// backup (that is how deletions are applied), so restoring over an existing tree
// would delete unrelated files in it. A missing or empty directory is fine.
func errNonEmptyDest(destDir string) error {
	entries, err := os.ReadDir(destDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // will be created empty
		}
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("destination %s is not empty: a whole-DLE restore prunes it to match the backup (GNU tar incremental extraction deletes files not in the archive), which would remove unrelated files — restore into a new/empty directory, or pass --force to restore into this one anyway", destDir)
	}
	return nil
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

// joinPipelineErr combines a reader-side consumer's error with the in-process
// decrypt/decompress pipeline's Close error — used by the verify (List) path, which reads
// a server-local decoded stream rather than composing host-placed stages. tar (`-t`) sits
// last in that pipe, so when an upstream child fails (a wrong key, a missing passphrase,
// codec drift) tar only sees truncated input and reports a generic "not a tar archive";
// the real cause surfaces on the pipeline's Close. Surfacing both keeps a key/decrypt
// failure from hiding behind tar's misleading message.
func joinPipelineErr(consumeErr, closeErr error) error {
	if consumeErr == nil {
		return closeErr // normal Close returns nil; a late pipeline error still surfaces
	}
	if closeErr != nil {
		return fmt.Errorf("%w\n(decrypt/decompress pipeline: %v)", consumeErr, closeErr)
	}
	return consumeErr
}

// OpenRecover builds a browsable filesystem of a DLE as of a date (YYYY-MM-DD) —
// the amrecover entry point. It reads only the catalog (the member index lives in
// the seals), so no media is touched until files are extracted.
func (e *Engine) OpenRecover(dle, asOf string) (*recovery.Tree, error) {
	return recovery.BuildTree(e.cat.Slots(), dle, asOf)
}

// ExtractSelection extracts a selected set of files, grouped by their source
// archive, into destDir. It returns the number of member entries extracted.
func (e *Engine) ExtractSelection(steps []recovery.ExtractStep, destDir string, logf Logf) (int, error) {
	// File-level recover decodes server-side, so a client-only key is infeasible here —
	// fail fast (browse stays keyless; only extraction needs the key).
	for _, st := range steps {
		if d, ok := e.dleByName(st.DLE); ok {
			if hardErr, _ := clientSideKeyRestore(e.cfg.EncryptionFor(d.DumpTypeName()), st.DLE); hardErr != nil {
				return 0, hardErr
			}
		}
	}
	files := 0
	for _, st := range steps {
		logf.log("extracting %d file(s) from %s %s L%d", countFiles(st.Members), st.SlotID, e.DisplayDLE(st.DLE), st.Level)
		ec := config.EncryptConfig{}
		if d, ok := e.dleByName(st.DLE); ok {
			ec = e.cfg.EncryptionFor(d.DumpTypeName())
		}
		// Recover always extracts server-side (targetHost ""), so decrypt stays on the
		// server — the client-only-key case was already rejected above.
		if err := e.extractInto(st.SlotID, st.DLE, st.Level, st.Codec, st.Encrypt, st.Archiver, destDir, "", ec, st.Members); err != nil {
			return files, fmt.Errorf("extract from %s %s L%d: %w", st.SlotID, st.DLE, st.Level, err)
		}
		files += countFiles(st.Members)
	}
	return files, nil
}

// countFiles counts the file members in a selection, excluding the parent
// directories the extractor recreates to hold them (the archiver-neutral member
// convention marks directories with a trailing slash). So recovering one nested
// file reports 1, not "2 entries" once its parent dir is counted.
func countFiles(members []string) int {
	n := 0
	for _, m := range members {
		if !strings.HasSuffix(m, "/") {
			n++
		}
	}
	return n
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

// dleDisplayMap maps each internal DLE slug to its Amanda-style host:path identity,
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

// openArchiveFrom opens an archive for reading. With medium == "" it tries every
// copy, preferring the engine's own medium, until one opens (restore fails over to a
// copy); with medium set it reads only that medium's copy (a medium-scoped drill /
// restore against the offsite copy), so a fault on that copy is not masked by another.
func (e *Engine) openArchiveFrom(slotID, dle string, level int, codec, encrypt, medium string) (io.ReadCloser, error) {
	return e.eachPlacement(slotID, dle, level, medium, func(parts []format.FilePos, lib *librarian.Librarian, p catalog.Placement) (io.ReadCloser, error) {
		return e.reader.OpenArchiveParts(parts, codec, encrypt, slotio.Expect{Slot: slotID, DLE: dle, Level: level}, e.partOpener(lib, p.Medium))
	})
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

	reclaim := map[string]media.Reclamation{}
	for _, r := range profile.Reclaim(slots, floor, now) {
		reclaim[r.SlotID] = r
	}

	for _, s := range slots {
		if _, ok := reclaim[s.ID]; ok {
			continue // reported below
		}
		if reason, ok := floor.Reason(s.ID); ok {
			logf.log("keep   %s  (%s)", s.ID, reason)
		} else {
			logf.log("keep   %s  (fits capacity)", s.ID)
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
		r, ok := reclaim[s.ID]
		if !ok {
			continue
		}
		eligible++
		if apply {
			// Reclaim the copy on this medium only; the slot survives in the catalog
			// if it still has a copy elsewhere.
			if err := vol.RemoveSlot(s.ID); err != nil {
				return eligible, freed, fmt.Errorf("delete %s: %w", s.ID, err)
			}
			if _, err := e.cat.RemovePlacement(s.ID, mediumName); err != nil {
				return eligible, freed, fmt.Errorf("update catalog cache: %w", err)
			}
			freed += r.Bytes
			logf.log("DELETE %s  (%s freed, %s)", s.ID, sizeutil.FormatBytes(r.Bytes), r.Note)
		} else {
			logf.log("would delete %s  (%s, %s)", s.ID, sizeutil.FormatBytes(r.Bytes), r.Note)
		}
	}
	return eligible, freed, nil
}
