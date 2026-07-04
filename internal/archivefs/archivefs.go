// Package archivefs is NBackup's archive filesystem — the file layer that turns a logical
// archiveio.Ref into a byte endpoint and back, and nothing more. It is like a kernel VFS:
// it owns the archive map (resolve a copy's positions on read, record a run's placement
// on write — the Map role), the member index, and read-mounting via opened read media
// (the Mounter role). It speaks only io.* — plain byte endpoints, never a transfer — and
// exposes its two faces (contracts.go) plus the writer intake:
//
//   - ReadStore (implemented by FS): OpenArchive → an io.ReadCloser over one archive's raw
//     on-medium bytes (copy-selected); OpenArchives → the same for a selection, but the fs
//     drives an ordered one-pass read and calls back per archive; Members → the member list.
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
	AddArchive(arch record.Archive, medium string, pos archiveio.ArchivePos) error
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
func (fs *FS) Members(ref archiveio.Ref) ([]string, error) {
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
func indexPosOf(p catalog.Placement, dle string, level int) (archiveio.FilePos, bool) {
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return a.Index, a.Index != (archiveio.FilePos{})
		}
	}
	return archiveio.FilePos{}, false
}

// OpenArchive opens one archive's raw (undecoded, on-medium) part stream as an io.ReadCloser,
// with copy selection and fail-over: medium "" tries every copy (preferring the engine's own), a
// set medium reads only that copy so a fault on it is not masked by another. The open (and thus
// the copy-selection fail-over) happens eagerly, so a missing copy is reported before bytes flow.
// The caller wraps it for a transfer (xfer.Reader); the fs only hands back bytes.
//
// It is the single-archive special case of OpenArchives: one ref through the ordered pass, its
// handle lifted out of the callback. The located-but-unopenable verdict arrives as the pass's
// error (openRef's fail-over exhausted); the never-located verdict as the missing slice, which we
// turn back into the copy-selection error (both wrap ErrMissingCopy, the callers' one contract).
func (fs *FS) OpenArchive(ref archiveio.Ref, medium string) (io.ReadCloser, error) {
	var out io.ReadCloser
	missing, err := fs.OpenArchives([]archiveio.Ref{ref}, medium, func(_ archiveio.Ref, open func() (io.ReadCloser, error)) error {
		rc, err := open()
		out = rc
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, fs.missingCopyErr(ref, medium)
	}
	return out, nil
}

// VerifyPart re-hashes one part of an archive's copy on a medium against the seal the
// placement recorded — the bounded-egress integrity primitive: a sampling drill reads a
// single part off the medium instead of the archive. It is medium-pinned by nature (a
// seal describes one placement's layout, so there is no copy fail-over), and it errors
// when the placement records no seals — the caller falls back to a whole-archive check.
func (fs *FS) VerifyPart(ref archiveio.Ref, medium string, idx int) (bool, error) {
	for _, p := range onMedium(fs.cat.PlacementsFor(ref.Run), medium) {
		pa, ok := p.Placed(ref.DLE, ref.Level)
		if !ok {
			continue
		}
		if len(pa.Seals) != len(pa.Parts) || len(pa.Seals) == 0 {
			return false, fmt.Errorf("archive %s %s L%d on %q records no part seals", ref.Run, ref.DLE, ref.Level, medium)
		}
		if idx < 0 || idx >= len(pa.Parts) {
			return false, fmt.Errorf("archive %s %s L%d on %q has no part %d (%d part(s))", ref.Run, ref.DLE, ref.Level, medium, idx, len(pa.Parts))
		}
		r, err := fs.readerFor(medium)
		if err != nil {
			return false, err
		}
		return r.VerifyPart(ref, pa.Parts, idx, pa.Seals[idx])
	}
	return false, fs.missingCopyErr(ref, medium)
}

// readerFor returns the medium's bound block-layer read end: an archiveio.Reader over a
// mounting opener for the medium's volumes, paced by its shared bandwidth cap. Callers that
// drive a read loop themselves (OpenArchives) thread one Reader across all of a copy's
// archives, so consecutive same-volume reads reuse the mount.
func (fs *FS) readerFor(medium string) (*archiveio.Reader, error) {
	mounter, err := fs.deps.MounterFor(medium)
	if err != nil {
		return nil, err
	}
	open := func(p archiveio.FilePos) (record.Header, io.ReadCloser, error) {
		return mounter.ReadFileAt(p.Label, p.Epoch, p.Pos)
	}
	return archiveio.NewReader(open, fs.deps.Limiter(medium)), nil
}

// readIndex reads an archive's member index off a medium — the lazy fallback when the
// server-side member cache misses (a rebuilt run not yet browsed). It mounts the volume the
// index lives on and decodes it.
func (fs *FS) readIndex(medium string, pos archiveio.FilePos) ([]string, error) {
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

// openRef opens ref by copy selection + fail-over: it resolves the placements holding ref's run
// (all copies in read-preference order, or only those on medium when set), then tries each that
// carries the archive — obtaining that copy's medium Reader through readerFor — until one opens,
// so a read fails over to another copy. readerFor is threaded in rather than fixed to fs.readerFor
// so a batch (OpenArchives) can hand its pooled, mount-reusing factory while a bare open passes a
// fresh one. It is the sole home of copy selection and the missing-copy errors (ErrMissingCopy).
func (fs *FS) openRef(ref archiveio.Ref, medium string, readerFor func(string) (*archiveio.Reader, error)) (io.ReadCloser, error) {
	placements := fs.cat.PlacementsFor(ref.Run)
	if medium != "" {
		placements = onMedium(placements, medium)
	}
	if len(placements) == 0 {
		return nil, fs.missingCopyErr(ref, medium)
	}
	var lastErr error
	for _, p := range placements {
		parts, ok := p.Parts(ref.DLE, ref.Level)
		if !ok {
			continue
		}
		r, err := readerFor(p.Medium)
		if err != nil {
			// The medium won't open — not in this config, or refused because the open
			// write-window holds it. Treat it like any unavailable copy and fail over.
			lastErr = err
			continue
		}
		rc, err := r.Open(ref, parts)
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

// missingCopyErr is the copy-selection verdict when no placement carries ref (medium ""), or none
// on the pinned medium does — the len==0 head of openRef, factored out so OpenArchive can raise
// the same message when the ordered pass reports ref as missing rather than opening it.
func (fs *FS) missingCopyErr(ref archiveio.Ref, medium string) error {
	if medium != "" {
		return fmt.Errorf("%w: run %s has no copy on medium %q", ErrMissingCopy, ref.Run, medium)
	}
	return fmt.Errorf("%w: run %s not in catalog (run `nb rebuild`)", ErrMissingCopy, ref.Run)
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
