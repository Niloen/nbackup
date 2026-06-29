package archiveio

import (
	"time"

	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// WriteFS is an archive filesystem open for writing one slot. Create reserves a write handle for one
// archive (back-pressuring); the handle is the xfer.Sink the caller transfers the encoded bytes into,
// whose commit seals + records the archive. Finish seals the slot.
//
// The clerk implements it serially over one medium (recording each placement inline). The spool
// implements it concurrency-safe and buffered over a backing WriteFS plus holding media. The dumper
// points at either, so where an archive lands (straight to the backing medium, or buffered through
// holding disks) is the WriteFS implementation's concern, not the producer's.
type WriteFS interface {
	// Create reserves ingestion for the archive described by meta, estimated at est bytes, blocking
	// for back-pressure and returning the run's error if the filesystem has failed. prog receives the
	// running landed (compressed) byte count for the producer's progress tracker.
	Create(meta record.Archive, est int64, prog func(landed int64)) (xfer.Sink, error)
	// Finish seals the slot and returns it (or the run's error if a write failed).
	Finish(now time.Time) (*record.Slot, error)
}
