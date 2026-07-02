package archiveio

import (
	"errors"
	"io"

	"github.com/Niloen/nbackup/internal/record"
)

// store.go defines the surfaces above the raw writer: a Store (a medium end you write to, read back,
// and reclaim) and an Ingest (a producer's source of ArchiveWriters). They live here, below
// xfer, so they can reference the record-free ArchiveSpec. Neither is a factory-cum-store: writers
// are always built by NewAuthor over a WriteStore; a Store is the medium end, and the only thing that
// hands out writers is the spool (an Ingest), which is not a WriteStore.

// ArchiveSpec is the record-free description of a new archive to write: its DLE and identity
// (host/path), the archiver that produced it, the scheme it was encoded with, its level, and the run
// its base lives in. Only archiveio turns a spec plus the metered bytes into a record.Archive, so
// callers describe intent and never construct the storage record themselves.
type ArchiveSpec struct {
	DLE      string
	Host     string
	Path     string
	Archiver string
	Compress string
	Encrypt  string
	Level    int
	BaseRun  string
}

// CommitResult is the assembled outcome of writing one archive: the record and where it landed. The
// writer builds it in Commit and reports it to its WriteStore (WriteStore.Record) — the whole
// worker→coordinator crossing, a single value passed by copy, never shared writer state.
type CommitResult struct {
	Archive record.Archive
	Pos     record.ArchivePos
}

// Store is one authored run seen through its medium end: it is a WriteStore — a writer built over it (a
// serial one, or the spool's routed one) authors archives to it — and it can read a committed archive's
// payload back (OpenArchive) and drop it (Reclaim). It is not a factory: archive writers are made by
// NewAuthor over the WriteStore, never by the Store. A landing needs only the WriteStore; a holding disk is a
// full Store, since the drain reads its staged archives back and reclaims them. The clerk implements it.
type Store interface {
	WriteStore
	OpenArchive(arch record.Archive, pos record.ArchivePos) (io.ReadCloser, error)
	Reclaim(arch record.Archive, pos record.ArchivePos) error
}

// Ingest is the producer's source of ArchiveWriters: NewArchive reserves a per-archive
// writer, blocking for back-pressure and returning the run's error if it has failed. est is the size
// estimate — the spool (the only implementer) uses it to pick a medium. It is deliberately not a
// WriteStore: it manufactures writers (over whatever WriteStore it chooses), it is not itself written to. The
// dumper points at one and drives the writers it hands back.
type Ingest interface {
	NewArchive(spec ArchiveSpec, est int64) (*ArchiveWriter, error)
	// NewCopy reserves a per-archive writer that re-authors an already-sealed archive (a copy or
	// sync), preserving its identity, checksum, and members rather than producing a fresh one. Like
	// NewArchive it blocks for back-pressure and leases a drive; only the writer it builds differs.
	NewCopy(arch record.Archive, est int64) (*ArchiveWriter, error)
}

// ErrMissingCopy marks a read failure where no available copy of the requested archive
// exists (not in the catalog, or no copy on the pinned medium). Part of the ReadStore
// contract: callers classify it via errors.Is, so classification never depends on the
// message wording.
var ErrMissingCopy = errors.New("no available copy")

// ReadStore is the read face of the archive fs — the mirror of WriteStore: a logical Ref
// resolved to its raw on-medium bytes. It speaks only refs and bytes; the schemes, the far-end
// tar, and the transfers live in the operations (the Restorer, the Verifier), exactly as on the
// write side. The clerk implements it (copy selection over the catalog, mounting via the
// librarian); tests implement it with a fake, so the read operations never need real media.
type ReadStore interface {
	// Open returns one archive's raw part stream, copy-selected: medium "" tries every copy
	// (preferring the caller's own) with fail-over; a set medium reads only that copy, so a
	// fault on it is not masked by another. The open is eager — a missing copy errors here.
	Open(ref Ref, medium string) (io.ReadCloser, error)
	// ReadArchives reads a selection in one ordered pass (levels ascending per DLE, physically
	// forward otherwise), calling fn per archive with an open func over its bytes; fn may open
	// more than once. Refs with no available copy are skipped and returned as missing.
	ReadArchives(refs []Ref, medium string, fn func(ref Ref, open func() (io.ReadCloser, error)) error) (missing []Ref, err error)
	// Members returns an archive's member list (cache, else the on-medium index).
	Members(ref Ref) ([]string, error)
}
