package engine

import (
	"os"

	"github.com/Niloen/nbackup/internal/scheduler"
)

// newScheduler wires a scheduler.Scheduler to the engine's config, catalog history,
// archiver resolution, and pre-flight checks. The engine's plan/estimate/validate
// methods are thin pass-throughs to it (see internal/scheduler).
func (e *Engine) newScheduler() *scheduler.Scheduler {
	return scheduler.New(scheduler.Deps{
		DLEs:          e.cfg.DLEs,
		History:       e.cat.History,
		ForcedFulls:   e.cat.ForcedFulls,
		Workers:       e.cfg.Workers,
		ArchiverFor:   e.tc.archiverFor,
		ExcludeFor:    func(dt string) []string { return e.cfg.ResolveDumpType(dt).Exclude },
		CycleDays:     e.cfg.CycleDays,
		BumpPercent:   e.cfg.BumpPercent,
		Capacity:      e.dep.Profile().TotalBytes,
		CapacityRoom:  e.acct.CapacityRoom,
		LastCarves:    e.lastCarves,
		PreflightDeps: e.preflightDeps(),
	})
}

// lastCarves returns the carve set the DLE's most recent archive was dumped with —
// the recorded half of the partition re-baseline comparison (scheduler.Deps.LastCarves).
// Runs() is in run order, so the scan walks newest-first.
func (e *Engine) lastCarves(dle string) ([]string, bool) {
	runs := e.cat.Runs()
	for i := len(runs) - 1; i >= 0; i-- {
		for _, a := range runs[i].Archives {
			if a.DLE == dle {
				return a.Carves, true
			}
		}
	}
	return nil, false
}

// preflightDeps wires the shared dump pre-flight (scheduler.Preflight) to the
// engine's toolchain and config — one value handed to both the scheduler's
// preview and the conductor's strict run pre-flight.
func (e *Engine) preflightDeps() scheduler.PreflightDeps {
	return scheduler.PreflightDeps{
		CheckCompress:     e.tc.checkCompress,
		PreflightDumptype: e.tc.preflightDumptype,
		RemoteHost:        e.cfg.RemoteHost,
		StatSource:        func(p string) error { _, err := os.Stat(p); return err },
		SourceIsPath:      e.tc.sourceIsPath,
		ProbeReachable:    e.tc.probeReachable,
	}
}
