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
	Ext      string // the archiver's raw-stream filename extension (".tar"), the naming peer of Compress/Encrypt
	Compress string
	Encrypt  string
	Shape    record.Shape // stream shape, resolved by the dumper from the pipeline's declared capabilities
	AtomSize int64        // the atomic shape's atom bound (compressed bytes per sealed part); 0 for other shapes
	Level    int
	BaseRun  string
	Carves   []string // the anchored subtree carves the dump excluded (record.Archive.Carves)
}

// CommitResult is the assembled outcome of writing one archive: the record and where it landed. The
// writer builds it in Commit and reports it to its Recorder — the whole worker→coordinator
// crossing, a single value passed by copy, never shared writer state.
type CommitResult struct {
	Archive record.Archive
	Pos     ArchivePos
}

// Ref returns the committed archive's logical identity — Pos is pure position, so a
// caller acting on the result (the drain's read-back and reclaim) names it with this.
func (r CommitResult) Ref() Ref {
	return Ref{Run: r.Archive.Run, DLE: r.Archive.DLE, Level: r.Archive.Level}
}
