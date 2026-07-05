package conductor

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/spool"
)

// flush.go is the amflush analogue: it drains a crashed run's leftover holding-disk archives to
// their landings on the next dump. The live drain (spool.Spool) handles a running dump; this is the
// recovery path for what a crash stranded — flushing leftovers is the run lane's pre-flight step,
// so it lives here, not in the spool. A holding-disk run records each archive's holding placement
// before flushing it and removes it after, so a crash leaves the un-flushed archives recorded on
// the holding medium in the catalog — Flush reads those placements (no medium scan) and drains
// them. Like the rest of the conductor it reaches the engine's clerk/librarian machinery through
// closures only.

// FlushDeps is what Flush needs from the host: the catalog it reads staged placements from, the
// holding medium names, and the host-bound seams — resolving a staged archive's landing route (its
// dumptype's `landing`, so a crashed multi-landing run drains each DLE back to its own media,
// primary first; re-derived from CURRENT config, never persisted), reading a staged archive's
// payload (Open) and member list (Members), reclaiming a staged archive
// (Reclaim), opening a landing writer for a (landing, run), and the DLE display id — plus an
// optional log. The engine binds Open/Reclaim to a write session on each holding disk (the same
// handle the live drain uses), so they are positional like the session's own read-back: the
// catalog's staged placement supplies ref, pos, and the index position.
type FlushDeps struct {
	Cat         *catalog.Catalog
	LandingsFor func(dle string) []string
	Holdings    []string
	Open        func(holding string, ref archiveio.Ref, pos archiveio.ArchivePos) (io.ReadCloser, error)
	// Index returns an archive's per-archive index (members + frame table), given where it
	// sits on the holding — the host serves it from its index cache when it can, else reads
	// the index there.
	Index       func(holding string, ref archiveio.Ref, index archiveio.FilePos) (record.Index, error)
	Reclaim     func(holding string, ref archiveio.Ref, pos archiveio.ArchivePos) error
	OpenLanding func(landing string, spec archiveio.RunSpec) (*archiveio.Writer, error)
	DisplayDLE  func(dle string) string
	Logf        func(format string, args ...any)
}

// Flush drains a crashed run's leftover archives from the holding disks to their landings. It reads
// the stranded holding placements from the catalog (no medium scan), copies each archive to its
// landing, removes the holding placement, reclaims the disk, and seals the run. It is idempotent
// and a no-op when no holding disk is configured or nothing is staged.
func Flush(d FlushDeps) (flushed int, err error) {
	logf := d.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(d.Holdings) == 0 {
		return 0, nil
	}
	// Collect the union of runs staged across the holding disks — a single crashed run may have
	// placements spread over several of them. Drain each run once (one landing session per landing,
	// one seal), copying every holding disk's portion of it.
	runSet := map[string]*catalog.Run{}
	for _, h := range d.Holdings {
		for _, s := range d.Cat.RunsOn(h) {
			runSet[s.ID] = s
		}
	}
	if len(runSet) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(runSet))
	for id := range runSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		s := runSet[id]
		spec := archiveio.RunSpec{ID: s.ID, CreatedAt: s.LastArchiveAt()}
		// One landing writer per landing this run's staged archives route to, opened lazily — a
		// multi-landing run may have staged DLEs bound for different media onto one holding disk.
		writers := map[string]*archiveio.Writer{}
		writerFor := func(landing string) (*archiveio.Writer, error) {
			if w, ok := writers[landing]; ok {
				return w, nil
			}
			w, err := d.OpenLanding(landing, spec)
			if err != nil {
				return nil, fmt.Errorf("flush %s: open landing %q: %w", s.ID, landing, err)
			}
			writers[landing] = w
			return w, nil
		}

		for _, holding := range d.Holdings {
			hp, ok := placementOn(d.Cat, s.ID, holding)
			if !ok {
				continue
			}
			for _, ap := range hp.Archives {
				ref := archiveio.Ref{Run: s.ID, DLE: ap.DLE, Level: ap.Level}
				dleID := d.DisplayDLE(ap.DLE)
				landings := d.LandingsFor(ap.DLE)
				for _, landing := range landings {
					// A crash between recording a landing placement and reclaiming the holding one
					// leaves an archive on both; a crash mid-fan-out leaves some landings served and
					// some not. Copy only to the landings still missing the archive — then reclaim
					// once, below, after the whole route is served.
					if archiveOnLanding(d.Cat, landing, s.ID, ap.DLE, ap.Level) {
						continue
					}
					arch, err := catalogArchive(d.Cat, s.ID, ap.DLE, ap.Level)
					if err != nil {
						return flushed, fmt.Errorf("flush %s %s: %w", s.ID, dleID, err)
					}
					// Best-effort like the live drain's member cache: a copy without its member
					// list is still restorable, just not browsable, so an unreadable index does
					// not fail the flush.
					idx, _ := d.Index(holding, ref, ap.Index)
					arch.Members, arch.Frames = idx.Members, idx.Frames
					if !arch.Shape.Resplittable() {
						// The holding stages atoms in final shape; its placement's seals
						// drive the landing copy's 1:1 atom carriage.
						arch.PartSeals = ap.Seals
					}
					landingWriter, err := writerFor(landing)
					if err != nil {
						return flushed, err
					}
					// CopyStaged (shared with the live drain) opens the staged payload and streams it
					// into the copy writer, whose Commit records the landing placement.
					label := fmt.Sprintf("flush %s %s", s.ID, dleID)
					open := func() (io.ReadCloser, error) { return d.Open(holding, ref, ap.Pos()) }
					if err := spool.CopyStaged(context.Background(), label, open, landingWriter.NewCopy(arch), landing); err != nil {
						return flushed, err
					}
				}
				// Every landing on the route holds the archive now (an error above returned
				// early, keeping it staged), so the staged copy can go.
				if err := d.Reclaim(holding, ref, ap.Pos()); err != nil {
					return flushed, fmt.Errorf("flush %s %s: reclaim holding disk: %w", s.ID, dleID, err)
				}
				flushed++
				logf("flushed %s %s to %q", s.ID, dleID, strings.Join(landings, ", "))
			}
		}
	}
	return flushed, nil
}

// placementOn returns the run's placement on the named medium, if any.
func placementOn(cat *catalog.Catalog, runID, medium string) (catalog.Placement, bool) {
	for _, p := range cat.Placements(runID) {
		if p.Medium == medium {
			return p, true
		}
	}
	return catalog.Placement{}, false
}

// archiveOnLanding reports whether the run's landing placement already holds (dle, level).
func archiveOnLanding(cat *catalog.Catalog, landing, runID, dle string, level int) bool {
	p, ok := placementOn(cat, runID, landing)
	if !ok {
		return false
	}
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return true
		}
	}
	return false
}

// catalogArchive returns a holding-disk archive's catalogued record (checksum, sizes,
// scheme); the caller fills Members through the d.Members seam.
func catalogArchive(cat *catalog.Catalog, runID, dle string, level int) (record.Archive, error) {
	s, err := cat.ReadRun(runID)
	if err != nil {
		return record.Archive{}, err
	}
	for _, a := range s.Archives {
		if a.DLE == dle && a.Level == level {
			return a, nil
		}
	}
	return record.Archive{}, fmt.Errorf("archive %s L%d not in catalog", dle, level)
}
