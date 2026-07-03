package archivefs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// --- fakes -----------------------------------------------------------------

// fakeMap is the clerk's Map (its catalog slice): a fixed set of placements to resolve reads over,
// plus a record of the write calls Session makes, with injectable errors.
type fakeMap struct {
	placements []catalog.Placement

	added     []addCall
	addErr    error
	removed   []removeCall
	removeErr error
}

type addCall struct {
	arch   record.Archive
	medium string
	pos    archiveio.ArchivePos
}
type removeCall struct{ runID, medium, dle string }

func (m *fakeMap) PlacementsFor(string) []catalog.Placement { return m.placements }
func (m *fakeMap) AddArchive(arch record.Archive, medium string, pos archiveio.ArchivePos) error {
	if m.addErr != nil {
		return m.addErr
	}
	m.added = append(m.added, addCall{arch, medium, pos})
	return nil
}
func (m *fakeMap) RemoveArchive(runID, medium, dle string) (bool, bool, error) {
	if m.removeErr != nil {
		return false, false, m.removeErr
	}
	m.removed = append(m.removed, removeCall{runID, medium, dle})
	return true, true, nil
}

// memMounter is a Mounter over an in-memory position→file map. A position listed in failPos returns
// err instead, standing in for a damaged/absent file so the clerk's copy fail-over is exercised.
type memMounter struct {
	files   map[int]memFile
	failPos map[int]bool
	err     error // returned by every ReadFileAt when set
}
type memFile struct {
	h    record.Header
	data []byte
}

func (m *memMounter) ReadFileAt(_ string, _, pos int) (record.Header, io.ReadCloser, error) {
	if m.err != nil {
		return record.Header{}, nil, m.err
	}
	if m.failPos[pos] {
		return record.Header{}, nil, errors.New("simulated read fault at pos")
	}
	f, ok := m.files[pos]
	if !ok {
		return record.Header{}, nil, errors.New("no file at pos")
	}
	return f.h, io.NopCloser(bytes.NewReader(f.data)), nil
}

// fakeDeps hands out a Mounter per medium (or an error for an unknown one).
type fakeDeps struct {
	mounters map[string]*memMounter
	mountErr map[string]error
}

func (d *fakeDeps) MounterFor(medium string) (Mounter, error) {
	if err := d.mountErr[medium]; err != nil {
		return nil, err
	}
	m, ok := d.mounters[medium]
	if !ok {
		return nil, errors.New("no mounter for medium " + medium)
	}
	return m, nil
}
func (d *fakeDeps) Limiter(string) *ratelimit.Limiter { return nil }

// memVol is an in-memory media.Volume backing a write Session (author + read-back + reclaim). It
// records reclaimed positions in order so a test can assert the footer-first reclaim invariant.
type memVol struct {
	hdrs    map[int]record.Header
	data    map[int][]byte
	next    int
	removed []int
	rmErr   error // RemoveFile fault (crash mid-reclaim)
	rmAfter int   // fault only once this many removals have happened
}

func newMemVol() *memVol { return &memVol{hdrs: map[int]record.Header{}, data: map[int][]byte{}} }

func (v *memVol) AppendFile(ctx context.Context, h record.Header) (media.FileWriter, error) {
	return &memFW{v: v, ctx: ctx, h: h}, nil
}

type memFW struct {
	v   *memVol
	ctx context.Context
	h   record.Header
	buf bytes.Buffer
	pos int
}

