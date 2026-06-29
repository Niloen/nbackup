// Package conductor is NBackup's run lane: it executes one plan into one sealed
// slot — flushing leftovers, pre-flighting tools, opening the landing writer,
// running the producers, and draining onto the landing. It is the dump
// orchestration the engine used to do inline (Run/runOrchestrated), split out
// behind a narrow dependency slice. The methods are stubs in this commit (the
// engine still does the real work); a later lane fills them in.
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

// Conductor holds the slice of the orchestrator a single run needs. It is
// per-run (it carries the run's open landing volume and progress sinks), built
// fresh each Run; the closures bind to the engine's own machinery.
type Conductor struct {
	cat               *catalog.Catalog
	dmp               *dumper.Dumper
	plan              func(date time.Time, sink progress.Sink) *planner.Plan
	vol               media.Volume
	openWriter        func(medium string, spec archiveio.SlotSpec, now time.Time, lf logf.Logf) (PreparedWriter, error)
	checkCompress     func() error
	probeReachable    func(host string) error
	preflightDumptype func(dt, host string, checkArchiver bool, checked map[string]bool) error
	flush             func(now time.Time, lf logf.Logf) (int, error)
	holdingMedia      func() []string
	workers           func() int
	newFileSink       func() progress.Sink
	landing           string
	runSink           progress.Sink
	estimateSink      progress.Sink
}

// Deps is the exported mirror of the Conductor's dependency slice.
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
	HoldingMedia      func() []string
	Workers           func() int
	NewFileSink       func() progress.Sink
	Landing           string
	RunSink           progress.Sink
	EstimateSink      progress.Sink
}

// New constructs a Conductor from its dependencies.
func New(d Deps) *Conductor {
	return &Conductor{
		cat:               d.Cat,
		dmp:               d.Dmp,
		plan:              d.Plan,
		vol:               d.Vol,
		openWriter:        d.OpenWriter,
		checkCompress:     d.CheckCompress,
		probeReachable:    d.ProbeReachable,
		preflightDumptype: d.PreflightDumptype,
		flush:             d.Flush,
		holdingMedia:      d.HoldingMedia,
		workers:           d.Workers,
		newFileSink:       d.NewFileSink,
		landing:           d.Landing,
		runSink:           d.RunSink,
		estimateSink:      d.EstimateSink,
	}
}

// Run executes the plan for a date, producing one sealed slot.
func (c *Conductor) Run(now time.Time, lf logf.Logf) (*catalog.Slot, error) {
	panic("conductor: not yet wired")
}

// PlannedSlotID returns the slot id a real dump on date would seal next.
func (c *Conductor) PlannedSlotID(date time.Time) string {
	panic("conductor: not yet wired")
}
