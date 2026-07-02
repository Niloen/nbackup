package conductor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

var discardLog logf.Logf = func(string, ...any) {}

func shaOf(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// newCatalog opens an empty catalog on a temp workdir.
func newCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	return c
}

// addArchive records one archive on a medium, creating the run entry — the catalog's only write path,
// so a test builds run/placement state exactly as a real dump would.
func addArchive(t *testing.T, c *catalog.Catalog, id, medium, dle string, level int, arch record.Archive) {
	t.Helper()
	arch.Run, arch.DLE, arch.Level = id, dle, level
	if arch.CreatedAt.IsZero() {
		arch.CreatedAt = time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC)
	}
	pos := record.ArchivePos{DLE: dle, Level: level, Parts: []record.FilePos{{Pos: 1}}, Commit: record.FilePos{Pos: 2}}
	if err := c.AddArchive(arch, medium, pos); err != nil {
		t.Fatalf("AddArchive: %v", err)
	}
}

// fakeVol is a media.Volume whose Files()/RemoveFile() are the only methods allocRunID calls; the byte
// methods panic so a test proves allocRunID never touches them.
type fakeVol struct {
	files     []record.FileInfo
	filesErr  error
	removed   []int
	removeErr error
}

func (v *fakeVol) Files() ([]record.FileInfo, error) { return v.files, v.filesErr }
func (v *fakeVol) RemoveFile(pos int) error {
	if v.removeErr != nil {
		return v.removeErr
	}
	v.removed = append(v.removed, pos)
	return nil
}
func (v *fakeVol) AppendFile(context.Context, record.Header) (media.FileWriter, error) {
	panic("AppendFile: allocRunID must not write")
}
func (v *fakeVol) ReadFile(int) (record.Header, io.ReadCloser, error) {
	panic("ReadFile: allocRunID must not read payloads")
}

const runDay = "2026-07-02"

var day = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

// --- allocRunID branches ---------------------------------------------------

// TestAllocRunIDFirstOfDay: an empty catalog and empty drive give the day's first id.
func TestAllocRunIDFirstOfDay(t *testing.T) {
	c := &Conductor{d: Deps{Cat: newCatalog(t), Vol: &fakeVol{}}}
	id, seq, err := c.allocRunID(day)
	if err != nil || seq != 1 || id != record.IDFromParts(runDay, 1) {
		t.Fatalf("allocRunID = (%q,%d,%v); want the day's .001", id, seq, err)
	}
}

// TestAllocRunIDEmptyDriveTolerated: a changer with nothing loaded (ErrNoVolume from Files) is not a
// failure — the id is still allocated pool-globally from the catalog so a first dump can proceed.
func TestAllocRunIDEmptyDriveTolerated(t *testing.T) {
	c := &Conductor{d: Deps{Cat: newCatalog(t), Vol: &fakeVol{filesErr: media.ErrNoVolume}}}
	id, _, err := c.allocRunID(day)
	if err != nil || id != record.IDFromParts(runDay, 1) {
		t.Fatalf("allocRunID with empty drive = (%q,%v); want .001 tolerated", id, err)
	}
}

// TestAllocRunIDFilesErrorSurfaces: any Files() error other than ErrNoVolume is a hard failure.
func TestAllocRunIDFilesErrorSurfaces(t *testing.T) {
	boom := errors.New("scan boom")
	c := &Conductor{d: Deps{Cat: newCatalog(t), Vol: &fakeVol{filesErr: boom}}}
	if _, _, err := c.allocRunID(day); !errors.Is(err, boom) {
		t.Fatalf("allocRunID = %v; want the Files() error surfaced", err)
	}
}

// TestAllocRunIDSkipsSealedID: a sealed run already holds .001 (in the catalog), so a same-day rerun
// takes the next free .002 rather than shadowing it — even though the loaded volume carries nothing.
func TestAllocRunIDSkipsSealedID(t *testing.T) {
	c := newCatalog(t)
	addArchive(t, c, record.IDFromParts(runDay, 1), "disk", "h:/p", 0, record.Archive{})
	cd := &Conductor{d: Deps{Cat: c, Vol: &fakeVol{}}}
	id, seq, err := cd.allocRunID(day)
	if err != nil || seq != 2 || id != record.IDFromParts(runDay, 2) {
		t.Fatalf("allocRunID = (%q,%d,%v); want .002 after a sealed .001", id, seq, err)
	}
}

// orphanFiles builds a loaded-volume Files() listing for an uncommitted orphan of id: an archive part
// with no commit footer (a failed attempt), so its id is reclaimable.
func orphanFiles(id string, pos int) []record.FileInfo {
	return []record.FileInfo{{Pos: pos, Header: record.Header{Run: id, Kind: record.KindArchive}}}
}

