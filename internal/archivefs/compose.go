package archivefs

import (
	"io"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// compose.go is the fs's write side: a Session over a run on one medium. The Session is that
// medium's WriteStore — the fs write face: a Recorder (Record writes the placement to the
// catalog) plus OpenArchiveAt/ReclaimAt, the drain's read-back and reclaim of a staged archive. It
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
// volume is never read back through OpenArchiveAt/ReclaimAt.
type Session struct {
	fs *FS
	w  WriteMap
	m  Medium
}

var _ WriteStore = (*Session)(nil)

// OpenRun starts a write session on the opened medium m, recording into w. The run
// identity rides on each archive's own tag (a session may even carry archives from
// several source runs — a cross-run sync), so the session holds none. It builds no
// writer — a writer is bound with archiveio.NewWriter(alloc, session, …), where alloc
// is the opened medium's part allocator (the engine does so for a dump/copy; the
// spool routes both seams onto its orchestrator and builds its own).
func (fs *FS) OpenRun(w WriteMap, m Medium) *Session {
	return &Session{fs: fs, w: w, m: m}
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

// OpenArchiveAt reads a staged archive's payload back by concatenating the parts at pos straight off
// the session's volume (whose index the producer keeps current) — the drain's read seam, for copying
// a staged archive to the landing. It is positional: no catalog resolution, just ref (asserted
// against each part's header) and pos. ref carries the archive's own run, matching Record's
// per-archive keying so a session carrying archives from several source runs (a cross-run sync) reads
// each under the right run.
func (s *Session) OpenArchiveAt(ref archiveio.Ref, pos archiveio.ArchivePos) (io.ReadCloser, error) {
	open := func(p archiveio.FilePos) (record.Header, io.ReadCloser, error) { return s.m.Volume().ReadFile(p.Pos) }
	// Positional read-back reads unsealed (pos carries no seals); the drain's NewCopy
	// verifies the whole-archive checksum at commit, which covers this path.
	return archiveio.NewReader(open, nil).Open(ref, pos.Parts, nil)
}

// ReclaimAt deletes one archive's copy on the session's medium: it removes the archive's
// files from the volume (the commit footer first, so an interrupted reclaim un-commits
// before dropping parts) then drops its placement from the catalog. It is how any
// archive's copy dies on a per-file medium — the drain and the crash-recovery flush drop
// a staged archive once it has landed, prune reclaims retention-expired archives, and a
// forced re-copy reclaims the copy it supersedes — so the footer-first invariant lives
// only here. Positional like the read-back: ref names the archive, pos its files.
func (s *Session) ReclaimAt(ref archiveio.Ref, pos archiveio.ArchivePos) error {
	for _, p := range archivePosFiles(pos) {
		if err := s.m.Volume().RemoveFile(p); err != nil {
			return err
		}
	}
	_, _, err := s.w.RemoveArchive(ref.Run, s.m.Name(), ref.DLE)
	return err
}

// archivePosFiles lists an archive's file positions in safe removal order: the commit
// footer first, then the member index, then the parts.
//
// The order is crash-safety-critical and mirrors the write order in reverse. An archive
// is made durable by its commit footer, written LAST (after its parts and index); the
// footer's presence is what proves the whole archive landed, and a catalog rebuild
// assembles only archives that have a footer (assemble iterates the commits — parts
// without one are orphans it ignores). So removing the footer FIRST "un-commits" the
// archive: a crash mid-reclaim then leaves parts/index as orphans with no footer, which a
// rebuild skips. Removing parts first would leave a footer whose parts are gone — which a
// rebuild would resurrect into the catalog as a committed-but-unreadable archive (the
// exact "we think it's committed but it's only partly there" hazard). Removal is one
// os.Remove per file, so the ordering holds at the same level the write path relies on
// (no fsync either side).
func archivePosFiles(a archiveio.ArchivePos) []int {
	pos := make([]int, 0, len(a.Parts)+2)
	pos = append(pos, a.Commit.Pos)
	if a.Index != (archiveio.FilePos{}) {
		pos = append(pos, a.Index.Pos)
	}
	for _, pt := range a.Parts {
		pos = append(pos, pt.Pos)
	}
	return pos
}
