// Package archivefs is NBackup's archive filesystem — the file layer that turns a logical
// record.Ref into a byte endpoint and back, and nothing more. It is like a kernel VFS:
// it owns the archive map (resolve a copy's positions on read, record a run's placement
// on write — the Map role), the member index, and read-mounting via opened read media
// (the Mounter role). It speaks only io.* — plain byte endpoints, never a transfer — and
// exposes its two faces (contracts.go) plus the writer intake:
//
//   - ReadStore (implemented by FS): Open → an io.ReadCloser over one archive's raw
//     on-medium bytes (copy-selected); ReadArchives → the same, but the fs drives an
//     ordered one-pass read and calls back per archive; Members → the member list.
//   - WriteStore (implemented by Session): one run's write handle on its medium. A writer
//     is bound to it with archiveio.NewWriter (by the engine for a copy/flush, by the
//     spool for a concurrent dump) and, on Commit, reports the placement via the
//     Session's Record; OpenArchiveAt/ReclaimAt read a staged archive back and drop it.
//     There is no seal: a run is its committed archives.
//   - Ingest (implemented by the spool): the producer's writer factory.
//
// What it deliberately does NOT do: schemes, tar, or composing transfers. The decode/encode and
// the far-end tar live in the *operations* (the Dumper, Restorer, Verifier, …), which wrap an
// fs endpoint in an xfer.Transfer — exactly as cp/gzip compose over a filesystem's open().
// So the fs knows nothing of xfer, config, archivers, compress/encrypt, or the librarian
// package; its only deps are the Map, the Mounter, a bandwidth Limiter, and its own
// member-index cache.
package archivefs

import (
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
)

// ReadMap is the fs's read slice of the catalog — the archive map (the inode/extent
// table) resolved on read: which copies of a run, and where each archive's parts live.
// The engine implements it over the live catalog (and keeps the directory/retention/volume
// slices); a run window's read-only fs implements it over the window's catalog.View.
// PlacementsFor returns copies in read-preference order (the engine's own first).
type ReadMap interface {
	PlacementsFor(runID string) []catalog.Placement
}

// WriteMap is the fs's write slice of the catalog — the ReadMap's mirror, where a
// Session records a run's archives. The catalog implements it; the read-only window
// fs has none, so its read face is read-only by type, not by convention.
type WriteMap interface {
	// AddArchive records one committed archive's content + its on-medium position — the
	// catalog's single write path. The run entry is created from the archive's own run tag
	// (arch.Run), never added wholesale; there is no completion step — a run is its archives.
	AddArchive(arch record.Archive, medium string, pos record.ArchivePos) error
	// RemoveArchive drops one archive's placement on a medium (and its run entry if that was the
	// last copy) — the reclaim path when a staged archive has landed on the backing.
	RemoveArchive(runID, medium, dle string) (placementGone, entryGone bool, err error)
}

// Mounter is the fs's data-path slice of an opened read medium: mount the volume a part
// names and read it. The depot's ReadMedium satisfies it; the fs depends on the role, not
// the depot package, so a window's snapshot reader (or a test fake) plugs in the same way.
type Mounter interface {
	ReadFileAt(volume string, epoch, pos int) (record.Header, io.ReadCloser, error)
}

// Depot is the fs's slice of the depot — the rest of what the data path needs beside the
// map: a medium's read-mount and its bandwidth cap. (The transforms, the tar endpoints, and
// the archiver/executor resolution all moved up to the operations.) The engine implements
// it over its depot; this interface is the explicit boundary between deciding and doing.
type Depot interface {
	// MounterFor returns a read-mount onto a medium's volumes.
	MounterFor(medium string) (Mounter, error)
	// Limiter returns the medium's shared bandwidth cap (nil = uncapped).
	Limiter(medium string) *ratelimit.Limiter
}

// FS is the archive data path — the fs's read face (it implements ReadStore). Construct one
// with New over the archive map and the depot slice.
// It owns the member-index part of the catalog: it writes each archive's member list (cache +
// on-medium index) as it commits, and serves it on read, so member I/O lives in one place
// regardless of which operation needs it.
type FS struct {
	deps   Depot
	cat    ReadMap
	mindex *catalog.MemberIndex
}

// New returns a data path over the archive map and the orchestrator's services, using mindex
// as the server-side member cache.
func New(cat ReadMap, deps Depot, mindex *catalog.MemberIndex) *FS {
	return &FS{deps: deps, cat: cat, mindex: mindex}
}