// TestAllocRunIDReclaimsOrphan: the loaded volume carries an uncommitted orphan the catalog never
// recorded; allocRunID reclaims its files and reuses the id.
func TestAllocRunIDReclaimsOrphan(t *testing.T) {
	id := record.IDFromParts(runDay, 1)
	vol := &fakeVol{files: orphanFiles(id, 7)}
	cd := &Conductor{d: Deps{Cat: newCatalog(t), Vol: vol}}
	got, seq, err := cd.allocRunID(day)
	if err != nil || seq != 1 || got != id {
		t.Fatalf("allocRunID = (%q,%d,%v); want the orphan's .001 reclaimed", got, seq, err)
	}
	if len(vol.removed) != 1 || vol.removed[0] != 7 {
		t.Fatalf("orphan files removed = %v; want [7]", vol.removed)
	}
}

// TestAllocRunIDCommittedOrphanNeverReused: an orphan that DID commit (a real recovery point) keeps
// its id — allocRunID skips to the next sequence rather than reclaiming a committed archive.
func TestAllocRunIDCommittedOrphanNeverReused(t *testing.T) {
	id := record.IDFromParts(runDay, 1)
	vol := &fakeVol{files: []record.FileInfo{{Pos: 3, Header: record.Header{Run: id, Kind: record.KindCommit}}}}
	cd := &Conductor{d: Deps{Cat: newCatalog(t), Vol: vol}}
	got, seq, err := cd.allocRunID(day)
	if err != nil || seq != 2 || got != record.IDFromParts(runDay, 2) {
		t.Fatalf("allocRunID = (%q,%d,%v); want .002 (a committed id is never reused)", got, seq, err)
	}
	if len(vol.removed) != 0 {
		t.Fatalf("a committed archive must never be removed, removed = %v", vol.removed)
	}
}

// TestAllocRunIDNoFileRemovalSkips: a medium that cannot delete an individual file (tape) leaves the
// orphan in place and takes the next id instead of failing.
func TestAllocRunIDNoFileRemovalSkips(t *testing.T) {
	id := record.IDFromParts(runDay, 1)
	vol := &fakeVol{files: orphanFiles(id, 7), removeErr: media.ErrNoFileRemoval}
	cd := &Conductor{d: Deps{Cat: newCatalog(t), Vol: vol}}
	got, seq, err := cd.allocRunID(day)
	if err != nil || seq != 2 || got != record.IDFromParts(runDay, 2) {
		t.Fatalf("allocRunID = (%q,%d,%v); want .002 when the orphan can't be removed", got, seq, err)
	}
}

// TestAllocRunIDRemoveErrorSurfaces: a real removal error (not ErrNoFileRemoval) is a hard failure.
func TestAllocRunIDRemoveErrorSurfaces(t *testing.T) {
	id := record.IDFromParts(runDay, 1)
	boom := errors.New("remove boom")
	vol := &fakeVol{files: orphanFiles(id, 7), removeErr: boom}
	cd := &Conductor{d: Deps{Cat: newCatalog(t), Vol: vol}}
	if _, _, err := cd.allocRunID(day); !errors.Is(err, boom) {
		t.Fatalf("allocRunID = %v; want the removal error surfaced", err)
	}
}

// --- run.go backdated-run guard --------------------------------------------

// TestRunRejectsBackdatedRun is the data-integrity guard: a run dated earlier than one already sealed
// would splice an out-of-order archive into an incremental chain whose snapshot has advanced past it,
// silently dropping files at restore — so Run refuses it before doing any work.
func TestRunRejectsBackdatedRun(t *testing.T) {
	c := newCatalog(t)
	// A run already sealed for tomorrow.
	addArchive(t, c, record.IDFromParts("2026-07-03", 1), "disk", "h:/p", 0, record.Archive{})
	cd := &Conductor{d: Deps{Cat: c}}
	// Dump for today (before the sealed run) — must be rejected.
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.Local)
	_, err := cd.Run(context.Background(), now, discardLog)
	if err == nil || !strings.Contains(err.Error(), "corrupt the incremental restore order") {
		t.Fatalf("Run = %v; want the backdated-run rejection", err)
	}
}

// --- flush.go double-crash path --------------------------------------------

// memFlushVol / memFlushStore let OpenLanding hand back a real Author for the happy-path flush.
type memFlushVol struct {
	hdrs map[int]record.Header
	data map[int][]byte
	next int
}

func newFlushVol() *memFlushVol {
	return &memFlushVol{hdrs: map[int]record.Header{}, data: map[int][]byte{}}
}
func (v *memFlushVol) AppendFile(ctx context.Context, h record.Header) (media.FileWriter, error) {
	return &flushFW{v: v, ctx: ctx, h: h}, nil
}

type flushFW struct {
	v   *memFlushVol
	ctx context.Context
	h   record.Header
	buf bytes.Buffer
	pos int
}

