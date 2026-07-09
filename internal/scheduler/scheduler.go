// Package scheduler is NBackup's planning lane: it turns the configured DLEs, the
// catalog's run history, and the per-run capacity room into a planner.Plan — the
// level schedule a run executes and a preview (`nb plan`, `nb dump --dry-run`)
// shows. It is the engine-side DRIVER that feeds the pure `planner` algorithm its
// config/history/capacity inputs and the parallel size estimates, then applies the
// impure force-full post-pass the planner cannot do alone (it must probe the
// archiver's on-disk incremental state). The engine's plan/estimate/validate methods
// are thin pass-throughs to it.
package scheduler

import (
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
)

// Deps is the slice of the orchestrator the planner needs: the inputs are closures
// the engine binds to its own config/catalog/archiver resolution, so the scheduler
// never reaches into the engine directly. It is exported so the engine can wire one;
// the Scheduler embeds it directly.
type Deps struct {
	DLEs          func() []config.DLE
	History       func() *catalog.History
	ForcedFulls   func() map[string]bool
	Workers       func() int
	ArchiverFor   func(dtName, host string) (archiver.Archiver, error)
	ExcludeFor    func(dtName string) []string
	CycleDays     func() int
	BumpPercent   func() float64
	Capacity      func() int64
	CapacityRoom  func(now time.Time) int64
	PreflightDeps // the dump pre-flight closures, shared with the conductor's strict pre-flight
}

// Scheduler drives the planner: it estimates DLE sizes, builds and forecasts plans,
// and validates a run's config before previewing it.
type Scheduler struct{ d Deps }

// New constructs a Scheduler from its dependencies.
func New(d Deps) *Scheduler { return &Scheduler{d: d} }