func (w *memFW) Pos() int                    { return w.pos }
func (w *memFW) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *memFW) Close() error {
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

func (v *memVol) ReadFile(pos int) (record.Header, io.ReadCloser, error) {
	d, ok := v.data[pos]
	if !ok {
		return record.Header{}, nil, errors.New("no file")
	}
	return v.hdrs[pos], io.NopCloser(bytes.NewReader(d)), nil
}
func (v *memVol) Files() ([]record.FileInfo, error) { return nil, nil }
func (v *memVol) RemoveFile(pos int) error {
	if v.rmErr != nil && len(v.removed) >= v.rmAfter {
		return v.rmErr
	}
	v.removed = append(v.removed, pos)
	delete(v.hdrs, pos)
	delete(v.data, pos)
	return nil
}

// memWriteStore is a minimal allocator+recorder over a memVol so a real archiveio.Writer can author an
// archive onto it (for the OpenArchiveAt/ReclaimAt read-back tests).
type memWriteStore struct{ vol *memVol }

func (s *memWriteStore) NextPart() (media.Volume, int64, string, int, error) {
	return s.vol, -1, "vol", 1, nil
}
func (s *memWriteStore) PlaceFile(int64) (media.Volume, string, int, error) {
	return s.vol, "vol", 1, nil
}
func (s *memWriteStore) Bounded() bool                       { return false }
func (s *memWriteStore) Record(archiveio.CommitResult) error { return nil }

// authorArchive writes one real archive (parts + commit footer) onto vol and returns its record and
// on-volume positions, so the read-back and reclaim paths run over genuine framed files.
func authorArchive(t *testing.T, vol *memVol, run, dle string, level int, body []byte) (record.Archive, archiveio.ArchivePos) {
	t.Helper()
	ws := &memWriteStore{vol: vol}
	a := archiveio.NewWriter(ws, ws, archiveio.RunSpec{ID: run, CreatedAt: time.Unix(0, 0).UTC()}, nil, nil)
	aw := a.NewArchive(archiveio.ArchiveSpec{DLE: dle, Host: "h", Path: "/p", Compress: "none", Level: level})
	if _, err := xfer.Transfer(context.Background(), xfer.Reader(io.NopCloser(bytes.NewReader(body))), xfer.NewFilters(), aw); err != nil {
		t.Fatalf("author transfer: %v", err)
	}
	res, ok := aw.Committed()
	if !ok {
		t.Fatal("archive did not commit")
	}
	return res.Archive, res.Pos
}

// --- Session write-side tests ----------------------------------------------

// TestSessionRecord covers the Session's single catalog write: it caches the member index and adds
// the archive to the catalog under the archive's own run tag.
func TestSessionRecord(t *testing.T) {
	m := &fakeMap{}
	mindex := catalog.OpenMemberIndex(t.TempDir())
	c := New(m, &fakeDeps{}, mindex)
	sess := c.OpenRun(m, fakeMedium{name: "disk"})

	arch := record.Archive{Run: "run-2026-07-02.001", DLE: "h:/p", Level: 0, Members: []string{"a", "b"}}
	pos := archiveio.ArchivePos{}
	if err := sess.Record(archiveio.CommitResult{Archive: arch, Pos: pos}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(m.added) != 1 || m.added[0].medium != "disk" || m.added[0].arch.DLE != "h:/p" {
		t.Fatalf("AddArchive not called as expected: %+v", m.added)
	}
	got, ok, err := mindex.Load("run-2026-07-02.001", "h:/p", 0)
	if err != nil || !ok || strings.Join(got, ",") != "a,b" {
		t.Fatalf("member index not cached: got=%v ok=%v err=%v", got, ok, err)
	}
}

// TestSessionOpenArchive authors a real archive onto the session's volume and reads it back through
// OpenArchiveAt (the drain's read seam), asserting the bytes round-trip.
func TestSessionOpenArchive(t *testing.T) {
	vol := newMemVol()
	body := []byte("staged archive payload")
	arch, pos := authorArchive(t, vol, "run-2026-07-02.001", "h:/p", 0, body)

	m := &fakeMap{}
	c := New(m, &fakeDeps{}, catalog.OpenMemberIndex(t.TempDir()))
	sess := c.OpenRun(m, fakeMedium{name: "hd0", vol: vol})
	rc, err := sess.OpenArchiveAt(archiveio.Ref{Run: arch.Run, DLE: arch.DLE, Level: arch.Level}, pos)
	if err != nil {
		t.Fatalf("OpenArchiveAt: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("read back %q; want %q", got, body)
	}
}

// TestReclaimStagedFooterFirst pins the crash-safe reclaim ordering: the commit footer (the marker)
// is removed first — so an interrupted reclaim un-commits before dropping the parts a scan would
// otherwise still assemble — then the index, then the parts, and only then the catalog placement.
func TestReclaimStagedFooterFirst(t *testing.T) {
	vol := newMemVol()
	arch, pos := authorArchive(t, vol, "run-2026-07-02.001", "h:/p", 0, []byte("body"))
	// Give the archive a synthetic index position so the footer/index/parts order is observable.
	pos.Index = archiveio.FilePos{Label: "vol", Pos: 999}

	m := &fakeMap{}
	c := New(m, &fakeDeps{}, catalog.OpenMemberIndex(t.TempDir()))
	sess := c.OpenRun(m, fakeMedium{name: "hd0", vol: vol})
	if err := sess.ReclaimAt(archiveio.Ref{Run: arch.Run, DLE: arch.DLE, Level: arch.Level}, pos); err != nil {
		t.Fatalf("ReclaimAt: %v", err)
	}
	if len(vol.removed) == 0 || vol.removed[0] != pos.Commit.Pos {
		t.Fatalf("removed order = %v; the commit footer (pos %d) must be removed first", vol.removed, pos.Commit.Pos)
	}
	if vol.removed[1] != 999 {
		t.Fatalf("removed order = %v; the index (pos 999) must follow the footer", vol.removed)
	}
	if len(m.removed) != 1 || m.removed[0].dle != "h:/p" {
		t.Fatalf("catalog RemoveArchive not called once after files dropped: %+v", m.removed)
	}
}

// TestReclaimStagedFileFaultKeepsCatalog is the crash-safety guarantee: if a file removal faults
// mid-reclaim, the catalog placement is left intact (RemoveArchive is never reached), so the staged
// archive stays fully recorded and the next flush retries rather than losing track of it.
func TestReclaimStagedFileFaultKeepsCatalog(t *testing.T) {
	vol := newMemVol()
	arch, pos := authorArchive(t, vol, "run-2026-07-02.001", "h:/p", 0, []byte("body"))
	vol.rmErr = errors.New("remove faulted")
	vol.rmAfter = 0 // fault on the very first RemoveFile (the commit footer)

	m := &fakeMap{}
	c := New(m, &fakeDeps{}, catalog.OpenMemberIndex(t.TempDir()))
	sess := c.OpenRun(m, fakeMedium{name: "hd0", vol: vol})
	if err := sess.ReclaimAt(archiveio.Ref{Run: arch.Run, DLE: arch.DLE, Level: arch.Level}, pos); err == nil {
		t.Fatal("ReclaimAt must surface the file-removal fault")
	}
	if len(m.removed) != 0 {
		t.Fatalf("catalog RemoveArchive ran despite a file-removal fault: %+v", m.removed)
	}
}

// --- read-side eachPlacement / Open tests ----------------------------------

func archiveHeader(run, dle string, level, part int) record.Header {
	return record.Header{Kind: record.KindArchive, Run: run, DLE: dle, Level: level, Part: part}
}

// placementWith builds a placement whose one archive has a single part at pos.
func placementWith(medium, run, dle string, level, pos int) catalog.Placement {
	return catalog.Placement{Medium: medium, Archives: []catalog.PlacedArchive{{
		DLE: dle, Level: level, Parts: []archiveio.FilePos{{Label: "v", Pos: pos}},
	}}}
}

// TestOpenMissingCopy covers the three missing-copy verdicts eachPlacement emits — all classified by
// errors.Is(ErrMissingCopy) regardless of wording.
func TestOpenMissingCopy(t *testing.T) {
	ref := archiveio.Ref{Run: "run-2026-07-02.001", DLE: "h:/p", Level: 0}
	t.Run("run not in catalog", func(t *testing.T) {
		c := New(&fakeMap{}, &fakeDeps{}, catalog.OpenMemberIndex(t.TempDir()))
		if _, err := c.Open(ref, ""); !errors.Is(err, ErrMissingCopy) {
			t.Fatalf("err = %v; want ErrMissingCopy", err)
		}
	})
	t.Run("no copy on the pinned medium", func(t *testing.T) {
		m := &fakeMap{placements: []catalog.Placement{placementWith("disk", ref.Run, ref.DLE, 0, 0)}}
		c := New(m, &fakeDeps{}, catalog.OpenMemberIndex(t.TempDir()))
		if _, err := c.Open(ref, "tape"); !errors.Is(err, ErrMissingCopy) {
			t.Fatalf("err = %v; want ErrMissingCopy for a medium with no copy", err)
		}
	})
	t.Run("placement present but archive absent", func(t *testing.T) {
		// A placement on the medium, but it carries a different DLE — Parts() misses, so no open is
		// attempted and lastErr stays nil: the final ErrMissingCopy fires.
		m := &fakeMap{placements: []catalog.Placement{placementWith("disk", ref.Run, "other:/x", 0, 0)}}
		c := New(m, &fakeDeps{}, catalog.OpenMemberIndex(t.TempDir()))
		if _, err := c.Open(ref, ""); !errors.Is(err, ErrMissingCopy) {
			t.Fatalf("err = %v; want ErrMissingCopy when the archive is absent from every copy", err)
		}
	})
}

// TestOpenFailsOver proves copy fail-over: the first placement's copy will not open (a damaged file),
// so the clerk tries the next eligible copy and returns its bytes rather than surfacing the fault.
func TestOpenFailsOver(t *testing.T) {
	ref := archiveio.Ref{Run: "run-2026-07-02.001", DLE: "h:/p", Level: 0}
	body := []byte("good copy bytes")
	deps := &fakeDeps{mounters: map[string]*memMounter{
		"broken": {files: map[int]memFile{5: {h: archiveHeader(ref.Run, ref.DLE, 0, 0), data: body}}, failPos: map[int]bool{5: true}},
		"good":   {files: map[int]memFile{7: {h: archiveHeader(ref.Run, ref.DLE, 0, 0), data: body}}},
	}}
	m := &fakeMap{placements: []catalog.Placement{
		placementWith("broken", ref.Run, ref.DLE, 0, 5),
		placementWith("good", ref.Run, ref.DLE, 0, 7),
	}}
	c := New(m, deps, catalog.OpenMemberIndex(t.TempDir()))
	rc, err := c.Open(ref, "")
	if err != nil {
		t.Fatalf("Open must fail over to the good copy, got %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("read %q; want %q from the good copy", got, body)
	}
}

// TestOpenPinnedMediumNotMasked confirms a medium-pinned read is confined to that medium: a fault on
// the pinned copy surfaces rather than being masked by a healthy copy on another medium.
func TestOpenPinnedMediumNotMasked(t *testing.T) {
	ref := archiveio.Ref{Run: "run-2026-07-02.001", DLE: "h:/p", Level: 0}
	body := []byte("bytes")
	deps := &fakeDeps{mounters: map[string]*memMounter{
		"broken": {files: map[int]memFile{5: {h: archiveHeader(ref.Run, ref.DLE, 0, 0), data: body}}, failPos: map[int]bool{5: true}},
		"good":   {files: map[int]memFile{7: {h: archiveHeader(ref.Run, ref.DLE, 0, 0), data: body}}},
	}}
	m := &fakeMap{placements: []catalog.Placement{
		placementWith("broken", ref.Run, ref.DLE, 0, 5),
		placementWith("good", ref.Run, ref.DLE, 0, 7),
	}}
	c := New(m, deps, catalog.OpenMemberIndex(t.TempDir()))
	if _, err := c.Open(ref, "broken"); err == nil {
		t.Fatal("a pinned read must surface its own copy's fault, not fail over to another medium")
	}
}

// TestReadArchivesOrdersReadsAndReportsMissing drives the one-pass read: two present archives are
// read (in level/position order) with their bytes available through the open callback, while a ref
// with no copy is returned as missing rather than read.
func TestReadArchivesOrdersReadsAndReportsMissing(t *testing.T) {
	run := "run-2026-07-02.001"
	body0, body1 := []byte("L0 bytes"), []byte("L1 bytes")
	deps := &fakeDeps{mounters: map[string]*memMounter{
		"disk": {files: map[int]memFile{
			10: {h: archiveHeader(run, "h:/p", 0, 0), data: body0},
			20: {h: archiveHeader(run, "h:/p", 1, 0), data: body1},
		}},
	}}
	pl := catalog.Placement{Medium: "disk", Archives: []catalog.PlacedArchive{
		{DLE: "h:/p", Level: 0, Parts: []archiveio.FilePos{{Label: "v", Pos: 10}}},
		{DLE: "h:/p", Level: 1, Parts: []archiveio.FilePos{{Label: "v", Pos: 20}}},
	}}
	c := New(&fakeMap{placements: []catalog.Placement{pl}}, deps, catalog.OpenMemberIndex(t.TempDir()))

	refs := []archiveio.Ref{
		{Run: run, DLE: "h:/p", Level: 1},
		{Run: run, DLE: "h:/p", Level: 0},
		{Run: run, DLE: "gone:/x", Level: 0}, // no copy
	}
	var gotLevels []int
	missing, err := c.ReadArchives(refs, "", func(ref archiveio.Ref, open func() (io.ReadCloser, error)) error {
		gotLevels = append(gotLevels, ref.Level)
		rc, e := open()
		if e != nil {
			return e
		}
		_, _ = io.ReadAll(rc)
		return rc.Close()
	})
	if err != nil {
		t.Fatalf("ReadArchives: %v", err)
	}
	if len(gotLevels) != 2 || gotLevels[0] != 0 || gotLevels[1] != 1 {
		t.Fatalf("read levels = %v; want ascending [0 1]", gotLevels)
	}
	if len(missing) != 1 || missing[0].DLE != "gone:/x" {
		t.Fatalf("missing = %v; want the one ref with no copy", missing)
	}
}

// --- Members fallback tests ------------------------------------------------

// TestMembersOnMediumFallback covers the lazy member-index path: a cache miss falls back to reading
// the on-medium index (located via the placement's recorded Index position), decodes it, and
// re-caches it so a second call hits the cache.
func TestMembersOnMediumFallback(t *testing.T) {
	ref := archiveio.Ref{Run: "run-2026-07-02.001", DLE: "h:/p", Level: 0}
	members := []string{"dir/", "dir/file"}
	var idxBuf bytes.Buffer
	if err := record.EncodeIndex(&idxBuf, members); err != nil {
		t.Fatalf("encode index: %v", err)
	}
	deps := &fakeDeps{mounters: map[string]*memMounter{
		"disk": {files: map[int]memFile{42: {h: record.Header{Kind: record.KindIndex}, data: idxBuf.Bytes()}}},
	}}
	// A placement whose archive records an index at pos 42.
	pl := catalog.Placement{Medium: "disk", Archives: []catalog.PlacedArchive{{
		DLE: ref.DLE, Level: 0, Parts: []archiveio.FilePos{{Label: "v", Pos: 1}}, Index: archiveio.FilePos{Label: "v", Pos: 42},
	}}}
	m := &fakeMap{placements: []catalog.Placement{pl}}
	mindex := catalog.OpenMemberIndex(t.TempDir())
	c := New(m, deps, mindex)

	got, err := c.Members(ref)
	if err != nil || strings.Join(got, ",") != "dir/,dir/file" {
		t.Fatalf("Members fallback = %v, err = %v; want the decoded on-medium index", got, err)
	}
	// It re-caches: the index cache now has it.
	if cached, ok, _ := mindex.Load(ref.Run, ref.DLE, 0); !ok || strings.Join(cached, ",") != "dir/,dir/file" {
		t.Fatalf("Members did not re-cache the on-medium index: %v ok=%v", cached, ok)
	}
}

// TestMembersNoIndexIsNil confirms an archive that recorded no index (the zero Index position) yields
// a nil member list, not an error — indexPosOf reports "no index" and the fallback returns nothing.
func TestMembersNoIndexIsNil(t *testing.T) {
	ref := archiveio.Ref{Run: "run-2026-07-02.001", DLE: "h:/p", Level: 0}
	pl := catalog.Placement{Medium: "disk", Archives: []catalog.PlacedArchive{{
		DLE: ref.DLE, Level: 0, Parts: []archiveio.FilePos{{Label: "v", Pos: 1}}, // no Index
	}}}
	c := New(&fakeMap{placements: []catalog.Placement{pl}}, &fakeDeps{}, catalog.OpenMemberIndex(t.TempDir()))
	got, err := c.Members(ref)
	if err != nil || got != nil {
		t.Fatalf("Members = %v, err = %v; want nil,nil for an archive with no index", got, err)
	}
}

// fakeMedium is the Session's Medium slice for tests: a name and a volume.
type fakeMedium struct {
	name string
	vol  media.Volume
}

func (m fakeMedium) Name() string         { return m.name }
func (m fakeMedium) Volume() media.Volume { return m.vol }
