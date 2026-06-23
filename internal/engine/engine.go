// Package engine is NBackup's orchestrator, analogous to Amanda's driver. It
// wires the planner, dump method, transfer pipeline, media store, catalog, and
// policy together to execute runs, restores, verification, and pruning. It is
// the only place that knows about all the abstractions at once; everything below
// it depends only on interfaces.
package engine

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/filter"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/method"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/policy"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/restore"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/slot"
	"github.com/Niloen/nbackup/internal/slotio"

	// Register the bundled media and method implementations.
	_ "github.com/Niloen/nbackup/internal/media/disk"
	_ "github.com/Niloen/nbackup/internal/media/s3"
	_ "github.com/Niloen/nbackup/internal/media/tape"
	_ "github.com/Niloen/nbackup/internal/method/gnutar"
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
// Dump methods are resolved per dumptype and cached.
type Engine struct {
	cfg        *config.Config
	mediumName string       // name of the medium new dumps land on
	mediumDef  config.Media // its definition
	vol        media.Volume
	reader     *slotio.Reader
	profile    media.Profile
	minAge     time.Duration
	cat        *catalog.Catalog
	methods    map[string]method.Method // by cache key (dumptype or "@method")
	codec      string                   // compression codec for new archives
	fopts      filter.Options           // codec invocation options (level/threads/nice)
	op         Operator                 // optional: handles manual tape swaps (nil = unattended)
}

// SetOperator attaches an operator so manual single-drive media can prompt for a
// reel swap mid-command. Without one, manual swaps degrade to an actionable error.
func (e *Engine) SetOperator(op Operator) { e.op = op }

// New constructs an Engine from configuration: it opens the landing volume and
// its capacity profile via the media registry, and loads the catalog cache
// (refreshing it from the volume the first time it is needed). Dump methods are
// opened lazily per dumptype.
func New(cfg *config.Config) (*Engine, error) {
	name, err := cfg.LandingName()
	if err != nil {
		return nil, err
	}
	mediaDef := cfg.Media[name]
	vol, err := media.OpenVolume(mediaDef.Type, media.Options(mediaDef.Params))
	if err != nil {
		return nil, err
	}
	profile, err := media.OpenProfile(mediaDef.Type, media.Options(mediaDef.ProfileOptions()))
	if err != nil {
		return nil, err
	}
	cat, err := catalog.Open(cfg.WorkdirPath())
	if err != nil {
		return nil, err
	}
	if err := cat.EnsureFresh(name, vol); err != nil {
		return nil, err
	}
	minAge, _ := mediaDef.MinAge()
	fopts := filter.Options{
		Program: cfg.Compress.Program,
		Level:   cfg.Compress.Level,
		Threads: cfg.Compress.Threads,
		Nice:    cfg.Nice,
	}
	return &Engine{
		cfg:        cfg,
		mediumName: name,
		mediumDef:  mediaDef,
		vol:        vol,
		reader:     slotio.NewReader(vol, fopts),
		profile:    profile,
		minAge:     minAge,
		cat:        cat,
		methods:    map[string]method.Method{},
		codec:      cfg.CompressCodec(),
		fopts:      fopts,
	}, nil
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
		return nil, config.Media{}, false, fmt.Errorf("unknown medium %q", name)
	}
	v, err := media.OpenVolume(d.Type, media.Options(d.Params))
	return v, d, false, err
}

// Capacity returns the landing medium's total retainable bytes (0 = unbounded).
func (e *Engine) Capacity() int64 { return e.profile.TotalBytes() }

