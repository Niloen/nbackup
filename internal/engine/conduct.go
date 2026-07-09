package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/depot"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// openWriter folds the engine's fs/librarian write machinery into the medium-
// neutral conductor.PreparedWriter the run lane needs: it prepares the writer (the
// shared PrepareWrite→WriteSink→NewWriter contract), opens the run store over it,
// and reports whether the medium is serial and its capacity. It mirrors the calls
// runOrchestrated makes (archivefs.OpenRun, Media.CapacityBytes) so the types line up —
// folding that machinery here keeps the conductor free of the archivefs/librarian implementation wiring.
func (e *Engine) openWriter(medium string, spec archiveio.RunSpec, now time.Time, lf logf.Logf) (conductor.PreparedWriter, error) {
	allocs, store, wm, err := e.landingSeams(medium, spec, now, lf)
	if err != nil {
		return conductor.PreparedWriter{}, err
	}
	capB, _ := e.cfg.Media[medium].CapacityBytes()
	return conductor.PreparedWriter{
		Allocs:  allocs,
		Store:   store,
		Lim:     e.dep.Limiter(medium),
		Release: func() { _ = wm.Close() },
		// Serial keys off the concurrent-write capability: a serial medium (tape) shares one
		// rolling drive per store and writes one archive at a time on it, while a concurrent-write
		// object store/disk writes archives as independent objects/files and stays parallel — even
		// when it splits a large archive into parts. So part_size on cloud never clamps workers.
		Serial:   !media.ConcurrentWrite(e.cfg.Media[medium].Type),
		Capacity: capB,
		Writers:  e.cfg.Media[medium].Writers,
	}, nil
}

// openReader is the run window's read face: a read-only fs over the window's
// catalog.View (the committed placements as of window-open). The View's copy means a
// reader and the window's writer never share the live entries; which media a read may
// MOUNT is the media layer's business — MounterFor refuses a window-written medium,
// so copy selection fails over past such a placement like any unavailable copy.
// Serving reads from a point-in-time view is sound because a session never reads its
// own writes through the catalog: everything written inside the window belongs to
// this run, everything read (a copy's source placements) was recorded by a previous
// one, and the one same-run read-back — a drain reopening a staged archive — travels
// by value in CommitResult, not through the catalog.
func (e *Engine) openReader(view *catalog.View) archivefs.ReadStore {
	m := readView{view: view, own: e.dep.LandingName()}
	return archivefs.New(m, fsDeps{e}, catalog.OpenMemberIndex(e.cfg.WorkdirPath()))
}

// readView is the window fs's ReadMap: the View's placements in the usual
// read-preference order (the engine's own medium first, see placementsFor). It has no
// write methods — the window's read face is read-only by type.
type readView struct {
	view *catalog.View
	own  string
}

func (m readView) PlacementsFor(runID string) []catalog.Placement {
	all := m.view.PlacementsFor(runID)
	ordered := append([]catalog.Placement(nil), all...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Medium == m.own && ordered[j].Medium != m.own
	})
	return ordered
}

// landingSeams opens the two write seams a landing is written through: the part allocators —
// one per tape drive for a robotic multi-drive library (each a lazy per-drive allocator that
// loads its own tape on first write, so the concurrent writers land on independent drives), or a
// single one for a single-drive tape, a manual drive, or a directly-addressed medium (disk/cloud
// — its own concurrency is independent files) — and the medium's one run store (the fs Session
// every writer records through). The one OpenForWrite here takes the window's claim on the
// medium; the returned handle's Close (wired into PreparedWriter.Release) gives it back at
// window end.
func (e *Engine) landingSeams(medium string, spec archiveio.RunSpec, now time.Time, lf logf.Logf) ([]archiveio.PartAllocator, archivefs.WriteStore, depot.WriteMedium, error) {
	wm, def, err := e.dep.OpenForWrite(medium)
	if err != nil {
		return nil, nil, nil, err
	}
	if !wm.Parallel() {
		wt, err := e.prepareWriterOn(wm, def, spec, now, lf) // eager single allocator (existing contract)
		if err != nil {
			_ = wm.Close()
			return nil, nil, nil, err
		}
		return []archiveio.PartAllocator{wt.alloc}, wt.session, wm, nil
	}
	partSize, exp, err := e.writePrelude(medium, now, lf)
	if err != nil {
		_ = wm.Close()
		return nil, nil, nil, err
	}
	lazyAllocs := wm.LazyDriveAllocators(def.IsAppendable(), exp.Label, partSize, now, librarian.Logf(lf))
	allocs := make([]archiveio.PartAllocator, len(lazyAllocs))
	for i, a := range lazyAllocs {
		allocs[i] = a
	}
	return allocs, e.fs.OpenRun(e.cat, wm), wm, nil
}

