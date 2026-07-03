package engine

import (
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
)

// openWriter folds the engine's clerk/librarian write machinery into the medium-
// neutral conductor.PreparedWriter the run lane needs: it prepares the writer (the
// shared PrepareWrite→WriteSink→NewWriter contract), opens the run store over it,
// and reports whether the medium is serial and its capacity. It mirrors the calls
// runOrchestrated makes (clerk.OpenRun, Media.CapacityBytes) so the types line up —
// folding that machinery here keeps the conductor free of the clerk/librarian packages.
func (e *Engine) openWriter(medium string, spec archiveio.RunSpec, now time.Time, lf logf.Logf) (conductor.PreparedWriter, error) {
	stores, err := e.landingStores(medium, spec, now, lf)
	if err != nil {
		return conductor.PreparedWriter{}, err
	}
	capB, _ := e.cfg.Media[medium].CapacityBytes()
	return conductor.PreparedWriter{
		Stores: stores,
		Lim:    e.dep.limiter(medium),
		// Serial keys off the concurrent-write capability: a serial medium (tape) shares one
		// rolling drive per store and writes one archive at a time on it, while a concurrent-write
		// object store/disk writes archives as independent objects/files and stays parallel — even
		// when it splits a large archive into parts. So part_size on cloud never clamps workers.
		Serial:   !media.ConcurrentWrite(e.cfg.Media[medium].Type),
		Capacity: capB,
		Writers:  e.cfg.Media[medium].Writers,
	}, nil
}

// openReader is the run window's read face: a read-only clerk over the window's
// catalog.View (the committed placements as of window-open). The View's copy means a
// reader and the window's writer never share the live entries; which media a read may
// MOUNT is the media layer's business — MounterFor refuses a window-written medium,
// so copy selection fails over past such a placement like any unavailable copy.
// Serving reads from a point-in-time view is sound because a session never reads its
// own writes through the catalog: everything written inside the window belongs to
// this run, everything read (a copy's source placements) was recorded by a previous
// one, and the one same-run read-back — a drain reopening a staged archive — travels
// by value in CommitResult, not through the catalog.
func (e *Engine) openReader(view *catalog.View) archiveio.ReadStore {
	m := readView{view: view, own: e.dep.landingName}
	return clerk.New(m, clerkDeps{e}, catalog.OpenMemberIndex(e.cfg.WorkdirPath()))
}

// readView is the window clerk's ReadMap: the View's placements in the usual
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

// landingStores opens the run stores a landing is written through: one per tape drive for a robotic
// multi-drive library (each a lazy per-drive sink that loads its own tape on first write, so the
// concurrent writers land on independent drives), or a single store for a single-drive tape, a manual
// drive, or a directly-addressed medium (disk/cloud — its own concurrency is independent files).
func (e *Engine) landingStores(medium string, spec archiveio.RunSpec, now time.Time, lf logf.Logf) ([]archiveio.Store, error) {
	lib, def, _, err := e.dep.librarianFor(medium)
	if err != nil {
		return nil, err
	}
	if !lib.Parallel() {
		wt, err := e.prepareWriter(medium, spec, now, lf) // eager single store (existing contract)
		if err != nil {
			return nil, err
		}
		return []archiveio.Store{wt.session}, nil
	}
	partSize, err := e.dep.partSizeFor(medium)
	if err != nil {
		return nil, err
	}
	exp := e.acct.ExpectedVolumeFor(medium, now)
	announceExpectation(medium, exp, lf)
	sinks := lib.LazyDriveSinks(def.IsAppendable(), exp.Label, partSize, now, librarian.Logf(lf))
	stores := make([]archiveio.Store, len(sinks))
	for i, sink := range sinks {
		stores[i] = e.clerk.OpenRun(sink, e.cat, medium, lib.Volume(), spec.ID)
	}
	return stores, nil
}

// landingFor resolves the medium a DLE lands on: its dumptype's `landing` override, else the run's
// default landing (e.mediumName). The override is validated against `media` at config load, so a
// resolve error or empty result falls back to the default.
func (e *Engine) landingFor(d config.DLE) string {
	if name, err := e.cfg.LandingFor(d); err == nil && name != "" {
		return name
	}
	return e.dep.landingName
}

// landingForDLEName resolves the landing of a DLE named by its catalog slug (DLE.Name()) — what a
// staged archive's placement carries — for the crash-recovery flush. A DLE no longer in config (the
// config changed since the crash) drains to the default landing.
func (e *Engine) landingForDLEName(slug string) string {
	for _, d := range e.cfg.DLEs() {
		if d.Name() == slug {
			return e.landingFor(d)
		}
	}
	return e.dep.landingName
}

// newConductor wires a per-run conductor.Conductor to the engine's dumper, plan
// lane, landing volume, and write/flush machinery. Plan binds to the scheduler's
// method (not the engine's own planWith) so the run lane reads its plan from the
// scheduler. The engine's Backup/PlannedRunID methods build one of these per run
// and delegate to it (see internal/conductor).
func (e *Engine) newConductor() *conductor.Conductor {
	return conductor.New(conductor.Deps{
		Cat:               e.cat,
		Dmp:               e.dmp,
		Plan:              e.sched.Plan,
		Vol:               e.dep.vol,
		OpenWriter:        e.openWriter,
		OpenReader:        e.openReader,
		ClaimWrites:       e.dep.claimWrites,
		CheckCompress:     e.tc.checkCompress,
		ProbeReachable:    e.tc.probeReachable,
		PreflightDumptype: e.tc.preflightDumptype,
		Flush:             e.Flush,
		HoldingMedia:      e.cfg.HoldingMedia(),
		Workers:           e.cfg.Workers(),
		NewFileSink:       func() progress.Sink { return progress.NewFileSink(e.cfg.WorkdirPath(), time.Now) },
		Landing:           e.dep.landingName,
		LandingFor:        func(it planner.Item) string { return e.landingFor(it.DLE) },
		RunSink:           e.runSink,
		EstimateSink:      e.estimateSink,
	})
}