// BudgetStatus reports whether current usage exceeds capacity and the percent
// used (0 when unbounded).
func (e *Engine) BudgetStatus(current int64) (over bool, pct float64) {
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

// methodForDumpType resolves and caches the dump method for a dumptype,
// configured with the dumptype's options (plus the global tar path).
func (e *Engine) methodForDumpType(dtName string) (method.Method, error) {
	if m, ok := e.methods[dtName]; ok {
		return m, nil
	}
	dt := e.cfg.ResolveDumpType(dtName)
	m, err := method.Open(dt.Method, e.methodOptions(dt.Params))
	if err != nil {
		return nil, err
	}
	e.methods[dtName] = m
	return m, nil
}

// methodByName resolves and caches a method by name with only global options
// (used for restore, where the archive records the producing method).
func (e *Engine) methodByName(name string) (method.Method, error) {
	key := "@" + name
	if m, ok := e.methods[key]; ok {
		return m, nil
	}
	m, err := method.Open(name, e.methodOptions(nil))
	if err != nil {
		return nil, err
	}
	e.methods[key] = m
	return m, nil
}

// methodOptions merges dumptype params with global method configuration.
func (e *Engine) methodOptions(params map[string]string) method.Options {
	opts := method.Options{}
	for k, v := range params {
		opts[k] = v
	}
	if _, ok := opts["tar_path"]; !ok && e.cfg.GnuTarPath != "" {
		opts["tar_path"] = e.cfg.GnuTarPath
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

// CopySlot streams a sealed slot from one configured medium to another, then
// records the new copy in the catalog (a second placement). The source defaults to
// the landing medium when fromMedia is ""; any other medium holding the slot is
// allowed (e.g. un-vaulting tape -> disk). Reading the source mounts the volume
// that holds the slot (on a changer); the write to the target runs the same label
// verification as a dump.
func (e *Engine) CopySlot(slotID, fromMedia, targetMedia string, force bool, logf Logf) error {
	s, err := e.cat.ReadSlot(slotID)
	if err != nil {
		return err
	}
	if fromMedia == "" {
		fromMedia = e.mediumName
	}
	if fromMedia == targetMedia {
		return fmt.Errorf("copy source and target are the same medium %q", targetMedia)
	}
	// Idempotency: a slot already recorded on the target is not re-copied. On
	// append-only media a second copy would orphan the first (unreferenced files,
	// reclaimable only by relabel); --force overrides for a deliberate re-copy.
	if !force {
		for _, p := range e.cat.Placements(slotID) {
			if p.Medium == targetMedia {
				return fmt.Errorf("slot %s is already on medium %q (volume %q); use --force to copy again", slotID, targetMedia, p.Volume)
			}
		}
	}
	src, err := e.copySource(slotID, fromMedia)
	if err != nil {
		return err
	}
	dst, def, _, err := e.mediumVolume(targetMedia)
	if err != nil {
		return err
	}
	volName, epoch, err := e.verifyWritableInteractive(dst, targetMedia, def.IsAppendable(), time.Now().UTC(), logf)
	if err != nil {
		return err
	}
	logf.log("copying %s from %q to %q", slotID, fromMedia, targetMedia)
	files, err := media.CopySlot(dst, src, slotID)
	if err != nil {
		return err
	}
	p := catalog.Placement{Medium: targetMedia, Volume: volName, Epoch: epoch}
	for _, f := range files {
		if f.Header.Kind == media.KindArchive {
			p.Archives = append(p.Archives, catalog.ArchivePos{DLE: f.Header.DLE, Level: f.Header.Level, Pos: f.Pos})
		}
	}
	if err := e.cat.Record(s, p); err != nil {
		return fmt.Errorf("record copy in catalog: %w", err)
	}
	logf.log("copied %d file(s) to %q", len(files), targetMedia)
	return nil
}

// copySource resolves the volume that holds a slot on the source medium and mounts
// it for reading — the read side of a copy. It mirrors readerFor: the engine's own
// medium reuses the open handle; any other medium is opened fresh; a changer is
// mounted to the volume holding the slot and its identity verified. It errors if
// the slot has no copy on fromMedia (the catalog knows of none to read).
func (e *Engine) copySource(slotID, fromMedia string) (media.Volume, error) {
	var src catalog.Placement
	for _, p := range e.cat.Placements(slotID) {
		if p.Medium == fromMedia {
			src = p
			break
		}
	}
	if src.Medium == "" {
		return nil, fmt.Errorf("slot %s has no copy on source medium %q", slotID, fromMedia)
	}
	vol := e.vol
	if fromMedia != e.mediumName {
		v, _, _, err := e.mediumVolume(fromMedia)
		if err != nil {
			return nil, err
		}
		vol = v
	}
	if err := e.mountForRead(vol, src); err != nil {
		return nil, err
	}
	if err := e.assertVolume(vol, src); err != nil {
		return nil, err
	}
	return vol, nil
}

// Catalog exposes the catalog for read-only commands.
func (e *Engine) Catalog() *catalog.Catalog { return e.cat }

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

// TapeExpectation describes the volume the next run on a labeled medium will
// write to — NBackup's analogue of Amanda's "amdump will expect tape X". It is
// derived from the catalog (the tapelist) and the retention policy, never from a
// physical scan: for a one-run-per-tape (non-appendable) medium it names the
// oldest reusable volume the run would recycle, or a fresh tape when none is
// reusable; for an appendable medium it names the current volume the run extends.
type TapeExpectation struct {
	Medium      string    // the labeled medium this expectation is for
	Appendable  bool      // true: extend a volume; false: one run per tape (recycle/fresh)
	Label       string    // the expected volume's label; "" when a fresh tape is expected
	WrittenAt   time.Time // when that volume was last labeled (zero for a fresh tape)
	Recycles    int       // runs on it the next run would overwrite (non-appendable reuse)
	NewTape     bool      // no reusable volume exists — a fresh/blank tape is expected
	VolumeBytes int64     // the reel's physical capacity (volume_size); 0 = unknown/unsized
	UsedBytes   int64     // bytes already on the expected reel (0 for a fresh/recycled reel)
}

// ExpectedTape reports the tape the next run on the landing medium will write to,
// or ok=false for address-identified media (disk, s3) that carry no label and so
// have no tape to expect.
func (e *Engine) ExpectedTape(now time.Time) (TapeExpectation, bool) {
	if _, ok := e.vol.(media.Labeled); !ok {
		return TapeExpectation{}, false
	}
	exp := e.expectedTapeFor(e.mediumName, now)
	// The reel's capacity and current fill bound this run physically: an appendable
	// run extends the latest reel (room = size - used), a fresh or recycled reel
	// offers a whole reel (used stays 0).
	exp.VolumeBytes = e.profile.VolumeSize()
	if exp.Appendable && !exp.NewTape {
		for _, s := range e.cat.SlotsOnVolume(exp.Label) {
			exp.UsedBytes += s.TotalBytes
		}
	}
	return exp, true
}

// expectedTapeFor computes the expected volume for a labeled medium from the
// catalog's volume registry (the tapelist) ordered oldest-written-first. A
// non-appendable run reuses the oldest volume whose every run is unprotected (the
// retention safety floor: past minimum age, with a newer recovery path), matching
// Amanda's taper picking the oldest reusable tape; an appendable run extends the
// most recently written volume in the pool.
func (e *Engine) expectedTapeFor(medium string, now time.Time) TapeExpectation {
	def := e.cfg.Media[medium]
	exp := TapeExpectation{Medium: medium, Appendable: def.IsAppendable()}

	var pool []catalog.VolumeRecord
	for _, v := range e.cat.Volumes() {
		if v.Label.Pool == medium {
			pool = append(pool, v)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].Label.WrittenAt.Before(pool[j].Label.WrittenAt) })

	if exp.Appendable {
		if n := len(pool); n > 0 {
			exp.Label, exp.WrittenAt = pool[n-1].Label.Name, pool[n-1].Label.WrittenAt
		} else {
			exp.NewTape = true
		}
		return exp
	}

	minAge, _ := def.MinAge()
	protected := policy.Protected(e.cat.Slots(), minAge, now)
	for _, v := range pool {
		held := e.cat.SlotsOnVolume(v.Label.Name)
		reusable := true
		for _, s := range held {
			if _, p := protected[s.ID]; p {
				reusable = false
				break
			}
		}
		if reusable {
			exp.Label, exp.WrittenAt, exp.Recycles = v.Label.Name, v.Label.WrittenAt, len(held)
			return exp
		}
	}
	exp.NewTape = true // nothing reusable — the run needs a fresh tape
	return exp
}

// Plan builds the plan for a run date: it estimates every DLE, then balances
// fulls (degrade to fit capacity, optionally promote to fill light runs).
func (e *Engine) Plan(date time.Time) *planner.Plan {
	dles := e.cfg.DLEs()
	return planner.Build(dles, e.cat.History(), e.estimates(dles), e.plannerParams(date), date)
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
		FullIntervalDays:    e.cfg.FullIntervalDays(),
		CapacityBytes:       e.profile.TotalBytes(),
		CapacityRoomBytes:   e.capacityRoom(date),
		Promote:             e.cfg.Cycle.Promote,
		PromoteCeilingBytes: e.promoteCeiling(),
	}
}

// estimates predicts each DLE's full and next-incremental size by asking the dump
// method (Amanda's "client" estimate). For gnutar this is a fast metadata-only
// tar pass; see gnutar.Estimate. Sizes are uncompressed — an upper bound on the
// compressed bytes finally stored.
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
	m, err := e.methodForDumpType(d.DumpTypeName())
	if err != nil || m.Check() != nil {
		return planner.Estimate{} // no estimator available (e.g. tar missing)
	}
	full, _ := m.Estimate(method.BackupRequest{SourcePath: d.Path, Level: 0})

	var incr int64
	lvl := st.IncrementalsSinceFull() + 1
	if lvl > planner.MaxLevel {
		lvl = planner.MaxLevel
	}
	if st.LastFullDate != "" && e.cat.SnapshotExists(name, lvl-1) {
		incr, _ = m.Estimate(method.BackupRequest{
			SourcePath: d.Path, Level: lvl, BaseSnap: e.cat.SnapshotPath(name, lvl-1),
		})
	}
	return planner.Estimate{Full: full, Incr: incr}
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
	protected := policy.Protected(slots, e.minAge, now)
	var protectedBytes int64
	for _, s := range slots {
		if _, ok := protected[s.ID]; ok {
			protectedBytes += s.TotalBytes
		}
	}
	if room := capacity - protectedBytes; room > 0 {
		return room
	}
	return 0
}