func (w *flushFW) Pos() int                    { return w.pos }
func (w *flushFW) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *flushFW) Close() error {
	if w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	pos := w.v.next
	w.v.next++
	w.v.hdrs[pos] = w.h
	w.v.data[pos] = append([]byte(nil), w.buf.Bytes()...)
	w.pos = pos
	return nil
}
func (v *memFlushVol) ReadFile(pos int) (record.Header, io.ReadCloser, error) {
	return v.hdrs[pos], io.NopCloser(bytes.NewReader(v.data[pos])), nil
}
func (v *memFlushVol) Files() ([]record.FileInfo, error) { return nil, nil }
func (v *memFlushVol) RemoveFile(int) error              { return nil }

type memFlushStore struct{ vol *memFlushVol }

func (s *memFlushStore) NextPart() (media.Volume, int64, string, int, error) {
	return s.vol, -1, "L", 1, nil
}
func (s *memFlushStore) PlaceRecord(int64) (media.Volume, string, int, error) {
	return s.vol, "L", 1, nil
}
func (s *memFlushStore) Bounded() bool                       { return false }
func (s *memFlushStore) Record(archiveio.CommitResult) error { return nil }

// TestFlushDoubleCrashReclaimsOnly is the crash-in-the-crash case: a previous flush recorded the
// landing placement but died before reclaiming the holding copy, so the archive is on BOTH media.
// The next Flush must take the reclaim-only path — never re-copying (which would double-write the
// landing) — and simply drop the stale holding copy.
func TestFlushDoubleCrashReclaimsOnly(t *testing.T) {
	c := newCatalog(t)
	id := record.IDFromParts(runDay, 1)
	// The same archive recorded on both the holding disk and its landing.
	addArchive(t, c, id, "hd0", "h:/p", 0, record.Archive{})
	addArchive(t, c, id, "landing", "h:/p", 0, record.Archive{})

	opened := false
	reclaimed := 0
	flushed, err := Flush(FlushDeps{
		Cat:        c,
		LandingFor: func(string) string { return "landing" },
		Holdings:   []string{"hd0"},
		Open: func(string, string, int, string) (io.ReadCloser, error) {
			opened = true
			return nil, errors.New("Open must not be called on the reclaim-only path")
		},
		Members: func(string, string, int) ([]string, error) { return nil, nil },
		Reclaim: func(string, string, string, record.ArchivePos) error { reclaimed++; return nil },
		OpenLanding: func(string, archiveio.RunSpec) (*archiveio.Author, error) {
			return nil, errors.New("OpenLanding must not be called on the reclaim-only path")
		},
		DisplayDLE: func(dle string) string { return dle },
	})
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if opened {
		t.Fatal("Flush re-opened the holding archive; the double-crash case must reclaim only, not re-copy")
	}
	if reclaimed != 1 || flushed != 1 {
		t.Fatalf("reclaimed=%d flushed=%d; want the stale holding copy reclaimed exactly once", reclaimed, flushed)
	}
}

// TestFlushCopiesAndReclaims is the normal amflush-on-next path: an archive staged on a holding disk
// but not yet on the landing is copied to the landing (identity/checksum preserved) and its holding
// copy reclaimed.
func TestFlushCopiesAndReclaims(t *testing.T) {
	c := newCatalog(t)
	id := record.IDFromParts(runDay, 1)
	body := []byte("staged but never flushed")
	addArchive(t, c, id, "hd0", "h:/p", 0, record.Archive{SHA256: shaOf(body), Compress: "none", Uncompressed: int64(len(body))})

	reclaimed := 0
	flushed, err := Flush(FlushDeps{
		Cat:        c,
		LandingFor: func(string) string { return "landing" },
		Holdings:   []string{"hd0"},
		Open: func(runID, dle string, level int, medium string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		},
		Members: func(string, string, int) ([]string, error) { return nil, nil },
		Reclaim: func(string, string, string, record.ArchivePos) error { reclaimed++; return nil },
		OpenLanding: func(landing string, spec archiveio.RunSpec) (*archiveio.Author, error) {
			return archiveio.NewAuthor(&memFlushStore{vol: newFlushVol()}, spec, nil, nil), nil
		},
		DisplayDLE: func(dle string) string { return dle },
	})
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if flushed != 1 || reclaimed != 1 {
		t.Fatalf("flushed=%d reclaimed=%d; want the staged archive copied to the landing then reclaimed", flushed, reclaimed)
	}
}

// TestFlushNoHolding is the fast no-op: no holding media configured means nothing to flush.
func TestFlushNoHolding(t *testing.T) {
	n, err := Flush(FlushDeps{Cat: newCatalog(t)})
	if err != nil || n != 0 {
		t.Fatalf("Flush with no holding = (%d,%v); want (0,nil)", n, err)
	}
}
