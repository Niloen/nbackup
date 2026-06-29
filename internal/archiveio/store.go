package archiveio

import (
	"io"

	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// store.go defines the slot-write surface the producer and the spool drive, one level above the raw
// xfer.Sink: an ArchiveStore hands out per-archive ArchiveWriters and owns reading a committed
// archive back / dropping it. These interfaces live here, below xfer, so they can reference the
// record-free ArchiveSpec (xfer stays pure byte plumbing and imports neither). The spool depends only
// on these (one backing store, an array of holding ones) and never imports the catalog or the clerk
// that implements them. There is no "finish": a slot is its committed archives — each durable via its
// own footer and indexed in the catalog as it commits — so the run's slot is read from the catalog,
// not produced by sealing.

// ArchiveSpec is the record-free description of a new archive to write: its DLE and identity
// (host/path), the archiver that produced it, the scheme it was encoded with, its level, and the slot
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
	BaseSlot string
}

// ArchiveWriter is one archive's write handle. A transfer drives it as an xfer.Sink (NextPart +
// Commit), and the caller reads the committed archive and its on-medium position from Result
// afterwards — e.g. to queue a holding->backing copy or to reclaim the staged copy once it has
// landed. Commit records the placement; Result is valid only after a successful Commit.
type ArchiveWriter interface {
	xfer.Sink
	Result() (record.Archive, record.ArchivePos)
}

// ArchiveWriteStore is the slot-write surface the producer drives: NewArchive reserves a per-archive
// ArchiveWriter (whose Commit records the placement), blocking for back-pressure and returning the
// run's error if the store has failed. est is the producer's size estimate — a routing store (the
// spool) uses it to pick a medium; a leaf medium ignores it. prog, when non-nil, receives the running
// landed (compressed) byte count. The dumper points at either a spool (buffered, concurrency-safe over
// a backing + holding media) or a single medium's store, never caring which.
type ArchiveWriteStore interface {
	NewArchive(spec ArchiveSpec, est int64, prog func(int64)) (ArchiveWriter, error)
}

// ArchiveStore is a single medium's slot store: an ArchiveWriteStore that additionally re-authors an
// already-committed archive (NewCopy — raw bytes, checksum verified, source identity preserved), reads
// a committed archive's payload back (OpenArchive), and drops it (Reclaim). The latter two are the
// read/delete a holding->backing drain needs to move a staged archive to the backing and free the
// holding disk; NewCopy is the write side of that drain (and of `nb copy` / crash-recovery Flush). The
// spool composes these (one backing, an array of holding); the clerk implements them.
type ArchiveStore interface {
	ArchiveWriteStore
	NewCopy(arch record.Archive, prog func(int64)) (ArchiveWriter, error)
	OpenArchive(arch record.Archive, pos record.ArchivePos) (io.ReadCloser, error)
	Reclaim(arch record.Archive, pos record.ArchivePos) error
}
