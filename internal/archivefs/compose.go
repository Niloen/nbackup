package archivefs

import (
	"io"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// compose.go is the fs's write side: a Session over a run on one medium. The Session is that
// medium's WriteStore — the fs write face: a Recorder (Record writes the placement to the
// catalog) plus OpenArchive/Reclaim, the drain's read-back and reclaim of a staged archive. It
// holds no allocator: part placement is the device side's business (the opened WriteMedium's
// librarian Allocator), which a writer is bound to separately — archiveio.NewWriter(alloc,
// session, …). The engine builds one for the serial CopyRun/Flush paths, the spool builds its
// own over routed seams. The fs never sees a scheme or a transfer; it records whatever writer
// is driven over its medium.

// Medium is the write session's slice of its opened medium: its identity (where the
// catalog placement pins) and its volume (the staged-archive read-back on a holding
// disk). The engine's opened write face (librarian.WriteMedium) satisfies it — a
// Session holds the handle the run opened, not a loose name, so "which medium" has one
// source of truth.
type Medium interface {
	Name() string
	Volume() media.Volume
}

// Session is one run's write handle on its medium and is its WriteStore. w is the
// catalog's write face the session records into; m identifies the opened medium and
// serves the volume reads on a single-volume medium (a holding disk) — a landing's
// volume is never read back through OpenArchive/Reclaim.
type Session struct {
	fs    *FS
	w     WriteMap
	m     Medium
	runID string // the run tag every archive carries (for the catalog + member index)
}

var _ WriteStore = (*Session)(nil)

// OpenRun starts a write session for run runID on the opened medium m, recording into w.
// It builds no writer — a writer is bound with archiveio.NewWriter(alloc, session, …),
// where alloc is the opened medium's part allocator (the engine does so for a dump/copy;
// the spool routes both seams onto its orchestrator and builds its own).
func (c *FS) OpenRun(w WriteMap, m Medium, runID string) *Session {
	return &Session{fs: c, w: w, m: m, runID: runID}
}

// Record commits one finished archive's placement onto this session's medium: it caches the member
// index and adds the archive to the catalog. A writer bound to this session calls it from Commit once
// the record is assembled — the single catalog write per archive, run wherever the writer runs it
// (inline for a serial run, on the orchestrator when the spool has routed it).
func (s *Session) Record(r archiveio.CommitResult) error {
	arch := r.Archive
	if len(arch.Members) > 0 {
		// Key the member index on the archive's own run, not the session's, so a session that carries
		// archives from several source runs (a cross-run sync through one spool) files each under the
		// right run. For a dump or a per-run copy the two are identical, so this changes nothing there.
		_ = s.fs.mindex.Store(arch.Run, arch.DLE, arch.Level, arch.Members)
	}
	return s.w.AddArchive(arch, s.m.Name(), r.Pos)
}

// OpenArchive reads a committed archive's payload back by concatenating its parts straight off the
// session's volume (whose index the producer keeps current) — the drain's read seam, for copying a
// staged archive to the landing.
func (s *Session) OpenArchive(arch record.Archive, pos record.ArchivePos) (io.ReadCloser, error) {
	open := func(p record.FilePos) (record.Header, io.ReadCloser, error) { return s.m.Volume().ReadFile(p.Pos) }
	return archiveio.NewReader(open, nil).Open(record.Ref{Run: s.runID, DLE: arch.DLE, Level: arch.Level}, pos.Parts)
}

// Reclaim drops a staged archive once it has landed on the landing; see FS.ReclaimStaged.
func (s *Session) Reclaim(arch record.Archive, pos record.ArchivePos) error {
	return s.fs.ReclaimStaged(s.w, s.m.Name(), s.m.Volume(), s.runID, arch.DLE, pos)
}

// ReclaimStaged drops a staged archive from a holding medium once it has landed: it removes the
// archive's files from vol (the commit footer first, so an interrupted reclaim un-commits before
// dropping parts) then drops its placement from the catalog via w. The live drain reaches it
// through Session.Reclaim; the crash-recovery flush (conductor.Flush) calls it directly — the
// footer-first invariant lives only here.
func (c *FS) ReclaimStaged(w WriteMap, medium string, vol media.Volume, runID, dle string, pos record.ArchivePos) error {
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
