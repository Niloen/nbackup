package clerk

import (
	"context"
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

// NewArchive begins writing a fresh archive's already-encoded payload onto the slot, pulled
// part-by-part by the returned handle's NextPart (the operation copies into each part writer and
// closes it). Commit then seals the archive once the producer's raw stats are known. tap, if non-nil,
// receives the running count of landed bytes.
func (s *Session) NewArchive(meta record.Archive, tap func(int64)) *ArchiveWriter {
	return &ArchiveWriter{s: s, aw: s.w.NewArchive(meta, tap)}
}

// ArchiveWriter is one archive's NextPart-driven write handle: a thin clerk wrapper over the
// archiveio writer that additionally caches the member index server-side on Commit. It records no
// placement — a parallel dump's worker commits, and hands the position to the run's single
// orchestrator to record.
type ArchiveWriter struct {
	s  *Session
	aw *archiveio.ArchiveWriter
}

// NextPart rolls to the next volume and returns the next part's writer plus its byte cap (max < 0 =
// unbounded). The caller copies up to max bytes into it and closes it; cancel ctx before Close to
// abort the part.
func (a *ArchiveWriter) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	return a.aw.NextPart(ctx)
}

// Commit finalizes the archive (footer + member index) with the producer's raw-stream stats and
// returns the committed archive + its on-medium position, caching the members server-side.
func (a *ArchiveWriter) Commit(ctx context.Context, fileCount int, uncompressed int64, members []string) (record.Archive, record.ArchivePos, error) {
	arch, pos, err := a.aw.Commit(ctx, fileCount, uncompressed, members)
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, err
	}
	if len(arch.Members) > 0 {
		_ = a.s.clerk.mindex.Store(a.s.w.SlotID(), arch.DLE, arch.Level, arch.Members)
	}
	return arch, pos, nil
}

// CopyArchive re-writes an existing archive's raw payload (no transform) onto this slot's
// volumes, verifying it against the recorded checksum and committing it (footer + index from
// meta.Members) — so a copy needs no separate Commit (meta is already final). Being
// single-threaded (one copy/sync, or the orchestrator's drain), it records the archive's
// placement on this medium inline, as it lands.
func (s *Session) CopyArchive(ctx context.Context, meta record.Archive, payload io.Reader) error {
	arch, pos, err := s.w.CopyArchive(ctx, meta, payload, nil)
	if err != nil {
		return err
	}
	return s.clerk.cat.AddArchive(s.w.SlotMeta(), s.medium, arch, pos)
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
