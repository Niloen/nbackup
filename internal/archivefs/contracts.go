package archivefs

import (
	"errors"
	"io"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/record"
)

// contracts.go is the archive fs's two faces and its writer intake — the interfaces the
// operations (dumper, restorer, verifier, copier) and the engine consume. The canonical
// implementations live beside them: FS (the read face) and Session (the write face);
// the spool implements Ingest. Tests fake them, so the operations never need real media.

// ErrMissingCopy marks a read failure where no available copy of the requested archive
// exists (not in the catalog, or no copy on the pinned medium). Part of the ReadStore
// contract: callers classify it via errors.Is, so classification never depends on the
// message wording.
var ErrMissingCopy = errors.New("no available copy")

// ReadStore is the read face of the archive fs — the mirror of WriteStore: a logical
// record.Ref resolved to its raw on-medium bytes. It is global (any committed archive, all
// media, copy-selected) where the write face is one opened run — inherent to any fs: you
// read anywhere, you write through a handle you opened. It speaks only refs and bytes; the
// schemes, the far-end tar, and the transfers live in the operations (the Restorer, the
// Verifier), exactly as on the write side. The FS implements it (copy selection over the
// catalog, mounting via opened read media).
type ReadStore interface {
	// Open returns one archive's raw part stream, copy-selected: medium "" tries every copy
	// (preferring the caller's own) with fail-over; a set medium reads only that copy, so a
	// fault on it is not masked by another. The open is eager — a missing copy errors here.
	Open(ref record.Ref, medium string) (io.ReadCloser, error)
	// ReadArchives reads a selection in one ordered pass (levels ascending per DLE, physically
	// forward otherwise), calling fn per archive with an open func over its bytes; fn may open
	// more than once. Refs with no available copy are skipped and returned as missing.
	ReadArchives(refs []record.Ref, medium string, fn func(ref record.Ref, open func() (io.ReadCloser, error)) error) (missing []record.Ref, err error)
	// Members returns an archive's member list (cache, else the on-medium index).
	Members(ref record.Ref) ([]string, error)
}

// WriteStore is the write face of the archive fs — one run's medium end, the mirror of
// ReadStore: it receives each committed archive (archiveio.Recorder — the block-layer
// writer's commit seam), and it can read a committed archive's payload back (OpenArchive)
// and drop it (Reclaim). A landing is only ever recorded to; a holding disk uses the full
// surface, since the drain reads its staged archives back and reclaims them. It is not a
// writer factory: archive writers are made by archiveio.NewWriter over a PartAllocator +
// this Recorder, never by the store. The Session implements it.
type WriteStore interface {
	archiveio.Recorder
	OpenArchive(arch record.Archive, pos record.ArchivePos) (io.ReadCloser, error)
	Reclaim(arch record.Archive, pos record.ArchivePos) error
}

// Ingest is the producer's source of ArchiveWriters: NewArchive reserves a per-archive
// writer, blocking for back-pressure and returning the run's error if it has failed. est is the size
// estimate — the spool (the only implementer) uses it to pick a medium. It is deliberately not a
// WriteStore: it manufactures writers (over whatever allocator and recorder it chooses), it is not
// itself written to. The dumper points at one and drives the writers it hands back.
type Ingest interface {
	NewArchive(spec archiveio.ArchiveSpec, est int64) (*archiveio.ArchiveWriter, error)
	// NewCopy reserves a per-archive writer that re-authors an already-sealed archive (a copy or
	// sync), preserving its identity, checksum, and members rather than producing a fresh one. Like
	// NewArchive it blocks for back-pressure and leases a drive; only the writer it builds differs.
	NewCopy(arch record.Archive, est int64) (*archiveio.ArchiveWriter, error)
}
