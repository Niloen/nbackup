// Package dumper is the producing half of a dump: it spins up workers that archive each planned
// DLE — running the tar source and the encode pipeline (compress/encrypt, placed client- or
// server-side) — and writes the bytes into a Store the consumer hands out. It owns parallelism and
// the per-item dump; it never touches the catalog or decides where an archive is stored. The
// consumer (a drain over holding disks, or the landing itself) implements Store: Acquire reserves a
// write Target (back-pressuring the producer), and Target.Landed reports the committed archive.
package dumper

import (
	"runtime"
	"sync"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
)

// Store is the consumer the producer writes into: it hands out one write Target per archive,
// back-pressuring via Acquire. The drain implements it (deciding holding-disk vs direct); a test
// can fake it.
type Store interface {
	// Acquire reserves where to write an archive estimated at est bytes, blocking for
	// back-pressure and returning the run's error if the consumer has failed.
	Acquire(est int64) (Target, error)
}

// Target is one archive's write reservation: the session to write into, and a callback to report
// the committed archive.
type Target interface {
	Session() *clerk.Session
	Landed(arch record.Archive, pos record.ArchivePos) error
}

// Config is the resolution the producer needs, injected by the engine so the producer stays free
// of config and the catalog: how to resolve a DLE's archiver, its dumptype excludes, and its encode
// recipe, plus the compressor's thread count (for the oversubscription warning).
type Config struct {
	ArchiverFor func(dumpType, host string) (archiver.Archiver, error)
	Exclude     func(dumpType string) []string
	Placement   func(dumpType string) EncodePlacement
	Threads     int
}

// Dumper archives planned items into a Store. Build it with New and drive it with Run.
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

// Run archives every item into store: it acquires a Target for each, dumps the archive into the
// Target's session, and reports it Landed. With workers > 1 it runs that many concurrently, bounded
// by a semaphore; the first error stops scheduling and is returned (a consumer failure surfaces as
// the error Acquire/Landed return, so blocked workers wake and stop too).
func (d *Dumper) Run(items []planner.Item, workers int, store Store, tr *progress.Tracker, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dumpOne := func(item planner.Item) error {
		t, err := store.Acquire(item.EstBytes)
		if err != nil {
			return err
		}
		arch, pos, err := d.dumpItem(t.Session(), item, tr, logf)
		if err != nil {
			return err
		}
		return t.Landed(arch, pos)
	}
	if workers <= 1 || len(items) <= 1 {
		for _, item := range items {
			if err := dumpOne(item); err != nil {
				return err
			}
		}
		return nil
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
		wg       sync.WaitGroup
		sem      = make(chan struct{}, workers)
		mu       sync.Mutex
		firstErr error
	)
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	for _, item := range items {
		if failed() {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it planner.Item) {
			defer wg.Done()
			defer func() { <-sem }()
			if failed() {
				return
			}
			if err := dumpOne(it); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(item)
	}
	wg.Wait()
	return firstErr
}
