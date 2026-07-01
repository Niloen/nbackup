// Package dumper is the producing half of a dump: it spins up workers that archive each planned
// DLE — running the tar source and the encode pipeline (compress/encrypt, placed client- or
// server-side) — and transfers the bytes into a Store the consumer hands out. It owns parallelism
// and the per-item dump; it never touches the catalog or decides where an archive is stored. The
// consumer (the drain over holding disks, or the landing itself) is an archive store: NewArchive
// reserves an ingestion xfer.Sink for one archive (back-pressuring the producer), the producer
// transfers the encoded stream into it, and the sink's commit finalizes the stored archive.
package dumper

import (
	"context"
	"errors"
	"runtime"
	"sync"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
)

// The producer ingests into an archiveio.Ingest: for each archive it calls NewArchive for a
// write handle (an xfer.Sink, back-pressuring), transfers the encoded stream into it, and the handle's
// commit seals the stored archive. The clerk implements a serial store; the spool a concurrency-safe,
// buffered one. The producer never sees the session, the medium, or the catalog — only the store.

// Config is the resolution the producer needs, injected by the engine so the producer stays free
// of config and the catalog: how to resolve a DLE's archiver, its dumptype excludes, and its encode
// recipe, plus the compressor's thread count (for the oversubscription warning).
type Config struct {
	ArchiverFor func(dumpType, host string) (archiver.Archiver, error)
	Exclude     func(dumpType string) []string
	Placement   func(dumpType string) EncodePlacement
	Threads     int
}

// Dumper archives planned items into an archiveio.Ingest. Build it with New and drive it with Run.
type Dumper struct {
	archiverFor func(dumpType, host string) (archiver.Archiver, error)
	exclude     func(dumpType string) []string
	placement   func(dumpType string) EncodePlacement
	threads     int
}

// New builds a Dumper from cfg.
func New(cfg Config) *Dumper {
	return &Dumper{archiverFor: cfg.ArchiverFor, exclude: cfg.Exclude, placement: cfg.Placement, threads: cfg.Threads}
}

// dumpGate bounds how many DLEs run the heavy transfer (the tar source + encode pipeline) at once.
// A DLE acquires its target (the store's NewArchive — a holding-disk slot or the backing permit)
// before entering the gate, so a DLE parked waiting on a full holding disk or a busy landing holds
// no slot, and `workers` counts dumps actually running rather than waiters. acquire blocks for a
// slot and returns the matching release.
type dumpGate func() (release func())

// noGate runs the transfer unbounded — the serial path (a single worker, or a single DLE) needs no slot.
var noGate = dumpGate(func() func() { return func() {} })

// Run archives every item into the store route maps it to: for each it opens an ingestion Sink
// (NewArchive), transfers the encoded archive into it, and commits it (see dumpItem). route resolves a
// DLE's landing (its dumptype's `landing` override, else the config-wide one) to that backing's store,
// so different sources land on different media within one run; the dumper itself never decides where an
// archive is stored. With workers > 1 it runs one goroutine per DLE, each acquiring its target before
// borrowing one of `workers` transfer slots (a dumpGate), so the bound counts dumps actually running,
// not DLEs waiting on a holding disk or a landing. A single DLE's failure (its source or its upload)
// does not stop the others — every DLE is attempted and the per-DLE errors are joined into the return
// value, so the archives that succeeded still commit while the run reports failure. Only a backing-store
// abort (a landing is unreachable, so nothing more can land) stops scheduling new DLEs.
func (d *Dumper) Run(ctx context.Context, items []planner.Item, workers int, route func(planner.Item) archiveio.Ingest, tr *progress.Tracker, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// A backing-store abort is fatal — once a landing is unreachable no further archive can land,
	// so stop scheduling. A DLE's own failure is not: it is recorded and the rest carry on. The Spool
	// exposes Aborted(); a single-medium store (the clerk) does not, so there it never aborts here.
	aborted := func(fs archiveio.Ingest) bool {
		a, ok := fs.(interface{ Aborted() error })
		return ok && a.Aborted() != nil
	}
	// Stop scheduling once the run is canceled (ctx) or a backing has aborted. An
	// in-flight dump's processes are killed through ctx (programs.RunPipe); this just keeps
	// no further DLE from starting.
	stop := func(fs archiveio.Ingest) bool { return ctx.Err() != nil || aborted(fs) }
	if workers <= 1 || len(items) <= 1 {
		var errs []error
		for _, item := range items {
			fs := route(item)
			if stop(fs) {
				break
			}
			if err := d.dumpItem(ctx, fs, item, noGate, tr, logf); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	threads := d.threads
	if threads < 1 {
		threads = 1
	}
	if cores := runtime.GOMAXPROCS(0); workers*threads > cores {
		logf("WARNING: %d workers x %d compressor thread(s) = %d exceeds %d cores; CPU may be oversubscribed",
			workers, threads, workers*threads, cores)
	}

	// One goroutine per DLE. Each acquires its target inside dumpItem (off the gate, so a DLE waiting
	// for a holding disk or the backing permit parks here without holding a worker) and then borrows
	// a slot through gate for the transfer only — so `workers` bounds dumps actually running.
	sem := make(chan struct{}, workers)
	gate := dumpGate(func() func() { sem <- struct{}{}; return func() { <-sem } })
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, item := range items {
		fs := route(item)
		if stop(fs) {
			break
		}
		wg.Add(1)
		go func(it planner.Item, fs archiveio.Ingest) {
			defer wg.Done()
			if err := d.dumpItem(ctx, fs, it, gate, tr, logf); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(item, fs)
	}
	wg.Wait()
	return errors.Join(errs...)
}
