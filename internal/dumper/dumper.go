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

// The producer ingests into an archiveio.ArchiveWriteStore: for each archive it calls NewArchive for a
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

// Dumper archives planned items into an archiveio.ArchiveWriteStore. Build it with New and drive it with Run.
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

// Run archives every item into fs: for each it opens an ingestion Sink (NewArchive), transfers the
// encoded archive into it, and commits it (see dumpItem). With workers > 1 it runs that many
// concurrently, bounded by a semaphore. A single DLE's failure (its source or its upload) does not
// stop the others — every DLE is attempted and the per-DLE errors are joined into the return value,
// so the archives that succeeded still commit while the run reports failure. Only a backing-store
// abort (the landing is unreachable, so nothing more can land) stops scheduling new DLEs.
func (d *Dumper) Run(ctx context.Context, items []planner.Item, workers int, fs archiveio.ArchiveWriteStore, tr *progress.Tracker, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dumpOne := func(item planner.Item) error {
		return d.dumpItem(ctx, fs, item, tr, logf)
	}
	// A backing-store abort is fatal — once the landing is unreachable no further archive can land,
	// so stop scheduling. A DLE's own failure is not: it is recorded and the rest carry on. The Spool
	// exposes Aborted(); a single-medium store (the clerk) does not, so there it never aborts here.
	aborted := func() bool {
		a, ok := fs.(interface{ Aborted() error })
		return ok && a.Aborted() != nil
	}
	// Stop scheduling once the run is canceled (ctx) or the backing has aborted. An
	// in-flight dump's processes are killed through ctx (programs.RunPipe); this just keeps
	// no further DLE from starting.
	stop := func() bool { return ctx.Err() != nil || aborted() }
	if workers <= 1 || len(items) <= 1 {
		var errs []error
		for _, item := range items {
			if stop() {
				break
			}
			if err := dumpOne(item); err != nil {
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

	var (
		wg   sync.WaitGroup
		sem  = make(chan struct{}, workers)
		mu   sync.Mutex
		errs []error
	)
	for _, item := range items {
		if stop() {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it planner.Item) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := dumpOne(it); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(item)
	}
	wg.Wait()
	return errors.Join(errs...)
}
