// Package conductor is NBackup's run lane: it executes one plan into one sealed
// run — flushing leftovers, pre-flighting tools, opening the landing writer,
// running the producers, and draining onto the landing. It is the dump
// orchestration the engine used to do inline (Run/runOrchestrated), split out
// behind a narrow dependency slice. The engine's Backup/PlannedRunID methods build
// a fresh Conductor per run and delegate to it.
package conductor

import (
	"time"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/dumper"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/scheduler"
)

// PreparedWriter is the folded view of a medium opened for writing: the run store the
// producers author into, whether the medium writes serially (which decides parallelism),
// and the medium's capacity in bytes. The engine builds it from its archivefs/librarian
// machinery so the conductor stays free of those packages.
//
// Serial means a single physical drive that rolls one shared volume (tape): only one
// archive can be on it at a time, so a direct (unbuffered) run is clamped to one worker
// and its landing writes serialized. A concurrent-write medium (disk, cloud) is not serial —
// it writes archives as independent objects/files and stays fully parallel, even when it
// splits a large archive into parts. Whether an archive is split is the writer's concern,
// not the conductor's, so it does not appear here.
type PreparedWriter struct {
	// Allocs is one part allocator per concurrent writer the medium supports: a single allocator
	// for a single-drive tape or a directly-addressed medium, or one per drive for a robotic
	// multi-drive library (each bound to its own drive so two archives write independent tapes). A
	// concurrent-write medium (disk, cloud) has one allocator shared by all its writers
	// (independent files, orchestrator-serialised control); a serial multi-drive one has a
	// distinct allocator per drive.
	Allocs []archiveio.PartAllocator
	// Store is the medium's run store (the fs Session): the Recorder every writer's commits are
	// recorded through — one per medium regardless of drive count — plus the drain's read-back
	// and reclaim on a holding disk.
	Store    archivefs.WriteStore
	Serial   bool
	Capacity int64
	Lim      *ratelimit.Limiter // the medium's byte-rate cap; the spool authors its concurrent writers with it
	// Writers is the medium's write-concurrency cap (its `writers` key; 0 = unset). It bounds
	// every write to the medium the same — direct dumps and drains alike. Unset, the medium's
	// natural width applies: its drive count when serial, else the run's worker count.
	Writers int
	// Release gives back the medium's write claim (taken when the writer was opened).
	// withSpool defers it to window end, so the claim spans exactly the window — a
	// read-mount onto the medium is refused for that duration and fails over. Nil-safe
	// for tests that fake a PreparedWriter.
	Release func()
}

// Deps is the slice of the orchestrator a single run needs. The closures bind to the
// engine's own machinery; Workers and HoldingMedia are static run config read once.
// It is exported so the engine can wire one; the Conductor embeds it directly.
type Deps struct {
	Cat        *catalog.Catalog
	Dmp        *dumper.Dumper
	Plan       func(date time.Time, sink progress.Sink) *planner.Plan
	OpenWriter func(medium string, spec archiveio.RunSpec, now time.Time, lf logf.Logf) (PreparedWriter, error)
	// OpenReader returns the read face of the archive fs over the run window's catalog
	// View. withSpool calls it once at window-open — while no concurrent writer exists
	// yet — and hands it to the run closure: inside the window the run mutates the live
	// catalog; the closure reads the View's copy (sound because a session never reads
	// its own writes). Which media it may mount is the media layer's business: a mount
	// onto a window-written medium is refused and the read fails over to another copy.
	OpenReader   func(view *catalog.View) archivefs.ReadStore
	Preflight    scheduler.PreflightDeps // the shared dump pre-flight closures (run strict here)
	Flush        func(now time.Time, lf logf.Logf) (int, error)
	HoldingMedia []string
	Workers      int
	NewFileSink  func() progress.Sink
	LandingsFor  func(item planner.Item) []string // the media an item's DLE lands on, primary first (dumptype override, else the default landing route)
	RunSink      progress.Sink
	EstimateSink progress.Sink
}

// Conductor executes one plan into one sealed run. It is per-run (it carries the
// run's open landing volume and progress sinks via Deps), built fresh each Run.
type Conductor struct{ d Deps }

// New constructs a Conductor from its dependencies.
func New(d Deps) *Conductor { return &Conductor{d: d} }
