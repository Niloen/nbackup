// Package engine is NBackup's orchestrator, analogous to Amanda's driver. It
// wires the planner, dump method, transfer pipeline, media store, catalog, and
// policy together to execute runs, restores, verification, and pruning. It is
// the only place that knows about all the abstractions at once; everything below
// it depends only on interfaces.
package engine

import (
	"fmt"
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/method"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/policy"
	"github.com/Niloen/nbackup/internal/restore"
	"github.com/Niloen/nbackup/internal/slot"

	// Register the bundled media and method implementations.
	_ "github.com/Niloen/nbackup/internal/media/localdisk"
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
// store; the catalog is a local cache the engine refreshes from the store.
// Dump methods are resolved per dumptype and cached.
type Engine struct {
	cfg                      *config.Config
	store                    media.Store
	profile                  media.Profile
	minAge                   time.Duration
	requireVerifiedSuccessor bool
	cat                      *catalog.Catalog
	methods                  map[string]method.Method // by cache key (dumptype or "@method")
}

// New constructs an Engine from configuration: it opens the landing store and
// its capacity profile via the media registry, and loads the catalog cache
// (refreshing it from the store the first time it is needed). Dump methods are
// opened lazily per dumptype.
func New(cfg *config.Config) (*Engine, error) {
	mediaDef, err := cfg.LandingMedia()
	if err != nil {
		return nil, err
	}
	store, err := media.OpenStore(mediaDef.Type, media.Options(mediaDef.Params))
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
	if err := cat.EnsureFresh(store); err != nil {
		return nil, err
	}
	minAge, _ := mediaDef.MinAge()
	return &Engine{
		cfg:                      cfg,
		store:                    store,
		profile:                  profile,
		minAge:                   minAge,
		requireVerifiedSuccessor: cfg.Cycle.RequireVerifiedSuccessor,
		cat:                      cat,
		methods:                  map[string]method.Method{},
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

// RebuildCatalog rescans the store and rewrites the local cache, returning the
// number of slots indexed.
func (e *Engine) RebuildCatalog() (int, error) {
	return e.cat.Rebuild(e.store)
}

// Catalog exposes the catalog for read-only commands.
func (e *Engine) Catalog() *catalog.Catalog { return e.cat }

// Plan builds the plan for a run date, balancing fulls to the landing medium's
// preferred run size.
func (e *Engine) Plan(date time.Time) *planner.Plan {
	return planner.Build(e.cfg.DLEs(), e.cat.History(), planner.Params{
		PreferredRunBytes: e.profile.PreferredRunBytes(),
		FullIntervalDays:  e.cfg.FullIntervalDays(),
	}, date)
}

// Estimate returns the uncompressed bytes a planned item would archive.
func (e *Engine) Estimate(item planner.Item) (int64, error) {
	m, err := e.methodForDumpType(item.DLE.DumpTypeName())
	if err != nil {
		return 0, err
	}
	req := method.BackupRequest{SourcePath: item.DLE.Path, Level: item.Level}
	if item.Level >= 1 {
		req.BaseSnap = e.cat.SnapshotPath(item.Name, item.BaseLevel)
	}
	return m.Estimate(req)
}

// Run executes the plan for a date, producing one sealed slot.
func (e *Engine) Run(date time.Time, logf Logf) (*slot.Slot, error) {
	plan := e.Plan(date)

	// Pre-flight: resolve and check every method before creating a slot.
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
	s := &slot.Slot{
		ID:        slotID,
		Date:      slot.DateString(date),
		Sequence:  seq,
		CreatedAt: time.Now().UTC(),
		Status:    slot.StatusOpen,
		Generator: "nbdump",
	}
	manifest := &slot.Manifest{SlotID: slotID}
	checksums := map[string]string{}

	for _, item := range plan.Items {
		arch, files, err := e.backupItem(s, item, logf)
		if err != nil {
			return nil, err
		}
		s.Archives = append(s.Archives, *arch)
		s.TotalBytes += arch.Compressed
		checksums[arch.File] = arch.SHA256
		manifest.Archives = append(manifest.Archives, *files)
	}

	if err := e.writeObject(slotID, slot.FileManifest, manifest.Marshal); err != nil {
		return nil, err
	}
	if err := e.putBytes(slotID, slot.FileChecksums, slot.FormatChecksums(checksums)); err != nil {
		return nil, err
	}

	logf.log("verifying %d archive checksum(s)", len(checksums))
	if err := e.verifyChecksums(slotID, checksums); err != nil {
		return nil, err
	}

	// Seal: write SLOT.json last, then record the slot in the local cache.
	s.Status = slot.StatusSealed
	s.SealedAt = time.Now().UTC()
	if err := e.writeObject(slotID, slot.FileSlot, s.Marshal); err != nil {
		return nil, err
	}
	if err := e.cat.Add(s); err != nil {
		return nil, fmt.Errorf("update catalog cache: %w", err)
	}
	return s, nil
}

// allocSlotID picks the slot ID for a run on the given date: the first run of
// the day is "slot-DATE", later runs get the next free ".N". A leftover open
// (unsealed) slot from a failed attempt is reclaimed. This consults the store
// (the write path may touch media) so it is robust to a stale cache.
func (e *Engine) allocSlotID(date time.Time) (id string, seq int, err error) {
	ids, err := e.store.ListSlots()
	if err != nil {
		return "", 0, err
	}
	existing := map[string]bool{}
	for _, x := range ids {
		existing[x] = true
	}
	day := slot.DateString(date)
	for seq = 1; ; seq++ {
		id = slot.IDFromParts(day, seq)
		if !existing[id] {
			return id, seq, nil
		}
		sealed, serr := catalog.SealedID(e.store, id)
		if serr == nil && sealed {
			continue // a sealed slot occupies this id; try the next sequence
		}
		// Unsealed/unreadable leftover: reclaim it.
		if err := e.store.Remove(id); err != nil {
			return "", 0, err
		}
		return id, seq, nil
	}
}

// backupItem archives a single DLE into the slot and returns its metadata.
func (e *Engine) backupItem(s *slot.Slot, item planner.Item, logf Logf) (*slot.Archive, *slot.ArchiveFiles, error) {
	m, err := e.methodForDumpType(item.DLE.DumpTypeName())
	if err != nil {
		return nil, nil, err
	}

	fileName := fmt.Sprintf("%s-L%d.tar.zst", item.Name, item.Level)
	rel := slot.DirArchives + "/" + fileName

	req := method.BackupRequest{
		SourcePath: item.DLE.Path,
		Level:      item.Level,
		OutSnap:    e.cat.SnapshotPath(item.Name, item.Level),
	}
	if item.Level >= 1 {
		req.BaseSnap = e.cat.SnapshotPath(item.Name, item.BaseLevel)
		if !e.cat.SnapshotExists(item.Name, item.BaseLevel) {
			return nil, nil, fmt.Errorf("DLE %s: incremental L%d needs the L%d snapshot but it is missing",
				item.Name, item.Level, item.BaseLevel)
		}
	}

	logf.log("archiving %s (L%d) from %s", item.Name, item.Level, item.DLE.Path)

	// dest (media) <- filter (zstd+checksum) <- source (dump method).
	w, err := e.store.Create(s.ID, rel)
	if err != nil {
		return nil, nil, err
	}
	sink, err := newSink(w)
	if err != nil {
		w.Close()
		return nil, nil, err
	}
	res, berr := m.Backup(req, sink)
	closeErr := sink.Close()
	wCloseErr := w.Close()
	if berr != nil {
		return nil, nil, fmt.Errorf("archive %s: %w", item.Name, berr)
	}
	if closeErr != nil {
		return nil, nil, closeErr
	}
	if wCloseErr != nil {
		return nil, nil, wCloseErr
	}

	logf.log("  %d file(s), %s compressed", res.FileCount, humanBytes(sink.Compressed()))

	arch := &slot.Archive{
		DLE:          item.Name,
		Host:         item.DLE.Host,
		Path:         item.DLE.Path,
		Method:       m.Name(),
		Level:        item.Level,
		File:         rel,
		Compressed:   sink.Compressed(),
		Uncompressed: res.Uncompressed,
		FileCount:    res.FileCount,
		SHA256:       sink.SHA256(),
		BaseSlot:     item.BaseSlot,
	}
	files := &slot.ArchiveFiles{DLE: item.Name, Level: item.Level, Files: res.Members}
	return arch, files, nil
}

// Restore reconstructs a DLE as of a slot into destDir.
func (e *Engine) Restore(slotID, dleName, destDir string, logf Logf) error {
	steps, err := restore.Chain(e.cat.Slots(), dleName, slotID)
	if err != nil {
		return err
	}
	for _, step := range steps {
		logf.log("extracting %s L%d -> %s", step.SlotID, step.Level, destDir)
		if err := e.extractStep(step, destDir); err != nil {
			return fmt.Errorf("extract %s: %w", step.File, err)
		}
	}
	return nil
}

func (e *Engine) extractStep(step restore.Step, destDir string) error {
	m, err := e.methodByName(step.Method)
	if err != nil {
		return err
	}
	rc, err := e.store.Open(step.SlotID, step.File)
	if err != nil {
		return err
	}
	defer rc.Close()
	src, err := newSource(rc)
	if err != nil {
		return err
	}
	defer src.Close()
	return m.Restore(src, destDir)
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
	data, err := e.readObject(id, slot.FileChecksums)
	if err != nil {
		return false, err
	}
	sums, err := slot.ParseChecksums(data)
	if err != nil {
		return false, err
	}
	ok := true
	for rel, want := range sums {
		got, herr := e.hashObject(id, rel)
		if herr != nil {
			logf.log("%s: %s MISSING (%v)", id, rel, herr)
			ok = false
			continue
		}
		if got != want {
			logf.log("%s: %s CHECKSUM MISMATCH", id, rel)
			ok = false
		}
	}
	if ok {
		logf.log("%s: OK (%d archive(s))", id, len(sums))
	}
	return ok, nil
}

// Prune reconciles the landing medium to its retention model: it computes the
// protected slots (cross-cutting safety) and asks the medium's retention
// strategy which non-protected slots to reclaim to fit capacity.
func (e *Engine) Prune(now time.Time, apply bool, logf Logf) (eligible int, err error) {
	slots := e.cat.Slots()
	protected := policy.Protected(slots, e.minAge, now, e.requireVerifiedSuccessor)
	plan := e.profile.Retention().Plan(slots, protected, now)

	reclaim := map[string]media.Reclamation{}
	for _, r := range plan.Reclaim {
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
			if err := e.store.Remove(s.ID); err != nil {
				return eligible, fmt.Errorf("delete %s: %w", s.ID, err)
			}
			if err := e.cat.Remove(s.ID); err != nil {
				return eligible, fmt.Errorf("update catalog cache: %w", err)
			}
			logf.log("DELETE %s  (%s freed, %s)", s.ID, humanBytes(r.Bytes), r.Note)
		} else {
			logf.log("would delete %s  (%s, %s)", s.ID, humanBytes(r.Bytes), r.Note)
		}
	}
	return eligible, nil
}

// --- object I/O helpers over the store ---

func (e *Engine) writeObject(slotID, name string, marshal func() ([]byte, error)) error {
	data, err := marshal()
	if err != nil {
		return err
	}
	return e.putBytes(slotID, name, data)
}

func (e *Engine) putBytes(slotID, name string, data []byte) error {
	w, err := e.store.Create(slotID, name)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

func (e *Engine) readObject(slotID, name string) ([]byte, error) {
	rc, err := e.store.Open(slotID, name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (e *Engine) hashObject(slotID, name string) (string, error) {
	rc, err := e.store.Open(slotID, name)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	return hashReader(rc)
}

func (e *Engine) verifyChecksums(slotID string, want map[string]string) error {
	for rel, w := range want {
		got, err := e.hashObject(slotID, rel)
		if err != nil {
			return fmt.Errorf("verify %s: %w", rel, err)
		}
		if got != w {
			return fmt.Errorf("checksum mismatch for %s before sealing", rel)
		}
	}
	return nil
}
