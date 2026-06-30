package engine

import (
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/transform/compress"
)

// openWriter folds the engine's clerk/librarian write machinery into the medium-
// neutral conductor.PreparedWriter the run lane needs: it prepares the writer (the
// shared PrepareWrite→WriteSink→NewWriter contract), opens the slot store over it,
// and reports whether the medium is serial and its capacity. It mirrors the calls
// runOrchestrated makes (clerk.OpenSlot, Media.CapacityBytes) so the types line up —
// folding that machinery here keeps the conductor free of the clerk/librarian packages.
func (e *Engine) openWriter(medium string, spec archiveio.SlotSpec, now time.Time, lf logf.Logf) (conductor.PreparedWriter, error) {
	wt, err := e.prepareWriter(medium, spec, now, lf)
	if err != nil {
		return conductor.PreparedWriter{}, err
	}
	capB, _ := e.cfg.Media[medium].CapacityBytes()
	return conductor.PreparedWriter{
		Store: e.clerk.OpenSlot(wt.w, medium, wt.lib.Volume()),
		// Serial keys off the concurrent-write capability: a serial medium (tape) shares one
		// rolling drive and writes one archive at a time, while a concurrent-write object
		// store/disk writes archives as independent objects/files and stays parallel — even
		// when it splits a large archive into parts. So part_size on cloud never clamps workers.
		Serial:   !media.ConcurrentWrite(e.cfg.Media[medium].Type),
		Capacity: capB,
	}, nil
}

// landingFor resolves the medium a DLE lands on: its dumptype's `landing` override, else the run's
// default landing (e.mediumName). The override is validated against `media` at config load, so a
// resolve error or empty result falls back to the default.
func (e *Engine) landingFor(d config.DLE) string {
	if name, err := e.cfg.LandingFor(d); err == nil && name != "" {
		return name
	}
	return e.mediumName
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
	return e.mediumName
}

// newConductor wires a per-run conductor.Conductor to the engine's dumper, plan
// lane, landing volume, and write/flush machinery. Plan binds to the scheduler's
// method (not the engine's own planWith) so the run lane reads its plan from the
// scheduler. The engine's Backup/PlannedSlotID methods build one of these per run
// and delegate to it (see internal/conductor).
func (e *Engine) newConductor() *conductor.Conductor {
	return conductor.New(conductor.Deps{
		Cat:               e.cat,
		Dmp:               e.dmp,
		Plan:              e.sched.Plan,
		Vol:               e.vol,
		OpenWriter:        e.openWriter,
		CheckCompress:     func() error { return compress.Check(e.compressScheme, e.fopts) },
		ProbeReachable:    e.probeReachable,
		PreflightDumptype: e.preflightDumptype,
		Flush:             e.Flush,
		HoldingMedia:      e.cfg.HoldingMedia(),
		Workers:           e.cfg.Workers(),
		NewFileSink:       func() progress.Sink { return progress.NewFileSink(e.cfg.WorkdirPath(), time.Now) },
		Landing:           e.mediumName,
		LandingFor:        func(it planner.Item) string { return e.landingFor(it.DLE) },
		RunSink:           e.runSink,
		EstimateSink:      e.estimateSink,
	})
}