// landingsFor resolves the media a DLE lands on, primary first: its dumptype's `landing` override,
// else the run's default landing route. The route is validated against `media` at config load, so a
// resolve error or empty result falls back to the default landing. Keyed by the dumptype name —
// a resolved (partition-derived) DLE's whole route is its dumptype's, and its slug is not in config.
func (e *Engine) landingsFor(dumptype string) []string {
	if names, err := e.cfg.LandingsForDumptype(dumptype); err == nil && len(names) > 0 {
		return names
	}
	return []string{e.dep.LandingName()}
}

// landingFor is a DLE's primary landing — the accounted medium — for the single-landing consumers.
func (e *Engine) landingFor(d config.DLE) string {
	return e.landingsFor(d.DumpTypeName())[0]
}

// atomCeilingErr is the dump-time half of the atom validation ladder: a hard error
// when an atomic dumptype's atoms exceed ANY routed landing's part ceiling — a sealed
// atom can never be re-cut to fit, so the dump must refuse rather than write archives
// no copy could carry there (a fan-out writes every landing on the route, so each
// must carry the atom). The check-time sibling (checkAtomShapes) warns about
// every dumptype × medium pair; this fires only for the pairs actually routed.
func (e *Engine) atomCeilingErr(dumpType string, atomSize int64) error {
	landings := e.cfg.ResolveDumpType(dumpType).Landing
	if len(landings) == 0 {
		landings = e.cfg.Landing
	}
	for _, landing := range landings {
		ceiling := media.PartSizeFor(e.cfg.Media[landing].Type).Max
		if ceiling > 0 && atomSize > ceiling {
			return fmt.Errorf("dumptype %q: its %s atoms (part_size) exceed landing %q's %s part ceiling and a sealed atom cannot be re-cut — lower the dumptype's part_size or route it to a medium with a higher ceiling",
				dumpType, sizeutil.FormatBytes(atomSize), landing, sizeutil.FormatBytes(ceiling))
		}
	}
	return nil
}

// landingsForDLEName resolves the landing route of a DLE named by its catalog slug (DLE.Name()) —
// what a staged archive's placement carries — for the crash-recovery flush. A DLE no longer in
// config (the config changed since the crash) drains to the default primary landing.
func (e *Engine) landingsForDLEName(slug string) []string {
	for _, d := range e.cfg.DLEs() {
		if d.Name() == slug {
			return e.landingsFor(d.DumpTypeName())
		}
	}
	return []string{e.dep.LandingName()}
}

// newConductor wires a per-run conductor.Conductor to the engine's dumper, plan
// lane, landing volume, and write/flush machinery. Plan binds to the scheduler's
// method (not the engine's own planWith) so the run lane reads its plan from the
// scheduler. The engine's Run/PlannedRunID methods build one of these per run
// and delegate to it (see internal/conductor).
func (e *Engine) newConductor() *conductor.Conductor {
	return conductor.New(conductor.Deps{
		Cat:          e.cat,
		Dmp:          e.dmp,
		Plan:         e.sched.Plan,
		OpenWriter:   e.openWriter,
		OpenReader:   e.openReader,
		Preflight:    e.preflightDeps(),
		MakeRoom:     e.acct.MakeRoom,
		Flush:        e.Flush,
		HoldingMedia: e.cfg.HoldingMedia(),
		Workers:      e.cfg.Workers(),
		NewFileSink:  func() progress.Sink { return progress.NewFileSink(e.cfg.WorkdirPath(), time.Now) },
		LandingsFor:  func(it planner.Item) []string { return e.landingsFor(it.DLE.DumpTypeName()) },
		RunSink:      e.runSink,
		EstimateSink: e.estimateSink,
	})
}

// MakeRoomForecast previews the pre-write reclamation a dump of plan would run,
// per landing medium — `nb plan`'s window into capacity-as-a-promise: what
// tonight's run costs in history (the archives make-room will reclaim), or the
// fail-loud infeasibility the dump would refuse with.
type MakeRoomForecast struct {
	Medium   string
	Incoming int64 // the plan's estimated bytes routed to this landing
	Freed    int64 // bytes make-room would reclaim (0 = fits as-is)
	Archives int   // archives those bytes come from
	Err      error // non-nil: the dump would refuse (protected set + incoming exceed capacity)
}

// MakeRoomForecasts routes the plan's estimates to their landings (the same
// routing the conductor uses) and previews each landing's make-room step.
func (e *Engine) MakeRoomForecasts(plan *planner.Plan, now time.Time) []MakeRoomForecast {
	incoming := map[string]int64{}
	var order []string
	for _, item := range plan.Items {
		for _, landing := range e.landingsFor(item.DLE.DumpTypeName()) {
			if _, seen := incoming[landing]; !seen {
				order = append(order, landing)
			}
			incoming[landing] += item.EstBytes
		}
	}
	var out []MakeRoomForecast
	for _, landing := range order {
		freed, n, err := e.acct.MakeRoomPreview(landing, incoming[landing], now)
		out = append(out, MakeRoomForecast{Medium: landing, Incoming: incoming[landing], Freed: freed, Archives: n, Err: err})
	}
	return out
}
