package archiveio

import (
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
}