// Members returns an archive's member list, lazily: from the member-index cache, else by
// reading the on-medium index (via a copy's recorded index position) and re-caching it. A nil
// list is a valid "no members" answer (an archive with no files records no index).
func (fs *FS) Members(ref record.Ref) ([]string, error) {
	if members, ok, err := fs.mindex.Load(ref.Run, ref.DLE, ref.Level); err != nil {
		return nil, err
	} else if ok {
		return members, nil
	}
	for _, p := range fs.cat.PlacementsFor(ref.Run) {
		pos, ok := indexPosOf(p, ref.DLE, ref.Level)
		if !ok {
			continue
		}
		members, err := fs.readIndex(p.Medium, pos)
		if err != nil {
			continue // try another copy
		}
		_ = fs.mindex.Store(ref.Run, ref.DLE, ref.Level, members)
		return members, nil
	}
	return nil, nil
}

// indexPosOf finds an archive's recorded member-index position on a placement (the zero
// position means the archive recorded no index — it had no members).
func indexPosOf(p catalog.Placement, dle string, level int) (record.FilePos, bool) {
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return a.Index, a.Index != (record.FilePos{})
		}
	}
	return record.FilePos{}, false
}

// Open opens an archive's raw (undecoded, on-medium) part stream as an io.ReadCloser, with
// copy selection and fail-over: medium "" tries every copy (preferring the engine's own), a set
// medium reads only that copy so a fault on it is not masked by another. The open (and thus the
// copy-selection fail-over) happens eagerly, so a missing copy is reported before bytes flow.
// The caller wraps it for a transfer (xfer.Reader); the fs only hands back bytes.
func (fs *FS) Open(ref record.Ref, medium string) (io.ReadCloser, error) {
	return fs.eachPlacement(ref, medium, func(parts []record.FilePos, p catalog.Placement) (io.ReadCloser, error) {
		r, err := fs.readerFor(p.Medium)
		if err != nil {
			return nil, err
		}
		return r.Open(ref, parts)
	})
}

// readerFor returns the medium's bound block-layer read end: an archiveio.Reader over a
// mounting opener for the medium's volumes, paced by its shared bandwidth cap. Callers that
// drive a read loop themselves (ReadArchives) thread one Reader across all of a copy's
// archives, so consecutive same-volume reads reuse the mount.
func (fs *FS) readerFor(medium string) (*archiveio.Reader, error) {
	mounter, err := fs.deps.MounterFor(medium)
	if err != nil {
		return nil, err
	}
	open := func(p record.FilePos) (record.Header, io.ReadCloser, error) {
		return mounter.ReadFileAt(p.Label, p.Epoch, p.Pos)
	}
	return archiveio.NewReader(open, fs.deps.Limiter(medium)), nil
}

// readIndex reads an archive's member index off a medium — the lazy fallback when the
// server-side member cache misses (a rebuilt run not yet browsed). It mounts the volume the
// index lives on and decodes it.
func (fs *FS) readIndex(medium string, pos record.FilePos) ([]string, error) {
	mounter, err := fs.deps.MounterFor(medium)
	if err != nil {
		return nil, err
	}
	_, rc, err := mounter.ReadFileAt(pos.Label, pos.Epoch, pos.Pos)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return record.DecodeIndex(rc)
}

// eachPlacement resolves the placements holding ref's run (all copies, or only those on
// medium when set), then tries each that carries the archive — opening it via open — until
// one succeeds, so a read fails over to another copy. It is the one place the raw read paths
// share copy selection and the missing-copy errors (ErrMissingCopy).
func (fs *FS) eachPlacement(ref record.Ref, medium string, open func(parts []record.FilePos, p catalog.Placement) (io.ReadCloser, error)) (io.ReadCloser, error) {
	placements := fs.cat.PlacementsFor(ref.Run)
	if medium != "" {
		placements = onMedium(placements, medium)
	}
	if len(placements) == 0 {
		if medium != "" {
			return nil, fmt.Errorf("%w: run %s has no copy on medium %q", ErrMissingCopy, ref.Run, medium)
		}
		return nil, fmt.Errorf("%w: run %s not in catalog (run `nb rebuild`)", ErrMissingCopy, ref.Run)
	}
	var lastErr error
	for _, p := range placements {
		parts, ok := p.Parts(ref.DLE, ref.Level)
		if !ok {
			continue
		}
		rc, err := open(parts, p)
		if err != nil {
			lastErr = err
			continue
		}
		return rc, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w of %s %s L%d in the catalog", ErrMissingCopy, ref.Run, ref.DLE, ref.Level)
	}
	return nil, lastErr
}

func onMedium(ps []catalog.Placement, medium string) []catalog.Placement {
	out := ps[:0:0]
	for _, p := range ps {
		if p.Medium == medium {
			out = append(out, p)
		}
	}
	return out
}

// The FS is the archive fs's read face; the write face per run is the Session (a WriteStore).
var _ ReadStore = (*FS)(nil)