// volumeRoom is the physical bound: the bytes left on the reel the run lands on
// before it spills to the next. An appendable run extends the latest reel, so its
// room is volume_size minus what is already on it; a fresh or recycled reel
// offers a whole volume_size. Negative = unbounded (the medium has no reel size).
func (e *Engine) volumeRoom(now time.Time) int64 {
	exp, ok := e.ExpectedTape(now)
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

// promoteCeiling is the storage headroom promotion must not exceed, defaulting
// to 80% of capacity. Negative = unbounded.
func (e *Engine) promoteCeiling() int64 {
	capacity := e.profile.TotalBytes()
	if capacity <= 0 {
		return -1
	}
	h := e.cfg.Cycle.PromoteHeadroom
	if h <= 0 || h > 1 {
		h = 0.8
	}
	return int64(float64(capacity) * h)
}

// Run executes the plan for a date, producing one sealed slot.
func (e *Engine) Run(date time.Time, logf Logf) (*slot.Slot, error) {
	plan := e.Plan(date)
	for _, w := range plan.Warnings {
		logf.log("WARNING: %s", w)
	}

	// Pre-flight before creating a slot: the codec binary and every dump method.
	// Resolving every method here also populates the method cache, so the parallel
	// dumpers below only read it (no concurrent writes).
	if err := filter.Check(e.codec, e.fopts); err != nil {
		return nil, err
	}
	for _, item := range plan.Items {
		m, err := e.methodForDumpType(item.DLE.DumpTypeName())
		if err != nil {
			return nil, err
		}
		if err := m.Check(); err != nil {
			return nil, err
		}
	}

	volName, epoch, err := e.verifyWritableInteractive(e.vol, e.mediumName, e.mediumDef.IsAppendable(), time.Now().UTC(), logf)
	if err != nil {
		return nil, err
	}

	slotID, seq, err := e.allocSlotID(date)
	if err != nil {
		return nil, err
	}
	s := slot.NewSlot(slotID, slot.DateString(date), seq, "nbdump", time.Now().UTC())
	w, err := slotio.NewWriter(e.vol, s, e.codec, e.fopts)
	if err != nil {
		return nil, err
	}

	// Track live progress to the run-status file so `nb status` can watch a
	// detached run. Progress reporting never blocks or fails the backup.
	tr := progress.NewTracker(slotID, e.cfg.Dumpers(), planProgress(plan.Items), time.Now,
		progress.NewFileSink(e.cfg.WorkdirPath(), time.Now))

	if err := e.runDumpers(plan.Items, w, tr, logf); err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, err
	}

	tr.SetPhase(progress.PhaseSealing)
	logf.log("verifying %d archive checksum(s)", w.ArchiveCount())
	sealed, err := w.Seal(time.Now().UTC())
	if err != nil {
		tr.SetPhase(progress.PhaseFailed)
		return nil, err
	}
	placement := catalog.Placement{Medium: e.mediumName, Volume: volName, Epoch: epoch}
	for _, p := range w.Positions() {
		placement.Archives = append(placement.Archives, catalog.ArchivePos{DLE: p.DLE, Level: p.Level, Pos: p.Pos})
	}
	if err := e.cat.Record(sealed, placement); err != nil {
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
		out[i] = progress.Plan{Name: it.Name, Level: it.Level, EstBytes: it.EstBytes}
	}
	return out
}

