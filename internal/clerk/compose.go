package clerk

import (
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// compose.go is the clerk's write side: the two medium write endpoints (a metering sink for a
// fresh dump, a passthrough sink for a copy) and the slot session that commits archives and
// records the run. The encode filters and the tar source live in the operation (the Dumper),
// which runs the transfer into one of these sinks — the clerk only lands and records bytes.

// MediumSink lands a fresh archive's bytes onto a slot's volumes via the open archiveio.Writer:
// it meters (sha256 + size) and splits the stream into parts, keeping the measured archive +
// part positions for Session.Commit to finalize once the producer's stats are merged in. It is
// the write peer of the archive Source.
type MediumSink struct {
	w    *archiveio.Writer
	meta record.Archive

	arch  record.Archive // measured (Compressed/SHA256/Parts), filled by Drain
	parts []record.FilePos
}

func (m *MediumSink) Drain(in io.Reader, progress func(int64)) (xfer.SinkResult, error) {
	arch, parts, err := m.w.WriteArchive(m.meta, in, progress)
	if err != nil {
		return xfer.SinkResult{}, err
	}
	m.arch, m.parts = arch, parts
	return xfer.SinkResult{Compressed: arch.Compressed, SHA256: arch.SHA256}, nil
}

// CopySink re-splits a source copy's already-compressed bytes onto the target's volumes without
// recompressing, verifying the stream against the recorded checksum and committing the archive
// (footer + index) on Drain — so a copy needs no producer-stats merge (meta is already final).
type CopySink struct {
	w    *archiveio.Writer
	meta record.Archive
}

func (s *CopySink) Drain(in io.Reader, _ func(int64)) (xfer.SinkResult, error) {
	_, err := s.w.CopyArchive(s.meta, in)
	return xfer.SinkResult{}, err
}

// Session authors one slot onto medium: the operation opens it over an archiveio.Writer, runs a
// transfer into one of its sinks per archive (committing each), and Finishes — which seals the
// in-memory slot and records the run in the map. It is the single place a slot's placement and
// per-archive footers/indexes are assembled; the encode/decode and tar live in the operation.
type Session struct {
	clerk  *Clerk
	w      *archiveio.Writer
	medium string
}

// OpenSlot starts a write session over an open slot writer landing on medium.
func (c *Clerk) OpenSlot(w *archiveio.Writer, medium string) *Session {
	return &Session{clerk: c, w: w, medium: medium}
}

// Sink returns the metering medium sink for one fresh archive (a dump). The operation runs the
// encode transfer (tar → compress → encrypt) into it, then calls Commit with the producer's
// stats. meta carries the archive's descriptive identity and schemes.
func (s *Session) Sink(meta record.Archive) *MediumSink { return &MediumSink{w: s.w, meta: meta} }

// CopySink returns a passthrough sink that re-authors an existing archive (a copy): the
// operation transfers the source's raw bytes into it (no filters), and it verifies + commits on
// Drain. The caller sets meta.Members so the target writes a real member index.
func (s *Session) CopySink(meta record.Archive) *CopySink { return &CopySink{w: s.w, meta: meta} }

// Summary is what the operation needs back to track and log a finished archive — never its
// parts or storage record.
type Summary struct {
	FileCount    int
	Uncompressed int64
	Compressed   int64
	Codec        string // the compression scheme applied ("none" => stored, not compressed)
}

// Commit finalizes a dumped archive: it merges the producer's raw-stream stats (file count,
// uncompressed size, member list) into the metered archive, writes the commit footer + member
// index, caches the members server-side, and reports a Summary. Call it once the operation's
// transfer into the sink has drained.
func (s *Session) Commit(sink *MediumSink, produced xfer.Produced) (Summary, error) {
	arch := sink.arch
	arch.Uncompressed = produced.Uncompressed
	arch.FileCount = produced.FileCount
	arch.Members = produced.Members
	if err := s.w.Commit(arch, sink.parts); err != nil {
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
