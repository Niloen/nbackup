package engine

import (
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/transform/compress"
)

// openWriter folds the engine's clerk/librarian write machinery into the medium-
// neutral conductor.PreparedWriter the run lane needs: it prepares the writer (the
// shared PrepareWrite→WriteSink→NewWriter contract), opens the slot store over it,
// and reports the medium's spanning capability and capacity. It mirrors the calls
// runOrchestrated makes (clerk.OpenSlot, lib.CanSpan, Media.CapacityBytes) so the
// types line up — folding that machinery here keeps the conductor free of the
// clerk/librarian packages.
func (e *Engine) openWriter(medium string, spec archiveio.SlotSpec, now time.Time, lf logf.Logf) (conductor.PreparedWriter, error) {
	wt, err := e.prepareWriter(medium, spec, now, lf)
	if err != nil {
		return conductor.PreparedWriter{}, err
	}
	capB, _ := e.cfg.Media[medium].CapacityBytes()
	return conductor.PreparedWriter{
		Store:    e.clerk.OpenSlot(wt.w, medium, wt.lib.Volume()),
		CanSpan:  wt.lib.CanSpan(wt.partSize),
		Capacity: capB,
	}, nil
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
		RunSink:           e.runSink,
		EstimateSink:      e.estimateSink,
	})
}
