// Package conductor is NBackup's run lane: it executes one plan into one sealed
// slot — flushing leftovers, pre-flighting tools, opening the landing writer,
// running the producers, and draining onto the landing. It is the dump
// orchestration the engine used to do inline (Run/runOrchestrated), split out
// behind a narrow dependency slice. The engine's Backup/PlannedSlotID methods build
// a fresh Conductor per run and delegate to it.
package conductor

import (
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/dumper"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
)

// PreparedWriter is the folded view of a medium opened for writing: the slot store
// the producers author into, whether the medium can span volumes (so the caller can
// clamp parallelism), and the medium's capacity in bytes. The engine builds it from
// its clerk/librarian machinery so the conductor stays free of those packages.
type PreparedWriter struct {
	Store    archiveio.ArchiveStore
	CanSpan  bool
	Capacity int64
}

// Deps is the slice of the orchestrator a single run needs. The closures bind to the
// engine's own machinery; Workers and HoldingMedia are static run config read once.
// It is exported so the engine can wire one; the Conductor embeds it directly.
type Deps struct {
	Cat               *catalog.Catalog
	Dmp               *dumper.Dumper
	Plan              func(date time.Time, sink progress.Sink) *planner.Plan
	Vol               media.Volume
	OpenWriter        func(medium string, spec archiveio.SlotSpec, now time.Time, lf logf.Logf) (PreparedWriter, error)
	CheckCompress     func() error
	ProbeReachable    func(host string) error
	PreflightDumptype func(dt, host string, checkArchiver bool, checked map[string]bool) error
	Flush             func(now time.Time, lf logf.Logf) (int, error)
	HoldingMedia      []string
	Workers           int
	NewFileSink       func() progress.Sink
	Landing           string
	RunSink           progress.Sink
	EstimateSink      progress.Sink
}

// Conductor executes one plan into one sealed slot. It is per-run (it carries the
// run's open landing volume and progress sinks via Deps), built fresh each Run.
type Conductor struct{ d Deps }

// New constructs a Conductor from its dependencies.
func New(d Deps) *Conductor { return &Conductor{d: d} }
