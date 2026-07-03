package clerk

import (
	"io"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// compose.go is the clerk's write side: a Session over a run on one medium. The Session is that
// medium's archiveio.Store — a WriteStore (NextPart/PlaceRecord/Bounded forward to the medium's librarian,
// Record writes the placement to the catalog) plus OpenArchive/Reclaim, the drain's read-back and
// reclaim of a staged archive. It is not a writer factory: a writer is built over it with
// archiveio.NewAuthor — the engine builds one for the serial CopyRun/Flush paths, the spool builds its
// own over a routed wrapper. The clerk never sees a scheme or a transfer; it takes plain bytes through
// whatever writer is driven over it.

// Session authors one run onto medium and is its archiveio.Store. w is the catalog's write
// face the session records into (journaled while a run window is open). vol is the medium's
// volume, used to read and reclaim staged archives on a single-volume medium (a holding
// disk); a landing passes its loaded volume, which OpenArchive/Reclaim are never called on.
type Session struct {
	clerk  *Clerk
	sink   archiveio.VolumeSink // the medium's librarian — the VolumeSink half of WriteStore
	w      Writer
	medium string
	vol    media.Volume
	runID  string // the run tag every archive carries (for the catalog + member index)
}

var _ archiveio.Store = (*Session)(nil)

// OpenRun starts a write session for run runID on medium over the medium's librarian sink,
// recording into w. It builds no writer — a writer is authored over the returned Session
// with archiveio.NewAuthor (the engine does so for a dump/copy; the spool wraps the Session
// and builds its own).
func (c *Clerk) OpenRun(sink archiveio.VolumeSink, w Writer, medium string, vol media.Volume, runID string) *Session {
	return &Session{clerk: c, sink: sink, w: w, medium: medium, vol: vol, runID: runID}
}

// NextPart, PlaceRecord, Bounded and Record are the Session's WriteStore — the real, inline operations a
// writer built over this session runs. NextPart and PlaceRecord forward to the medium's librarian (it
// owns the changer and the roll); Bounded is a constant. The spool wraps these in a routing WriteStore so
// they run on its orchestrator; driven serially (CopyRun/Flush) they run on the caller's goroutine.
func (s *Session) NextPart() (media.Volume, int64, string, int, error) { return s.sink.NextPart() }
func (s *Session) PlaceRecord(size int64) (media.Volume, string, int, error) {
	return s.sink.PlaceRecord(size)
}
func (s *Session) Bounded() bool { return s.sink.Bounded() }

// Record commits one finished archive's placement onto this session's medium: it caches the member
// index and adds the archive to the catalog. A writer built over this session calls it from Commit once
// the record is assembled — the single catalog write per archive, run wherever the writer runs it
// (inline for a serial run, on the orchestrator when the spool has routed it).
func (s *Session) Record(r archiveio.CommitResult) error {
	arch := r.Archive
	if len(arch.Members) > 0 {
		// Key the member index on the archive's own run, not the session's, so a session that carries
		// archives from several source runs (a cross-run sync through one spool) files each under the
		// right run. For a dump or a per-run copy the two are identical, so this changes nothing there.
		_ = s.clerk.mindex.Store(arch.Run, arch.DLE, arch.Level, arch.Members)
	}
	return s.w.AddArchive(arch, s.medium, r.Pos)
}

// OpenArchive reads a committed archive's payload back by concatenating its parts straight off the
// session's volume (whose index the producer keeps current) — the drain's read seam, for copying a
// staged archive to the landing.
func (s *Session) OpenArchive(arch record.Archive, pos record.ArchivePos) (io.ReadCloser, error) {
	exp := archiveio.Ref{Run: s.runID, DLE: arch.DLE, Level: arch.Level}
	return archiveio.NewReader().Open(pos.Parts, exp,
		func(p record.FilePos) (record.Header, io.ReadCloser, error) { return s.vol.ReadFile(p.Pos) })
}

// Reclaim drops a staged archive once it has landed on the landing; see Clerk.ReclaimStaged.
func (s *Session) Reclaim(arch record.Archive, pos record.ArchivePos) error {
	return s.clerk.ReclaimStaged(s.w, s.medium, s.vol, s.runID, arch.DLE, pos)
}

// ReclaimStaged drops a staged archive from a holding medium once it has landed: it removes the
// archive's files from vol (the commit footer first, so an interrupted reclaim un-commits before
// dropping parts) then drops its placement from the catalog via w. The live drain reaches it
// through Session.Reclaim; the crash-recovery flush (conductor.Flush) calls it directly — the
// footer-first invariant lives only here.
func (c *Clerk) ReclaimStaged(w Writer, medium string, vol media.Volume, runID, dle string, pos record.ArchivePos) error {
	for _, p := range archivePosFiles(pos) {
		if err := vol.RemoveFile(p); err != nil {
			return err
		}
	}
	_, _, err := w.RemoveArchive(runID, medium, dle)
	return err
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
