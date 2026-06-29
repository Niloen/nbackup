package xfer

import (
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// storage.go defines the slot-write surface the producer and the spool drive, one level above the
// raw Sink: a SlotStorage hands out per-archive SlotWriters and owns the slot's catalog side
// (placement on Commit, seal on Finish) plus the read/delete a holding->backing drain needs. The
// spool depends only on these interfaces — one backing storage and an array of holding ones — so it
// never imports the catalog or the clerk that implements them.

// SlotWriter is one archive's write handle. A transfer drives it as a Sink (NextPart + Commit), and
// the caller reads the committed archive and its on-medium position from Result afterwards — e.g. to
// queue a holding->backing copy or to reclaim the staged copy once it has landed. Commit records the
// placement; Result is valid only after a successful Commit.
type SlotWriter interface {
	Sink
	Result() (record.Archive, record.ArchivePos)
}

// SlotStorage authors archives onto one slot on one medium. NewWrite hands out a per-archive
// SlotWriter (whose Commit records the placement); OpenArchive reads a committed archive's payload
// back and Reclaim drops it (placement + files) — the read/delete the drain needs to move a staged
// archive to the backing and free the holding disk; Finish seals the slot. prog, when non-nil,
// receives the running landed (compressed) byte count for the archive being written.
type SlotStorage interface {
	NewWrite(meta record.Archive, prog func(int64)) SlotWriter
	OpenArchive(arch record.Archive, pos record.ArchivePos) (io.ReadCloser, error)
	Reclaim(arch record.Archive, pos record.ArchivePos) error
	Finish(now time.Time) (*record.Slot, error)
}