// runDumpers backs up every planned item into the slot. With parallelism.dumpers
// > 1 it runs that many dumpers concurrently (Amanda's inparallel), bounded by a
// semaphore; the first error stops scheduling further items and is returned. Each
// dumper writes a distinct object into the slot, which the medium must allow
// concurrently (disk does) and the slot Writer serializes its bookkeeping.
func (e *Engine) runDumpers(items []planner.Item, w *slotio.Writer, tr *progress.Tracker, logf Logf) error {
	dumpers := e.cfg.Dumpers()
	if dumpers <= 1 || len(items) <= 1 {
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
	if cores := runtime.GOMAXPROCS(0); dumpers*threads > cores {
		logf.log("WARNING: %d dumpers x %d compressor thread(s) = %d exceeds %d cores; CPU may be oversubscribed",
			dumpers, threads, dumpers*threads, cores)
	}

	var (
		wg       sync.WaitGroup
		sem      = make(chan struct{}, dumpers)
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
func (e *Engine) allocSlotID(date time.Time) (id string, seq int, err error) {
	files, err := e.vol.Files()
	if err != nil {
		return "", 0, err
	}
	present := map[string]bool{} // slot id -> has any file
	sealed := map[string]bool{}  // slot id -> has a seal record
	for _, f := range files {
		present[f.Header.Slot] = true
		if f.Header.Kind == media.KindSeal {
			sealed[f.Header.Slot] = true
		}
	}
	day := slot.DateString(date)
	for seq = 1; ; seq++ {
		id = slot.IDFromParts(day, seq)
		if !present[id] {
			return id, seq, nil
		}
		if sealed[id] {
			continue // a sealed slot occupies this id; try the next sequence
		}
		// Unsealed leftover from a failed attempt: reclaim it.
		if err := e.vol.RemoveSlot(id); err != nil {
			return "", 0, err
		}
		return id, seq, nil
	}
}

// backupItem archives a single DLE into the slot via the writer. It owns the
// dump-method side (resolving the method, building the request, requiring the
// base snapshot for incrementals); the writer owns the on-media side. It reports
// the DLE's lifecycle (start, live bytes, finish/fail) to the run tracker.
func (e *Engine) backupItem(w *slotio.Writer, item planner.Item, tr *progress.Tracker, logf Logf) (err error) {
	tr.StartDLE(item.Name)
	var arch slot.Archive
	defer func() {
		if err != nil {
			tr.FinishDLE(item.Name, 0, 0, 0, err)
		} else {
			tr.FinishDLE(item.Name, arch.FileCount, arch.Uncompressed, arch.Compressed, nil)
		}
	}()

	m, err := e.methodForDumpType(item.DLE.DumpTypeName())
	if err != nil {
		return err
	}

	req := method.BackupRequest{
		SourcePath: item.DLE.Path,
		Level:      item.Level,
		OutSnap:    e.cat.SnapshotPath(item.Name, item.Level),
	}
	if item.Level >= 1 {
		req.BaseSnap = e.cat.SnapshotPath(item.Name, item.BaseLevel)
		if !e.cat.SnapshotExists(item.Name, item.BaseLevel) {
			return fmt.Errorf("DLE %s: incremental L%d needs the L%d snapshot but it is missing",
				item.Name, item.Level, item.BaseLevel)
		}
	}

	logf.log("archiving %s (L%d) from %s", item.Name, item.Level, item.DLE.Path)

	spec := slotio.ArchiveSpec{
		DLE:      item.Name,
		Host:     item.DLE.Host,
		Path:     item.DLE.Path,
		Method:   m.Name(),
		Level:    item.Level,
		BaseSlot: item.BaseSlot,
	}
	progressFn := func(uncompressed, compressed int64) { tr.AddBytes(item.Name, uncompressed, compressed) }
	arch, err = w.WriteArchive(spec, progressFn, func(out io.Writer) (slotio.Produced, error) {
		res, berr := m.Backup(req, out)
		if berr != nil {
			return slotio.Produced{}, berr
		}
		return slotio.Produced{Uncompressed: res.Uncompressed, FileCount: res.FileCount, Members: res.Members}, nil
	})
	if err != nil {
		return fmt.Errorf("archive %s: %w", item.Name, err)
	}

	logf.log("  %d file(s), %s compressed", arch.FileCount, sizeutil.FormatBytes(arch.Compressed))
	return nil
}

// Restore reconstructs a DLE as of a slot into destDir.
func (e *Engine) Restore(slotID, dleName, destDir string, logf Logf) error {
	steps, err := restore.Chain(e.cat.Slots(), dleName, slotID)
	if err != nil {
		return err
	}
	for _, step := range steps {
		logf.log("extracting %s %s L%d -> %s", step.SlotID, step.DLE, step.Level, destDir)
		if err := e.extractStep(step, destDir); err != nil {
			return fmt.Errorf("extract %s %s L%d: %w", step.SlotID, step.DLE, step.Level, err)
		}
	}
	return nil
}

func (e *Engine) extractStep(step restore.Step, destDir string) error {
	m, err := e.methodByName(step.Method)
	if err != nil {
		return err
	}
	rc, err := e.openArchive(step.SlotID, step.DLE, step.Level, step.Codec)
	if err != nil {
		return err
	}
	defer rc.Close()
	return m.Restore(rc, destDir, nil)
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
	files := 0
	for _, st := range steps {
		m, err := e.methodByName(st.Method)
		if err != nil {
			return files, err
		}
		rc, err := e.openArchive(st.SlotID, st.DLE, st.Level, st.Codec)
		if err != nil {
			return files, err
		}
		logf.log("extracting %d file(s) from %s %s L%d", len(st.Members), st.SlotID, st.DLE, st.Level)
		err = m.Restore(rc, destDir, st.Members)
		rc.Close()
		if err != nil {
			return files, fmt.Errorf("extract from %s %s L%d: %w", st.SlotID, st.DLE, st.Level, err)
		}
		files += len(st.Members)
	}
	return files, nil
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

// openArchive opens an archive from any available copy, preferring the engine's
// own medium, trying each placement until one opens (restore fails over to a copy).
func (e *Engine) openArchive(slotID, dle string, level int, codec string) (io.ReadCloser, error) {
	placements := e.placementsFor(slotID)
	if len(placements) == 0 {
		return nil, fmt.Errorf("slot %s not in catalog (run `nb rebuild`)", slotID)
	}
	var lastErr error
	for _, p := range placements {
		pos, ok := p.Pos(dle, level)
		if !ok {
			continue
		}
		rdr, err := e.readerFor(p)
		if err != nil {
			lastErr = err
			continue
		}
		rc, err := rdr.OpenArchive(pos, codec, slotio.Expect{Slot: slotID, DLE: dle, Level: level})
		if err != nil {
			lastErr = err
			continue
		}
		return rc, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no copy of %s %s L%d in the catalog", slotID, dle, level)
	}
	return nil, lastErr
}

// DLEsInSlot returns the distinct DLE names archived in a slot.
func (e *Engine) DLEsInSlot(s *slot.Slot) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range s.Archives {
		if !seen[a.DLE] {
			seen[a.DLE] = true
			out = append(out, a.DLE)
		}
	}
	return out
}

// Verify checks the checksums of the given slots (all if none given).
func (e *Engine) Verify(slotIDs []string, logf Logf) (failures int, err error) {
	if len(slotIDs) == 0 {
		for _, s := range e.cat.Slots() {
			slotIDs = append(slotIDs, s.ID)
		}
	}
	for _, id := range slotIDs {
		ok, verr := e.verifySlot(id, logf)
		if verr != nil {
			logf.log("%s: ERROR %v", id, verr)
			failures++
			continue
		}
		if !ok {
			failures++
		}
	}
	return failures, nil
}

func (e *Engine) verifySlot(id string, logf Logf) (bool, error) {
	s, err := e.cat.ReadSlot(id)
	if err != nil {
		return false, err
	}
	placements := e.placementsFor(id)
	if len(placements) == 0 {
		logf.log("%s: NO COPIES", id)
		return false, nil
	}
	ok := true
	// Verify every copy on every medium, so a corrupt copy is caught even when
	// another is fine.
	for _, p := range placements {
		rdr, err := e.readerFor(p)
		if err != nil {
			logf.log("%s [%s]: ERROR %v", id, p.Medium, err)
			ok = false
			continue
		}
		for _, a := range s.Archives {
			pos, found := p.Pos(a.DLE, a.Level)
			if !found {
				logf.log("%s [%s]: %s L%d POSITION MISSING", id, p.Medium, a.DLE, a.Level)
				ok = false
				continue
			}
			good, verr := rdr.VerifyFile(pos, slotio.Expect{Slot: id, DLE: a.DLE, Level: a.Level}, a.SHA256)
			if verr != nil {
				logf.log("%s [%s]: %s L%d ERROR %v", id, p.Medium, a.DLE, a.Level, verr)
				ok = false
			} else if !good {
				logf.log("%s [%s]: %s L%d CHECKSUM MISMATCH", id, p.Medium, a.DLE, a.Level)
				ok = false
			}
		}
	}
	if ok {
		logf.log("%s: OK (%d archive(s), %d cop(ies))", id, len(s.Archives), len(placements))
	}
	return ok, nil
}

// Prune reconciles the landing medium to its retention model: it computes the
// protected slots (cross-cutting safety) and asks the medium's retention
// strategy which non-protected slots to reclaim to fit capacity.
func (e *Engine) Prune(now time.Time, apply bool, logf Logf) (eligible int, err error) {
	slots := e.cat.SlotsOn(e.mediumName)
	protected := policy.Protected(slots, e.minAge, now)

	reclaim := map[string]media.Reclamation{}
	for _, r := range e.profile.Reclaim(slots, protected, now) {
		reclaim[r.SlotID] = r
	}

	for _, s := range slots {
		if _, ok := reclaim[s.ID]; ok {
			continue // reported below
		}
		if reason := protected[s.ID]; reason != "" {
			logf.log("keep   %s  (%s)", s.ID, reason)
		} else {
			logf.log("keep   %s  (fits capacity)", s.ID)
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
			if err := e.vol.RemoveSlot(s.ID); err != nil {
				return eligible, fmt.Errorf("delete %s: %w", s.ID, err)
			}
			if _, err := e.cat.RemovePlacement(s.ID, e.mediumName); err != nil {
				return eligible, fmt.Errorf("update catalog cache: %w", err)
			}
			logf.log("DELETE %s  (%s freed, %s)", s.ID, sizeutil.FormatBytes(r.Bytes), r.Note)
		} else {
			logf.log("would delete %s  (%s, %s)", s.ID, sizeutil.FormatBytes(r.Bytes), r.Note)
		}
	}
	return eligible, nil
}
