package archiveio

import (
	"github.com/Niloen/nbackup/internal/record"
)

// spec.go holds the block layer's descriptive value types: what to write (ArchiveSpec)
// and what was written (CommitResult). The fs contracts built over them (ReadStore,
// WriteStore, Ingest) live a layer up, in package archivefs.

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
// writer builds it in Commit and reports it to its Recorder — the whole worker→coordinator
// crossing, a single value passed by copy, never shared writer state.
type CommitResult struct {
	Archive record.Archive
	Pos     record.ArchivePos
}
