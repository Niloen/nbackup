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
	pos := archiveio.ArchivePos{Parts: []archiveio.FilePos{{Pos: 1}}, Commit: archiveio.FilePos{Pos: 2}}
	if err := c.AddArchive(arch, medium, pos); err != nil {
		t.Fatalf("AddArchive: %v", err)
	}
}

var day = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

// --- mintRunID branches ------------------------------------------------------

// TestMintRunIDFromClock: with no colliding run in the catalog, the id is the start
// instant's local wall clock, verbatim. Minting takes no volume — nothing here can
// scan a tape (the conductor's Deps has no media handle at all).
func TestMintRunIDFromClock(t *testing.T) {
	c := &Conductor{d: Deps{Cat: newCatalog(t)}}
	if id := c.mintRunID(day, time.UTC); id != "run-2026-07-02.120000" {
		t.Fatalf("mintRunID = %q; want the instant's own id", id)
	}
	// Existing earlier runs don't move a later instant's id.
	c2 := newCatalog(t)
	addArchive(t, c2, "run-2026-07-02.020000", "disk", "h:/p", 0, record.Archive{})
	cd := &Conductor{d: Deps{Cat: c2}}
	if id := cd.mintRunID(day, time.UTC); id != "run-2026-07-02.120000" {
		t.Fatalf("mintRunID = %q; want the clock id, unmoved by earlier runs", id)
	}
}

// TestMintRunIDLocalZone: the id carries the instant's wall clock in the given zone,
// so the same instant mints different ids (and even dates) across zones.
func TestMintRunIDLocalZone(t *testing.T) {
	c := &Conductor{d: Deps{Cat: newCatalog(t)}}
	inst := time.Date(2026, 7, 2, 23, 30, 0, 0, time.UTC)
	if id := c.mintRunID(inst, time.FixedZone("east", 2*3600)); id != "run-2026-07-03.013000" {
		t.Fatalf("mintRunID east = %q; want the zone's own wall clock", id)
	}
}

// TestMintRunIDBumpsPastLatest is the monotonicity guard: an instant at or below the
// latest catalog id (a --date run pinned to midnight when the day already has runs, or
// a clock stepped backwards) mints one second past that id instead — never at or below
// it, and never reusing a pruned id's slot, so "run ids sort as time" holds.
func TestMintRunIDBumpsPastLatest(t *testing.T) {
	c := newCatalog(t)
	addArchive(t, c, "run-2026-07-02.140000", "disk", "h:/p", 0, record.Archive{})
	cd := &Conductor{d: Deps{Cat: c}}
	// 12:00 is behind the sealed 14:00 run: bump to 14:00:01.
	if id := cd.mintRunID(day, time.UTC); id != "run-2026-07-02.140001" {
		t.Fatalf("mintRunID behind latest = %q; want one second past the latest id", id)
	}
	// The exact same second collides: same bump.
	inst := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	if id := cd.mintRunID(inst, time.UTC); id != "run-2026-07-02.140001" {
		t.Fatalf("mintRunID same second = %q; want one second past the latest id", id)
	}
}

// TestMintRunIDNonCanonicalLatest: a non-canonical id in the catalog cannot anchor
// the bump; the clock-minted id is used rather than failing the run.
func TestMintRunIDNonCanonicalLatest(t *testing.T) {
	c := newCatalog(t)
	addArchive(t, c, "zz-not-a-run-id", "disk", "h:/p", 0, record.Archive{})
	cd := &Conductor{d: Deps{Cat: c}}
	if id := cd.mintRunID(day, time.UTC); id != "run-2026-07-02.120000" {
		t.Fatalf("mintRunID with junk latest = %q; want the clock id", id)
	}
}

// --- run.go backdated-run guard --------------------------------------------

// TestRunRejectsBackdatedRun is the data-integrity guard: a run dated earlier than one already sealed
// would splice an out-of-order archive into an incremental chain whose snapshot has advanced past it,
// silently dropping files at restore — so Run refuses it before doing any work.
func TestRunRejectsBackdatedRun(t *testing.T) {
	c := newCatalog(t)
	// A run already sealed for tomorrow.
	addArchive(t, c, "run-2026-07-03.020000", "disk", "h:/p", 0, record.Archive{})
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
func (s *memFlushStore) PlaceFile(int64) (media.Volume, string, int, error) {
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
	id := "run-2026-07-02.020000"
	// The same archive recorded on both the holding disk and its landing.
	addArchive(t, c, id, "hd0", "h:/p", 0, record.Archive{})
	addArchive(t, c, id, "landing", "h:/p", 0, record.Archive{})

	opened := false
	reclaimed := 0
	flushed, err := Flush(FlushDeps{
		Cat:        c,
		LandingFor: func(string) string { return "landing" },
		Holdings:   []string{"hd0"},
		Open: func(string, archiveio.Ref, archiveio.ArchivePos) (io.ReadCloser, error) {
			opened = true
			return nil, errors.New("Open must not be called on the reclaim-only path")
		},
		Members: func(string, archiveio.Ref, archiveio.FilePos) ([]string, error) { return nil, nil },
		Reclaim: func(string, archiveio.Ref, archiveio.ArchivePos) error { reclaimed++; return nil },
		OpenLanding: func(string, archiveio.RunSpec) (*archiveio.Writer, error) {
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
	id := "run-2026-07-02.020000"
	body := []byte("staged but never flushed")
	addArchive(t, c, id, "hd0", "h:/p", 0, record.Archive{SHA256: shaOf(body), Compress: "none", Uncompressed: int64(len(body))})

	reclaimed := 0
	flushed, err := Flush(FlushDeps{
		Cat:        c,
		LandingFor: func(string) string { return "landing" },
		Holdings:   []string{"hd0"},
		Open: func(string, archiveio.Ref, archiveio.ArchivePos) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		},
		Members: func(string, archiveio.Ref, archiveio.FilePos) ([]string, error) { return nil, nil },
		Reclaim: func(string, archiveio.Ref, archiveio.ArchivePos) error { reclaimed++; return nil },
		OpenLanding: func(landing string, spec archiveio.RunSpec) (*archiveio.Writer, error) {
			ms := &memFlushStore{vol: newFlushVol()}
			return archiveio.NewWriter(ms, ms, spec, nil, nil), nil
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
