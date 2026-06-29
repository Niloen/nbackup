package clerk

import (
	"context"
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// compose.go is the clerk's write side: a Session over a slot writer that takes already-encoded
// archive bytes (an io.Reader), meters + splits + commits them, and records the run. The encode
// transfer and the xfer machinery live in the operation (the Dumper), which drives bytes into the
// SlotWriter's NextPart/Commit — the clerk takes plain bytes and never sees a scheme or a transfer.

// Session authors one slot onto medium: the operation opens it over an archiveio.Writer, writes each
// archive (committing each, which records its placement), and Finishes — which seals the slot. It is
// an xfer.SlotStorage: NewWrite hands out a per-archive SlotWriter, OpenArchive/Reclaim read and drop
// a staged archive (the holding->backing drain), and Finish seals. vol is the medium's volume, used
// to read and reclaim staged archives on a single-volume medium (a holding disk).
type Session struct {
	clerk  *Clerk
	w      *archiveio.Writer
	medium string
	vol    media.Volume
}

var _ xfer.SlotStorage = (*Session)(nil)

// OpenSlot starts a write session over an open slot writer landing on medium, with vol the medium's
// volume for staged reads/reclaims (a holding disk's single volume; the backing passes its loaded
// volume, which OpenArchive/Reclaim are never called on).
func (c *Clerk) OpenSlot(w *archiveio.Writer, medium string, vol media.Volume) *Session {
	return &Session{clerk: c, w: w, medium: medium, vol: vol}
}

// NewWrite begins writing a fresh archive's already-encoded payload onto the slot, pulled
// part-by-part by the returned SlotWriter's NextPart (the operation copies into each part writer and
// closes it). Commit then seals the archive — writing its footer + member index and recording its
// placement — once the producer's raw stats are known. prog, if non-nil, receives the running count
// of landed bytes. A single medium does not route, so est (the size estimate) is unused here.
func (s *Session) NewWrite(meta record.Archive, _ int64, prog func(int64)) (xfer.SlotWriter, error) {
	return &ArchiveWriter{s: s, aw: s.w.NewArchive(meta, prog)}, nil
}

// ArchiveWriter is one archive's NextPart-driven write handle (an xfer.SlotWriter): a thin clerk
// wrapper over the archiveio writer that, on Commit, additionally caches the member index and records
// the archive's placement on the slot's medium. Result hands back the committed archive + position
// after Commit.
type ArchiveWriter struct {
	s    *Session
	aw   *archiveio.ArchiveWriter
	arch record.Archive
	pos  record.ArchivePos
}

var _ xfer.SlotWriter = (*ArchiveWriter)(nil)

// NextPart rolls to the next volume and returns the next part's writer plus its byte cap (max < 0 =
// unbounded). The caller copies up to max bytes into it and closes it; cancel ctx before Close to
// abort the part.
func (a *ArchiveWriter) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	return a.aw.NextPart(ctx)
}

// Commit (xfer.Sink) finalizes the archive against the producer's stats (footer + member index),
// caches the members, and records the placement on the slot's medium. Run on the orchestrator (the
// sole catalog writer) via the spool's RemoteSink, so the catalog write stays single-owner.
func (a *ArchiveWriter) Commit(ctx context.Context, p xfer.Produced) error {
	if err := a.aw.Commit(ctx, p); err != nil {
		return err
	}
	arch, pos := a.aw.Result()
	if len(arch.Members) > 0 {
		_ = a.s.clerk.mindex.Store(a.s.w.SlotID(), arch.DLE, arch.Level, arch.Members)
	}
	if err := a.s.clerk.cat.AddArchive(a.s.w.SlotMeta(), a.s.medium, arch, pos); err != nil {
		return err
	}
	a.arch, a.pos = arch, pos
	return nil
}

// Result returns the committed archive and its on-medium position; valid only after a successful
// Commit.
func (a *ArchiveWriter) Result() (record.Archive, record.ArchivePos) { return a.arch, a.pos }

// OpenArchive reads a committed archive's payload back by concatenating its parts straight off the
// session's volume (whose index the producer keeps current) — the drain's read seam, for copying a
// staged archive to the backing.
func (s *Session) OpenArchive(arch record.Archive, pos record.ArchivePos) (io.ReadCloser, error) {
	exp := archiveio.Expect{Slot: s.w.SlotID(), DLE: arch.DLE, Level: arch.Level}
	return archiveio.NewReader().Open(pos.Parts, exp,
		func(p record.FilePos) (record.Header, io.ReadCloser, error) { return s.vol.ReadFile(p.Pos) })
}

// Reclaim drops a staged archive once it has landed on the backing: it removes the archive's files
// from the medium's volume (the commit footer first, so an interrupted reclaim un-commits before
// dropping parts) then drops its placement from the catalog.
func (s *Session) Reclaim(arch record.Archive, pos record.ArchivePos) error {
	for _, p := range archivePosFiles(pos) {
		if err := s.vol.RemoveFile(p); err != nil {
			return err
		}
	}
	_, _, err := s.clerk.cat.RemoveArchive(s.w.SlotID(), s.medium, arch.DLE)
	return err
}

// CopyArchive re-writes an existing archive's raw payload (no transform) onto this slot's
// volumes, verifying it against the recorded checksum and committing it (footer + index from
// meta.Members) — so a copy needs no separate Commit (meta is already final). Being
// single-threaded (one copy/sync, or the flush recovery path), it records the archive's
// placement on this medium inline, as it lands.
func (s *Session) CopyArchive(ctx context.Context, meta record.Archive, payload io.Reader) error {
	arch, pos, err := s.w.CopyArchive(ctx, meta, payload, nil)
	if err != nil {
		return err
	}
	return s.clerk.cat.AddArchive(s.w.SlotMeta(), s.medium, arch, pos)
}

// Finish ends the write session and returns the slot it authored. There is no seal — the archives
// were each recorded as they committed (NewWrite's Commit, or CopyArchive inline), so a slot is
// simply its committed archives.
func (s *Session) Finish(now time.Time) (*record.Slot, error) {
	return s.w.Finish(now)
}

// archivePosFiles lists an archive's file positions for reclamation, the commit footer (the marker)
// first so an interrupted reclaim un-commits before dropping parts.
func archivePosFiles(a record.ArchivePos) []int {
	pos := make([]int, 0, len(a.Parts)+2)
	pos = append(pos, a.Commit.Pos)
	if a.Index != (record.FilePos{}) {
		pos = append(pos, a.Index.Pos)
	}
	for _, pt := range a.Parts {
		pos = append(pos, pt.Pos)
	}
	return pos
}
