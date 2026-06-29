package engine

import (
	"os"

	"github.com/Niloen/nbackup/internal/scheduler"
	"github.com/Niloen/nbackup/internal/transform/compress"
)

// newScheduler wires a scheduler.Scheduler to the engine's config, catalog history,
// archiver resolution, and pre-flight checks. The engine's plan/estimate/validate
// methods are thin pass-throughs to it (see internal/scheduler).
func (e *Engine) newScheduler() *scheduler.Scheduler {
	return scheduler.New(scheduler.Deps{
		DLEs:              e.cfg.DLEs,
		History:           e.cat.History,
		ForcedFulls:       e.cat.ForcedFulls,
		Workers:           e.cfg.Workers,
		ArchiverFor:       e.archiverFor,
		ExcludeFor:        func(dt string) []string { return e.cfg.ResolveDumpType(dt).Exclude },
		CycleDays:         e.cfg.CycleDays,
		BumpPercent:       e.cfg.BumpPercent,
		Capacity:          e.profile.TotalBytes,
		CapacityRoom:      e.capacityRoom,
		CompressCheck:     func() error { return compress.Check(e.compressScheme, e.fopts) },
		PreflightDumptype: e.preflightDumptype,
		RemoteHost:        e.cfg.RemoteHost,
		StatSource:        func(p string) error { _, err := os.Stat(p); return err },
		ProbeReachable:    e.probeReachable,
	})
}
