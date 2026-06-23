// Package engine is NBackup's orchestrator, analogous to Amanda's driver. It
// wires the planner, dump method, transfer pipeline, media store, catalog, and
// policy together to execute runs, restores, verification, and pruning. It is
// the only place that knows about all the abstractions at once; everything below
// it depends only on interfaces.
package engine

import (
	"errors"
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

// Operator handles physical actions the engine cannot perform itself — chiefly
// swapping the reel in a single-drive (manual) tape station when the loaded tape
// won't do. The CLI implements it interactively over stdin; an unattended run
// leaves it nil, so a manual swap degrades to an actionable error instead of
// blocking forever waiting for a human.
type Operator interface {
	// Swap asks the operator to load a reel into the drive for the stated need and
	// returns the chosen reel's Bay id (from req.Shelf), or ok=false to abort
	// (leaving the drive unchanged).
	Swap(req SwapRequest) (reel string, ok bool)
}

// SwapRequest describes why a manual tape swap is needed and what is available.
type SwapRequest struct {
	Medium string            // medium name
	Pool   string            // its label pool
	Reason string            // why the loaded reel won't do (the underlying error)
	Need   string            // a specific volume label wanted (read); "" means any writable (write)
	Loaded media.BayStatus   // what is in the drive now (zero value when empty)
	Shelf  []media.BayStatus // reels available to load
}

// reloadable wraps a write-eligibility failure that loading a different volume
// could fix (so a manual station can prompt for a swap), as opposed to a hard
// failure (stale catalog, I/O) that swapping would not help.
type reloadable struct{ error }

func reloadableErr(format string, a ...any) error { return reloadable{fmt.Errorf(format, a...)} }

func isReloadable(err error) bool { r := reloadable{}; return errors.As(err, &r) }

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
	// A changer medium (tape library) holds many volumes; summarize the count and
	// point at `nb changer` for the detail. A single labeled volume shows its name.
	if d.Type == "tape" {
		n := 0
		for _, v := range e.cat.Volumes() {
			if v.Label.Pool == name {
				n++
			}
		}
		info.Volume = fmt.Sprintf("%d tape(s)", n)
	} else if lbl, ok := e.cat.VolumeForMedium(name); ok {
		info.Volume = lbl.Name
		info.Epoch = lbl.Epoch
	}
	return info, true
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

// CopySlot streams a sealed slot from the engine's own volume to another
// configured medium, then records the new copy in the catalog (a second
// placement). Copy is a write, so it runs the same label verification as a dump.
func (e *Engine) CopySlot(slotID, targetMedia string, force bool, logf Logf) error {
	s, err := e.cat.ReadSlot(slotID)
	if err != nil {
		return err
	}
	dst, def, own, err := e.mediumVolume(targetMedia)
	if err != nil {
		return err
	}
	if own {
		return fmt.Errorf("target medium %q is the copy source", targetMedia)
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
	volName, epoch, err := e.verifyWritableInteractive(dst, targetMedia, def.IsAppendable(), time.Now().UTC(), logf)
	if err != nil {
		return err
	}
	logf.log("copying %s to %q", slotID, targetMedia)
	files, err := media.CopySlot(dst, e.vol, slotID)
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

// Catalog exposes the catalog for read-only commands.
func (e *Engine) Catalog() *catalog.Catalog { return e.cat }

// verifyWritable enforces the label protocol before writing to a medium (a dump
// to the landing, or a copy to a target). Address-identified media (disk, s3) are
// trusted by their path/bucket and return that name with epoch 0. For labeled
// (tape) media it refuses a foreign, blank (unless auto_label), wrong-pool, wrong,
// or relabeled-since volume — the overwrite and wrong-tape protection — records the
// accepted label, and returns the volume identity to record in a placement.
func (e *Engine) verifyWritable(vol media.Volume, medium string, appendable bool, now time.Time) (volName string, epoch int, err error) {
	lv, ok := vol.(media.Labeled)
	if !ok {
		return medium, 0, nil // address-identified: identity is the medium itself
	}
	lbl, labeled, err := lv.ReadLabel()
	switch {
	case errors.Is(err, media.ErrNoVolume):
		return "", 0, reloadableErr("medium %q has no tape loaded; load one with `nb changer load %s <bay>` or label a blank one with `nb label %s <name>`", medium, medium, medium)
	case errors.Is(err, media.ErrForeignVolume):
		return "", 0, reloadableErr("medium %q holds non-NBackup data; refusing to overwrite — relabel it explicitly with `nb label --force %s <name>`", medium, medium)
	case err != nil:
		return "", 0, err
	case !labeled: // blank volume
		if !e.cfg.AutoLabel {
			return "", 0, reloadableErr("medium %q is blank/unlabeled; run `nb label %s <name>` first (or set auto_label: true)", medium, medium)
		}
		lbl = media.Label{Name: fmt.Sprintf("%s-%s", medium, slot.DateString(now)), Pool: medium, Epoch: 1, WrittenAt: now}
		if err := lv.WriteLabel(lbl); err != nil {
			return "", 0, err
		}
	}
	if lbl.Pool != "" && lbl.Pool != medium {
		return "", 0, reloadableErr("mounted volume %q belongs to pool %q, not %q — wrong tape", lbl.Name, lbl.Pool, medium)
	}
	// Relabeled-since check: a tape we know whose epoch advanced means the catalog
	// is stale for it. (A genuinely different tape is not an error — that is what
	// loading another tape in the pool is for.)
	if known, ok := e.cat.Volume(lbl.Name); ok && known.Label.Epoch != lbl.Epoch {
		return "", 0, fmt.Errorf("volume %q was relabeled since the catalog was updated (epoch %d mounted vs %d cached); run `nb catalog rebuild`", lbl.Name, lbl.Epoch, known.Label.Epoch)
	}
	// One-run-per-tape media refuse to append onto a tape that already holds a run.
	if !appendable {
		if held := e.cat.SlotsOnVolume(lbl.Name); len(held) > 0 {
			return "", 0, reloadableErr("medium %q is not appendable and tape %q already holds %d run(s); load a fresh tape", medium, lbl.Name, len(held))
		}
	}
	if err := e.cat.RecordVolume(lbl); err != nil {
		return "", 0, err
	}
	return lbl.Name, lbl.Epoch, nil
}

// verifyWritableInteractive is verifyWritable plus the manual-station swap loop:
// on a single-drive (manual) medium whose loaded tape is unusable in a way a
// different reel would fix, it prompts the operator to swap and retries. Robotic
// libraries and unattended runs fall straight through to the underlying error.
func (e *Engine) verifyWritableInteractive(vol media.Volume, medium string, appendable bool, now time.Time, logf Logf) (string, int, error) {
	for {
		name, epoch, err := e.verifyWritable(vol, medium, appendable, now)
		if err == nil {
			return name, epoch, nil
		}
		mc, ok := vol.(media.ManualChanger)
		if !ok || !isReloadable(err) {
			return "", 0, err
		}
		if e.op == nil {
			return "", 0, fmt.Errorf("%v (load a writable tape into the drive and retry)", err)
		}
		reel, ok := e.promptSwap(mc, medium, "", err)
		if !ok {
			return "", 0, fmt.Errorf("%v (no tape loaded)", err)
		}
		logf.log("loading reel %s into the %q drive", reel, medium)
		if err := mc.Insert(reel); err != nil {
			return "", 0, err
		}
	}
}

// promptSwap asks the operator (via e.op) to pick a reel to load on a manual
// medium. need is the specific volume label wanted (reads) or "" (writes).
func (e *Engine) promptSwap(mc media.ManualChanger, medium, need string, cause error) (string, bool) {
	shelf, err := mc.Shelf()
	if err != nil {
		return "", false
	}
	var loaded media.BayStatus
	if bays, err := mc.Bays(); err == nil && len(bays) > 0 {
		loaded = bays[0]
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	return e.op.Swap(SwapRequest{Medium: medium, Pool: medium, Reason: reason, Need: need, Loaded: loaded, Shelf: shelf})
}

// assertVolume confirms the volume mounted on a medium matches a placement's
// recorded identity (label name + epoch), before reading from it.
func (e *Engine) assertVolume(vol media.Volume, p catalog.Placement) error {
	lv, ok := vol.(media.Labeled)
	if !ok {
		return nil // address-identified: identity is the medium itself
	}
	lbl, labeled, err := lv.ReadLabel()
	if err != nil {
		return err
	}
	if !labeled || lbl.Name != p.Volume || lbl.Epoch != p.Epoch {
		return fmt.Errorf("medium %q has volume %q (epoch %d) mounted, but slot copy is on %q (epoch %d) — mount it or run `nb catalog rebuild`",
			p.Medium, lbl.Name, lbl.Epoch, p.Volume, p.Epoch)
	}
	return nil
}

// mountForRead loads the bay holding a placement's volume on a changer medium
// (a tape library), so the reader can seek it. On disk-emulated tape this is
// automatic; a real manual changer would prompt the operator. Non-changer media
// are a no-op (a single addressable volume).
func (e *Engine) mountForRead(vol media.Volume, p catalog.Placement) error {
	if mc, ok := vol.(media.ManualChanger); ok {
		return e.mountManualForRead(mc, p)
	}
	ch, ok := vol.(media.Changer)
	if !ok {
		return nil
	}
	if bay, ok := ch.Loaded(); ok {
		if lbl, labeled, err := readVolumeLabel(vol); err == nil && labeled && lbl == p.Volume {
			return nil // the right tape is already in the drive
		}
		_ = bay
	}
	bays, err := ch.Bays()
	if err != nil {
		return err
	}
	for _, b := range bays {
		if b.Label == p.Volume {
			return ch.Mount(b.Bay)
		}
	}
	return fmt.Errorf("tape %q (holding a copy of the slot on %q) is not in the library; load it with `nb changer load %s <bay>`", p.Volume, p.Medium, p.Medium)
}

// mountManualForRead loads the reel a placement needs on a single-drive (manual)
// medium: if it is not already in the drive, it prompts the operator to swap it in
// (the bay's content changes), looping until the right tape is loaded or the
// operator aborts. Unattended, it returns an actionable error rather than blocking.
func (e *Engine) mountManualForRead(mc media.ManualChanger, p catalog.Placement) error {
	for {
		if lbl, labeled, err := readVolumeLabel(mc); err == nil && labeled && lbl == p.Volume {
			return nil // the needed reel is already in the drive
		}
		if e.op == nil {
			return fmt.Errorf("medium %q needs tape %q in the drive (a copy of the slot is on it); load it and retry", p.Medium, p.Volume)
		}
		reel, ok := e.promptSwap(mc, p.Medium, p.Volume, fmt.Errorf("need tape %q", p.Volume))
		if !ok {
			return fmt.Errorf("tape %q was not loaded into the %q drive", p.Volume, p.Medium)
		}
		if err := mc.Insert(reel); err != nil {
			return err
		}
	}
}

// readVolumeLabel reads the loaded volume's label name, if any. It accepts any
// medium handle (Volume or Changer view) and is a no-op for address-identified
// media that carry no label.
func readVolumeLabel(vol any) (name string, labeled bool, err error) {
	lv, ok := vol.(media.Labeled)
	if !ok {
		return "", false, nil
	}
	lbl, ok, err := lv.ReadLabel()
	return lbl.Name, ok, err
}

// readerFor opens a Reader positioned to read a placement's volume, after
// mounting the right tape (on a changer) and verifying its identity. For the
// engine's own medium it reuses the open handle and reader.
func (e *Engine) readerFor(p catalog.Placement) (*slotio.Reader, error) {
	if p.Medium == e.mediumName {
		if err := e.mountForRead(e.vol, p); err != nil {
			return nil, err
		}
		if err := e.assertVolume(e.vol, p); err != nil {
			return nil, err
		}
		return e.reader, nil
	}
	vol, _, _, err := e.mediumVolume(p.Medium)
	if err != nil {
		return nil, err
	}
	if err := e.mountForRead(vol, p); err != nil {
		return nil, err
	}
	if err := e.assertVolume(vol, p); err != nil {
		return nil, err
	}
	return slotio.NewReader(vol, e.fopts), nil
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

// LabelVolume writes (or rewrites) the identity label of a medium's volume — the
// deliberate operator act that makes a tape writable. It refuses to overwrite
// foreign data or a still-active volume unless forced; relabeling our own volume
// requires --relabel and bumps the epoch.
func (e *Engine) LabelVolume(mediumName, name string, relabel, force bool, now time.Time, logf Logf) error {
	vol, def, own, err := e.mediumVolume(mediumName)
	if err != nil {
		return err
	}
	minAge, _ := def.MinAge()
	lv, ok := vol.(media.Labeled)
	if !ok {
		return fmt.Errorf("medium %q is address-identified and does not use labels", mediumName)
	}

	// On a single-drive (manual) station there is no bay to choose: labeling acts
	// on whatever reel the operator has loaded into the drive. On a robotic library,
	// pick the physical bay this label belongs to and mount it — an existing tape
	// for --relabel, a blank one for a new label.
	if _, ok := vol.(media.ManualChanger); ok {
		// nothing to mount; the loaded reel (if any) is the target
	} else if ch, ok := vol.(media.Changer); ok {
		bay, err := chooseBay(ch, name, relabel)
		if err != nil {
			return err
		}
		if err := ch.Mount(bay); err != nil {
			return err
		}
	}

	cur, labeled, err := lv.ReadLabel()
	epoch := 1
	switch {
	case errors.Is(err, media.ErrForeignVolume):
		if !force {
			return fmt.Errorf("volume holds non-NBackup data; refusing to overwrite (use --force)")
		}
	case err != nil:
		return err
	case labeled:
		if !relabel {
			return fmt.Errorf("volume is already labeled %q (epoch %d); use --relabel to reuse it", cur.Name, cur.Epoch)
		}
		slots, serr := catalog.ScanSlots(vol)
		if serr != nil {
			return serr
		}
		if protected := policy.Protected(slots, minAge, now); len(protected) > 0 && !force {
			return fmt.Errorf("volume %q still holds %d protected slot(s); refusing to relabel (use --force)", cur.Name, len(protected))
		}
		epoch = cur.Epoch + 1
	}

	lbl := media.Label{Name: name, Pool: mediumName, Epoch: epoch, WrittenAt: now}
	if err := lv.WriteLabel(lbl); err != nil {
		return err
	}
	if got, ok, err := lv.ReadLabel(); err != nil || !ok || got.Name != name {
		return fmt.Errorf("label write could not be confirmed (read back %q, ok=%v, err=%v)", got.Name, ok, err)
	}
	logf.log("labeled %q as %q (epoch %d)", mediumName, name, epoch)

	// Relabeling the engine's own medium wipes the contents the catalog caches:
	// rebuild from the now-empty volume so the catalog reflects reality.
	if own {
		if _, err := e.cat.Rebuild(map[string]media.Volume{e.mediumName: e.vol}); err != nil {
			return fmt.Errorf("rebuild catalog after labeling: %w", err)
		}
	}
	return nil
}

// chooseBay selects which physical bay a label operation targets: for --relabel,
// the bay already holding that label; for a new label, a blank bay (preferring
// one already in the drive). It refuses to reuse an existing label without
// --relabel, or to label when no blank bay is free.
func chooseBay(ch media.Changer, name string, relabel bool) (string, error) {
	bays, err := ch.Bays()
	if err != nil {
		return "", err
	}
	if relabel {
		for _, b := range bays {
			if b.Label == name {
				return b.Bay, nil
			}
		}
		return "", fmt.Errorf("no tape labeled %q in the library to relabel", name)
	}
	for _, b := range bays {
		if b.Label == name {
			return "", fmt.Errorf("a tape labeled %q already exists; use --relabel to reuse it", name)
		}
	}
	if cur, ok := ch.Loaded(); ok {
		for _, b := range bays {
			if b.Bay == cur && b.Blank {
				return cur, nil // label the blank already in the drive
			}
		}
	}
	for _, b := range bays {
		if b.Blank {
			return b.Bay, nil
		}
	}
	return "", fmt.Errorf("no blank bay available; all %d are in use — relabel an aged-out tape with `nb label --relabel`", len(bays))
}

// Bays inventories a changer medium's library: the loaded bay and every bay's
// physical state plus the label it holds.
func (e *Engine) Bays(mediumName string) (loaded string, bays []media.BayStatus, err error) {
	vol, _, _, err := e.mediumVolume(mediumName)
	if err != nil {
		return "", nil, err
	}
	ch, ok := vol.(media.Changer)
	if !ok {
		return "", nil, fmt.Errorf("medium %q has no changer to inventory (it is addressed directly, not by loading tapes)", mediumName)
	}
	bays, err = ch.Bays()
	if err != nil {
		return "", nil, err
	}
	loaded, _ = ch.Loaded()
	return loaded, bays, nil
}

// LoadVolume mounts a volume on a changer medium, addressed by bay id, or by
// label when byLabel is set (the host-side "load the volume labeled X" helper).
func (e *Engine) LoadVolume(mediumName, target string, byLabel bool, logf Logf) error {
	vol, _, _, err := e.mediumVolume(mediumName)
	if err != nil {
		return err
	}
	// A single-drive (manual) station loads a reel from the room into its one drive.
	if mc, ok := vol.(media.ManualChanger); ok {
		return e.insertManual(mc, mediumName, target, byLabel, logf)
	}
	ch, ok := vol.(media.Changer)
	if !ok {
		return fmt.Errorf("medium %q has no changer to load (it is addressed directly, not by loading tapes)", mediumName)
	}
	bay := target
	if byLabel {
		bays, err := ch.Bays()
		if err != nil {
			return err
		}
		found := ""
		for _, b := range bays {
			if b.Label == target {
				found = b.Bay
				break
			}
		}
		if found == "" {
			return fmt.Errorf("no tape labeled %q in the library", target)
		}
		bay = found
	}
	if err := ch.Mount(bay); err != nil {
		return err
	}
	if lbl, labeled, _ := readVolumeLabel(vol); labeled {
		logf.log("loaded %q: bay %s holds %q", mediumName, bay, lbl)
	} else {
		logf.log("loaded %q: bay %s (blank)", mediumName, bay)
	}
	return nil
}

// insertManual loads a reel from a manual station's room into its single drive,
// addressed by reel id or (with byLabel) by the label it carries.
func (e *Engine) insertManual(mc media.ManualChanger, mediumName, target string, byLabel bool, logf Logf) error {
	shelf, err := mc.Shelf()
	if err != nil {
		return err
	}
	reel := ""
	for _, b := range shelf {
		if (byLabel && b.Label == target) || (!byLabel && b.Bay == target) {
			reel = b.Bay
			break
		}
	}
	if reel == "" {
		what := "reel"
		if byLabel {
			what = "tape labeled"
		}
		return fmt.Errorf("no %s %q in the %q room (it may already be in the drive)", what, target, mediumName)
	}
	if err := mc.Insert(reel); err != nil {
		return err
	}
	if lbl, labeled, _ := readVolumeLabel(mc); labeled {
		logf.log("loaded %q: reel %s holds %q", mediumName, reel, lbl)
	} else {
		logf.log("loaded %q: reel %s (blank)", mediumName, reel)
	}
	return nil
}

// Shelf returns the offline reels of a single-drive (manual) medium — those in the
// room but not in the drive — or nil for any other medium. Used by `nb changer
// list` to show the operator what is available to load.
func (e *Engine) Shelf(mediumName string) ([]media.BayStatus, error) {
	vol, _, _, err := e.mediumVolume(mediumName)
	if err != nil {
		return nil, err
	}
	mc, ok := vol.(media.ManualChanger)
	if !ok {
		return nil, nil
	}
	return mc.Shelf()
}

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
	return m.Restore(rc, destDir)
}

// openArchive opens an archive from any available copy, preferring the engine's
// own medium, trying each placement until one opens (restore fails over to a copy).
func (e *Engine) openArchive(slotID, dle string, level int, codec string) (io.ReadCloser, error) {
	placements := e.placementsFor(slotID)
	if len(placements) == 0 {
		return nil, fmt.Errorf("slot %s not in catalog (run `nb catalog rebuild`)", slotID)
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
