package clerk

import (
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/record"
)

// compose.go is the clerk's write side: a Session over a slot writer that takes already-encoded
// archive bytes (an io.Reader), meters + splits + commits them, and records the run. The encode
// transfer and the xfer machinery live in the operation (the Dumper), which drives bytes into
// WriteArchive/CopyArchive — the clerk takes plain bytes and never sees a scheme or a transfer.

// Session authors one slot onto medium: the operation opens it over an archiveio.Writer, writes
// each archive's bytes (committing each), and Finishes — which seals the slot. Archives are
// recorded as they commit: a copy/sync inline in CopyArchive (single-threaded), a parallel dump
// by the run's orchestrator, which Commit hands the position back to. It is the single place a
// slot's per-archive footers/indexes are assembled.
type Session struct {
	clerk  *Clerk
	w      *archiveio.Writer
	medium string
}

// OpenSlot starts a write session over an open slot writer landing on medium.
func (c *Clerk) OpenSlot(w *archiveio.Writer, medium string) *Session {
	return &Session{clerk: c, w: w, medium: medium}
}

// WriteArchive writes a fresh archive's already-encoded payload onto the slot's volumes,
// metering (sha256 + size) and splitting it into parts. It returns the measured archive (sha,
// compressed size, part count) and the part positions; the operation merges the producer's raw
// stats and calls Commit. tap, if non-nil, receives the running count of landed bytes.
func (s *Session) WriteArchive(meta record.Archive, payload io.Reader, tap func(int64)) (record.Archive, []record.FilePos, error) {
	return s.w.WriteArchive(meta, payload, tap)
}

// CopyArchive re-writes an existing archive's raw payload (no transform) onto this slot's
// volumes, verifying it against the recorded checksum and committing it (footer + index from
// meta.Members) — so a copy needs no separate Commit (meta is already final). Being
// single-threaded (one copy/sync, or the orchestrator's drain), it records the archive's
// placement on this medium inline, as it lands.
func (s *Session) CopyArchive(meta record.Archive, payload io.Reader) error {
	arch, pos, err := s.w.CopyArchive(meta, payload)
	if err != nil {
		return err
	}
	return s.clerk.cat.AddArchive(s.w.SlotMeta(), s.medium, arch, pos)
}

// Summary is what the operation needs back to track and log a finished archive — never its
// parts or storage record.
type Summary struct {
	FileCount    int
	Uncompressed int64
	Compressed   int64
	Compress     string // the compression scheme applied ("none" => stored, not compressed)
}

// Commit finalizes a dumped archive: it merges the producer's raw-stream stats (file count,
// uncompressed size, member list) into the metered archive WriteArchive returned, writes the
// commit footer + member index, caches the members server-side, and returns a Summary plus the
// committed archive and its on-medium position. It does NOT record the placement: a dump's
// workers run in parallel and the catalog has no lock, so the caller hands the committed archive
// to the run's single orchestrator to record (see engine.Run).
func (s *Session) Commit(measured record.Archive, parts []record.FilePos, fileCount int, uncompressed int64, members []string) (Summary, record.Archive, record.ArchivePos, error) {
	arch := measured
	arch.FileCount = fileCount
	arch.Uncompressed = uncompressed
	arch.Members = members
	pos, err := s.w.Commit(arch, parts)
	if err != nil {
		return Summary{}, record.Archive{}, record.ArchivePos{}, err
	}
	if len(arch.Members) > 0 {
		_ = s.clerk.mindex.Store(s.w.SlotID(), arch.DLE, arch.Level, arch.Members)
	}
	return Summary{FileCount: arch.FileCount, Uncompressed: arch.Uncompressed, Compressed: arch.Compressed, Compress: arch.Compress}, arch, pos, nil
}

// Finish closes the slot: it seals the in-memory slot and stamps it sealed in the catalog. The
// archives were recorded as they committed — a copy/sync inline in CopyArchive, a dump via the
// orchestrator that owns the catalog — so Finish only marks the run complete.
func (s *Session) Finish(now time.Time) (*record.Slot, error) {
	sealed, err := s.w.Finish(now)
	if err != nil {
		return nil, err
	}
	if err := s.clerk.cat.SealSlot(sealed.ID, now); err != nil {
		return nil, err
	}
	return sealed, nil
}
