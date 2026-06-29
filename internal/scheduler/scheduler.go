// Package scheduler is NBackup's planning lane: it turns the configured DLEs, the
// catalog's run history, and the per-run capacity room into a planner.Plan — the
// level schedule a run executes and a preview (`nb plan`, `nb dump --dry-run`)
// shows. It is the estimate/plan/validate arithmetic the engine used to do inline,
// split out behind a narrow dependency slice. The methods are stubs in this commit
// (the engine still does the real work); a later lane fills them in.
package scheduler

import (
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
)

// Scheduler holds the slice of the orchestrator the planner needs: the inputs are
// closures the engine binds to its own config/catalog/archiver resolution, so the
// scheduler never reaches into the engine directly.
type Scheduler struct {
	dles              func() []config.DLE
	history           func() *catalog.History
	forcedFulls       func() map[string]bool
	workers           func() int
	archiverFor       func(dtName, host string) (archiver.Archiver, error)
	excludeFor        func(dtName string) []string
	cycleDays         func() int
	bumpPercent       func() float64
	capacity          func() int64
	capacityRoom      func(now time.Time) int64
	compressCheck     func() error
	preflightDumptype func(dt, host string, checkArchiver bool, checked map[string]bool) error
	remoteHost        func(host string) (config.SSHConfig, bool)
	statSource        func(path string) error
	probeReachable    func(host string) error
}

// Deps is the exported mirror of the Scheduler's dependency slice.
type Deps struct {
	DLEs              func() []config.DLE
	History           func() *catalog.History
	ForcedFulls       func() map[string]bool
	Workers           func() int
	ArchiverFor       func(dtName, host string) (archiver.Archiver, error)
	ExcludeFor        func(dtName string) []string
	CycleDays         func() int
	BumpPercent       func() float64
	Capacity          func() int64
	CapacityRoom      func(now time.Time) int64
	CompressCheck     func() error
	PreflightDumptype func(dt, host string, checkArchiver bool, checked map[string]bool) error
	RemoteHost        func(host string) (config.SSHConfig, bool)
	StatSource        func(path string) error
	ProbeReachable    func(host string) error
}

// New constructs a Scheduler from its dependencies.
func New(d Deps) *Scheduler {
	return &Scheduler{
		dles:              d.DLEs,
		history:           d.History,
		forcedFulls:       d.ForcedFulls,
		workers:           d.Workers,
		archiverFor:       d.ArchiverFor,
		excludeFor:        d.ExcludeFor,
		cycleDays:         d.CycleDays,
		bumpPercent:       d.BumpPercent,
		capacity:          d.Capacity,
		capacityRoom:      d.CapacityRoom,
		compressCheck:     d.CompressCheck,
		preflightDumptype: d.PreflightDumptype,
		remoteHost:        d.RemoteHost,
		statSource:        d.StatSource,
		probeReachable:    d.ProbeReachable,
	}
}

// Plan builds the plan for a run date, with an optional live sink for the
// (potentially slow) estimate phase.
func (s *Scheduler) Plan(date time.Time, sink progress.Sink) *planner.Plan {
	panic("scheduler: not yet wired")
}

// Validate checks each DLE the way a real run would resolve it.
func (s *Scheduler) Validate() (warnings []string, err error) {
	panic("scheduler: not yet wired")
}

// Simulate forecasts the next `days` daily runs from `start` without writing.
func (s *Scheduler) Simulate(start time.Time, days int) []*planner.Plan {
	panic("scheduler: not yet wired")
}
