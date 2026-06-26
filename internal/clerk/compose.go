package clerk

import (
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/record"
)

// compose.go is the clerk's write side: a Session over a slot writer that takes already-encoded
// archive bytes (an io.Reader), meters + splits + commits them, and records the run. The encode
// transfer and the xfer machinery live in the operation (the Dumper), which drives bytes into
// WriteArchive/CopyArchive — the clerk takes plain bytes and never sees a codec or a transfer.

// Session authors one slot onto medium: the operation opens it over an archiveio.Writer, writes
// each archive's bytes (committing each), and Finishes — which seals the in-memory slot and
// records the run in the map. It is the single place a slot's placement and per-archive
// footers/indexes are assembled.
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
// stats and calls Commit. progress, if non-nil, receives the running compressed byte count.
func (s *Session) WriteArchive(meta record.Archive, payload io.Reader, progress func(int64)) (record.Archive, []record.FilePos, error) {
	return s.w.WriteArchive(meta, payload, progress)
}

// CopyArchive re-writes an existing archive's raw payload (no transform) onto this slot's
// volumes, verifying it against the recorded checksum and committing it (footer + index from
// meta.Members) — so a copy needs no separate Commit (meta is already final).
func (s *Session) CopyArchive(meta record.Archive, payload io.Reader) error {
	_, err := s.w.CopyArchive(meta, payload)
	return err
}

// Summary is what the operation needs back to track and log a finished archive — never its
// parts or storage record.
type Summary struct {
	FileCount    int
	Uncompressed int64
	Compressed   int64
	Codec        string // the compression scheme applied ("none" => stored, not compressed)
}

// Commit finalizes a dumped archive: it merges the producer's raw-stream stats (file count,
// uncompressed size, member list) into the metered archive WriteArchive returned, writes the
// commit footer + member index, caches the members server-side, and reports a Summary.
func (s *Session) Commit(measured record.Archive, parts []record.FilePos, fileCount int, uncompressed int64, members []string) (Summary, error) {
	arch := measured
	arch.FileCount = fileCount
	arch.Uncompressed = uncompressed
	arch.Members = members
	if err := s.w.Commit(arch, parts); err != nil {
		return Summary{}, err
	}
	if len(arch.Members) > 0 {
		_ = s.clerk.mindex.Store(s.w.SlotID(), arch.DLE, arch.Level, arch.Members)
	}
	return Summary{FileCount: arch.FileCount, Uncompressed: arch.Uncompressed, Compressed: arch.Compressed, Codec: arch.Compress}, nil
}

// Finish closes the slot and records the run in the map: it seals the in-memory slot and records
// its placement (the archives' on-medium positions) under the session's medium. The clerk owns
// this map write, so every caller that authors a slot gets it recorded the same way.
func (s *Session) Finish(now time.Time) (*record.Slot, error) {
	sealed, err := s.w.Finish(now)
	if err != nil {
		return nil, err
	}
	placement := catalog.Placement{Medium: s.medium, Archives: s.w.Positions()}
	if err := s.clerk.cat.Record(sealed, placement); err != nil {
		return nil, err
	}
	return sealed, nil
}
