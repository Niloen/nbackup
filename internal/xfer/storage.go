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

// WriteSlotStorage is the slot-write surface the producer drives: NewWrite reserves a per-archive
// SlotWriter (whose Commit records the placement), blocking for back-pressure and returning the run's
// error if the store has failed; Finish seals the slot. est is the producer's size estimate — a
// routing store (the spool) uses it to pick a medium; a leaf medium ignores it. prog, when non-nil,
// receives the running landed (compressed) byte count. The dumper points at either a spool (buffered,
// concurrency-safe over a backing + holding media) or a single medium's store, never caring which.
type WriteSlotStorage interface {
	NewWrite(meta record.Archive, est int64, prog func(int64)) (SlotWriter, error)
	Finish(now time.Time) (*record.Slot, error)
}

// SlotStorage is a single medium's slot store: a WriteSlotStorage that additionally reads a committed
// archive's payload back (OpenArchive) and drops it (Reclaim) — the read/delete a holding->backing
// drain needs to move a staged archive to the backing and free the holding disk. The spool composes
// these (one backing, an array of holding); the clerk implements them.
type SlotStorage interface {
	WriteSlotStorage
	OpenArchive(arch record.Archive, pos record.ArchivePos) (io.ReadCloser, error)
	Reclaim(arch record.Archive, pos record.ArchivePos) error
}
