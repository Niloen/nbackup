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
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/filter"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/method"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/policy"
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
	cfg     *config.Config
	vol     media.Volume
	reader  *slotio.Reader
	profile media.Profile
	minAge  time.Duration
	cat     *catalog.Catalog
	methods map[string]method.Method // by cache key (dumptype or "@method")
	codec   string                   // compression codec for new archives
	fopts   filter.Options           // codec invocation options (level/threads/nice)
}

// New constructs an Engine from configuration: it opens the landing volume and
// its capacity profile via the media registry, and loads the catalog cache
// (refreshing it from the volume the first time it is needed). Dump methods are
// opened lazily per dumptype.
func New(cfg *config.Config) (*Engine, error) {
	mediaDef, err := cfg.LandingMedia()
	if err != nil {
		return nil, err
	}
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
	if err := cat.EnsureFresh(vol); err != nil {
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
		cfg:     cfg,
		vol:     vol,
		reader:  slotio.NewReader(vol, fopts),
		profile: profile,
		minAge:  minAge,
		cat:     cat,
		methods: map[string]method.Method{},
		codec:   cfg.CompressCodec(),
		fopts:   fopts,
	}, nil
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

// RebuildCatalog rescans the volume and rewrites the local cache, returning the
// number of slots indexed.
func (e *Engine) RebuildCatalog() (int, error) {
	return e.cat.Rebuild(e.vol)
}

// CopySlot streams a sealed slot from the landing volume to another configured
// medium, file by file. The destination assigns its own positions; restore from
// it works after a catalog rebuild against that medium.
func (e *Engine) CopySlot(slotID, targetMedia string, logf Logf) error {
	if _, err := e.cat.ReadSlot(slotID); err != nil {
		return err
	}
	if targetMedia == e.cfg.Landing {
		return fmt.Errorf("target medium %q is the landing medium", targetMedia)
	}
	def, ok := e.cfg.Media[targetMedia]
	if !ok {
		return fmt.Errorf("unknown medium %q", targetMedia)
	}
	dst, err := media.OpenVolume(def.Type, media.Options(def.Params))
	if err != nil {
		return err
	}
	logf.log("copying %s to %q", slotID, targetMedia)
	n, err := media.CopySlot(dst, e.vol, slotID)
	if err != nil {
		return err
	}
	logf.log("copied %d file(s) to %q", n, targetMedia)
	return nil
}

// Catalog exposes the catalog for read-only commands.
func (e *Engine) Catalog() *catalog.Catalog { return e.cat }

// Plan builds the plan for a run date: it estimates every DLE, then balances
// fulls (degrade to fit capacity, optionally promote to fill light runs).
func (e *Engine) Plan(date time.Time) *planner.Plan {
	dles := e.cfg.DLEs()
	return planner.Build(dles, e.cat.History(), e.estimates(dles), planner.Params{
		FullIntervalDays:    e.cfg.FullIntervalDays(),
		CapacityBytes:       e.profile.TotalBytes(),
		CapacityRoomBytes:   e.capacityRoom(date),
		Promote:             e.cfg.Cycle.Promote,
		PromoteCeilingBytes: e.promoteCeiling(),
	}, date)
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

// capacityRoom is the hard per-run write ceiling: capacity minus the bytes that
// cannot be reclaimed by pruning (the protected set). Negative = unbounded.
func (e *Engine) capacityRoom(now time.Time) int64 {
	capacity := e.profile.TotalBytes()
	if capacity <= 0 {
		return -1
	}
	slots := e.cat.Slots()
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

	slotID, seq, err := e.allocSlotID(date)
	if err != nil {
		return nil, err
	}
	s := slot.NewSlot(slotID, slot.DateString(date), seq, "nbdump", time.Now().UTC())
	w, err := slotio.NewWriter(e.vol, s, e.codec, e.fopts)
	if err != nil {
		return nil, err
	}

	if err := e.runDumpers(plan.Items, w, logf); err != nil {
		return nil, err
	}

	logf.log("verifying %d archive checksum(s)", w.ArchiveCount())
	sealed, err := w.Seal(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	posMap := map[string]int{}
	for _, p := range w.Positions() {
		posMap[catalog.ArchiveKey(p.DLE, p.Level)] = p.Pos
	}
	if err := e.cat.Add(sealed, posMap); err != nil {
		return nil, fmt.Errorf("update catalog cache: %w", err)
	}
	return sealed, nil
}

// runDumpers backs up every planned item into the slot. With parallelism.dumpers
// > 1 it runs that many dumpers concurrently (Amanda's inparallel), bounded by a
// semaphore; the first error stops scheduling further items and is returned. Each
// dumper writes a distinct object into the slot, which the medium must allow
// concurrently (disk does) and the slot Writer serializes its bookkeeping.
func (e *Engine) runDumpers(items []planner.Item, w *slotio.Writer, logf Logf) error {
	dumpers := e.cfg.Dumpers()
	if dumpers <= 1 || len(items) <= 1 {
		for _, item := range items {
			if err := e.backupItem(w, item, logf); err != nil {
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
			if err := e.backupItem(w, it, logf); err != nil {
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
// base snapshot for incrementals); the writer owns the on-media side.
func (e *Engine) backupItem(w *slotio.Writer, item planner.Item, logf Logf) error {
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
	arch, err := w.WriteArchive(spec, func(out io.Writer) (slotio.Produced, error) {
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
	pos, ok := e.cat.Position(step.SlotID, step.DLE, step.Level)
	if !ok {
		return fmt.Errorf("position not found in catalog (run `nb catalog rebuild`)")
	}
	rc, err := e.reader.OpenArchive(pos, step.Codec)
	if err != nil {
		return err
	}
	defer rc.Close()
	return m.Restore(rc, destDir)
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
	ok := true
	for _, a := range s.Archives {
		pos, found := e.cat.Position(id, a.DLE, a.Level)
		if !found {
			logf.log("%s: %s L%d POSITION MISSING", id, a.DLE, a.Level)
			ok = false
			continue
		}
		good, verr := e.reader.VerifyFile(pos, a.SHA256)
		if verr != nil {
			logf.log("%s: %s L%d ERROR %v", id, a.DLE, a.Level, verr)
			ok = false
			continue
		}
		if !good {
			logf.log("%s: %s L%d CHECKSUM MISMATCH", id, a.DLE, a.Level)
			ok = false
		}
	}
	if ok {
		logf.log("%s: OK (%d archive(s))", id, len(s.Archives))
	}
	return ok, nil
}

// Prune reconciles the landing medium to its retention model: it computes the
// protected slots (cross-cutting safety) and asks the medium's retention
// strategy which non-protected slots to reclaim to fit capacity.
func (e *Engine) Prune(now time.Time, apply bool, logf Logf) (eligible int, err error) {
	slots := e.cat.Slots()
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
			if err := e.vol.RemoveSlot(s.ID); err != nil {
				return eligible, fmt.Errorf("delete %s: %w", s.ID, err)
			}
			if err := e.cat.Remove(s.ID); err != nil {
				return eligible, fmt.Errorf("update catalog cache: %w", err)
			}
			logf.log("DELETE %s  (%s freed, %s)", s.ID, sizeutil.FormatBytes(r.Bytes), r.Note)
		} else {
			logf.log("would delete %s  (%s, %s)", s.ID, sizeutil.FormatBytes(r.Bytes), r.Note)
		}
	}
	return eligible, nil
}
