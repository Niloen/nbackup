package spool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

func shaOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// memVolume is a minimal, mutex-guarded in-memory media.Volume for the spool fault tests. AppendFile
// itself is opened from inside the orchestrator's serialized control loop (NextPart->PrepareWrite->
// ensureStarted->AppendFile), so it is correctly single-flight; a producer's actual byte I/O happens
// in Write calls on the already-open writer, on the producer's own goroutine, concurrently with other
// producers. inFile/maxInFile track overlap of those Write calls; barrier, when set, rendezvous-
// releases the first two Write calls together so a test can PROVE overlap deterministically instead of
// hoping a sleep wins a race — under `-race` the detector schedules goroutines far more serially, so a
// sleep-and-hope overlap check that passes under a plain `go test` reliably fails under `-race`. nil by
// default: only a test that opts in via pairGate pays for the rendezvous.
type memVolume struct {
	mu   sync.Mutex
	hdrs map[int]record.Header
	data map[int][]byte
	next int

	inFile    int32
	maxInFile int32
	barrier   *pairGate
}

func newMemVolume() *memVolume {
	return &memVolume{hdrs: map[int]record.Header{}, data: map[int][]byte{}}
}

// pairGate rendezvous-releases exactly its first two arrivals together; later arrivals pass straight
// through. Used to force two producers' Write calls to genuinely overlap in time.
type pairGate struct {
	arrived int32
	gate    chan struct{}
}

func newPairGate() *pairGate { return &pairGate{gate: make(chan struct{})} }

func (g *pairGate) arrive() {
	switch atomic.AddInt32(&g.arrived, 1) {
	case 1:
		<-g.gate // first arrival waits for a second Write call to release it
	case 2:
		close(g.gate) // second arrival releases the first; both proceed concurrently
	}
}

func (v *memVolume) AppendFile(ctx context.Context, h record.Header) (media.FileWriter, error) {
	return &memFileWriter{v: v, ctx: ctx, h: h}, nil
}

type memFileWriter struct {
	v   *memVolume
	ctx context.Context
	h   record.Header
	buf bytes.Buffer
	pos int
}

func (w *memFileWriter) Pos() int { return w.pos }
func (w *memFileWriter) Write(p []byte) (int, error) {
	if w.v.barrier != nil {
		w.v.barrier.arrive()
	}
	n := atomic.AddInt32(&w.v.inFile, 1)
	for {
		old := atomic.LoadInt32(&w.v.maxInFile)
		if n <= old || atomic.CompareAndSwapInt32(&w.v.maxInFile, old, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	atomic.AddInt32(&w.v.inFile, -1)
	return w.buf.Write(p)
}
func (w *memFileWriter) Close() error {
	if w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	w.v.mu.Lock()
	defer w.v.mu.Unlock()
	pos := w.v.next
	w.v.next++
	w.v.hdrs[pos] = w.h
	w.v.data[pos] = append([]byte(nil), w.buf.Bytes()...)
	w.pos = pos
	return nil
}

func (v *memVolume) ReadFile(pos int, _ media.Range) (record.Header, io.ReadCloser, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	d, ok := v.data[pos]
	if !ok {
		return record.Header{}, nil, errors.New("no file")
	}
	return v.hdrs[pos], io.NopCloser(bytes.NewReader(d)), nil
}

func (v *memVolume) Files() ([]record.FileInfo, error) { return nil, nil }

func (v *memVolume) RemoveFile(pos int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.hdrs, pos)
	delete(v.data, pos)
	return nil
}

// memStore is an in-memory archivefs.WriteStore plus PartAllocator (a landing's seams or a holding
// disk's full store), backed by one memVolume. Its control calls (NextPart/PlaceFile/Record) are the ones the spool
// routes onto the orchestrator; each optionally faults and each runs a concurrency probe so a test
// can assert they never overlap. OpenArchiveAt/ReclaimAt are the drain's read-back and drop, also
// faultable.
type memStore struct {
	name string
	vol  *memVolume

	nextPartErr error
	recordErr   error
	openErr     error
	reclaimErr  error
	partCap     int64 // per-part byte cap NextPart hands out; 0 = unbounded (-1)

	mu        sync.Mutex
	records   []archiveio.CommitResult
	reclaimed int

	inCtl    int32
	maxInCtl int32
}

func newMemStore(name string) *memStore { return &memStore{name: name, vol: newMemVolume()} }

// enterCtl marks a control call in flight and records the peak concurrency, so a test can prove the
// orchestrator serializes them (peak stays 1) even under concurrent producers.
func (s *memStore) enterCtl() func() {
	n := atomic.AddInt32(&s.inCtl, 1)
	for {
		old := atomic.LoadInt32(&s.maxInCtl)
		if n <= old || atomic.CompareAndSwapInt32(&s.maxInCtl, old, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	return func() { atomic.AddInt32(&s.inCtl, -1) }
}

func (s *memStore) NextPart() (media.Volume, int64, string, int, error) {
	defer s.enterCtl()()
	if s.nextPartErr != nil {
		return nil, 0, "", 0, s.nextPartErr
	}
	cap := s.partCap
	if cap == 0 {
		cap = -1
	}
	return s.vol, cap, s.name, 1, nil
}

func (s *memStore) PlaceFile(int64) (media.Volume, string, int, error) {
	defer s.enterCtl()()
	return s.vol, s.name, 1, nil
}

func (s *memStore) Bounded() bool { return false }

func (s *memStore) Record(r archiveio.CommitResult) error {
	defer s.enterCtl()()
	if s.recordErr != nil {
		return s.recordErr
	}
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
	return nil
}

func (s *memStore) OpenArchiveAt(ref archiveio.Ref, pos archiveio.ArchivePos) (io.ReadCloser, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	open := func(p archiveio.FilePos, rng media.Range) (record.Header, io.ReadCloser, error) {
		return s.vol.ReadFile(p.Pos, rng)
	}
	return archiveio.NewReader(open, nil).Open(ref, archiveio.BareParts(pos.Parts), media.Range{})
}

func (s *memStore) ReclaimAt(archiveio.Ref, archiveio.ArchivePos) error {
	if s.reclaimErr != nil {
		return s.reclaimErr
	}
	s.mu.Lock()
	s.reclaimed++
	s.mu.Unlock()
	return nil
}

func (s *memStore) recordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// transferArchive drives one archive's payload through sink exactly as the dumper's transfer does
// (a raw byte source, no filters), so the spool's real writer path runs — meter, part, footer,
// routed Record — and Close fires the spool's per-write release hook.
func transferArchive(sink archivefs.ArchiveSink, body []byte) error {
	_, err := xfer.Transfer(context.Background(), xfer.Reader(io.NopCloser(bytes.NewReader(body))), xfer.NewFilters(), sink)
	cerr := sink.Close()
	if err != nil {
		return err
	}
	return cerr
}

var archSpec = archiveio.ArchiveSpec{DLE: "localhost:/data", Host: "localhost", Path: "/data", Archiver: "m", Compress: "none", Level: 0}

// stageOne routes one dump through the spool bound for lane "landing" and waits for its drain to
// finish (Drain joins the goroutine), returning the run's error.
func stageOne(t *testing.T, sp *Spool, body []byte) error {
	t.Helper()
	aw, err := sp.Ingest("landing").NewArchive(archSpec, 1<<20)
	if err != nil {
		return err
	}
	if err := transferArchive(aw, body); err != nil {
		return err
	}
	return sp.Drain()
}

// TestDrainReadBackFaultAborts faults the holding-disk read-back (OpenArchiveAt) mid-drain: the copy
// to the landing can't even start, so the drain must abort the run and Drain must surface it.
func TestDrainReadBackFaultAborts(t *testing.T) {
	boom := errors.New("read holding boom")
	holding := newMemStore("holding")
	holding.openErr = boom
	landing := newMemStore("landing")
	sp := New(context.Background(), Config{
		Backings: []Backing{{Name: "landing", Allocs: []archiveio.PartAllocator{landing}, Rec: landing, Writers: 1}},
		Holding:  NewPool([]Disk{{Name: "hd0", Alloc: holding, Storage: holding, Capacity: 0}}),
	})
	err := stageOne(t, sp, []byte("payload"))
	if !errors.Is(err, boom) {
		t.Fatalf("Drain = %v; want the read-back fault %v", err, boom)
	}
	if landing.recordCount() != 0 {
		t.Fatalf("landing recorded %d archives; a failed read-back must land nothing", landing.recordCount())
	}
}

// TestDrainLandingRecordFaultAborts faults the landing's Record during the drain's copy: the copy's
// Commit fails, so the drain aborts and the holding copy is NOT reclaimed (nothing landed to make it
// safe to drop).
func TestDrainLandingRecordFaultAborts(t *testing.T) {
	boom := errors.New("landing record boom")
	holding := newMemStore("holding")
	landing := newMemStore("landing")
	landing.recordErr = boom
	sp := New(context.Background(), Config{
		Backings: []Backing{{Name: "landing", Allocs: []archiveio.PartAllocator{landing}, Rec: landing, Writers: 1}},
		Holding:  NewPool([]Disk{{Name: "hd0", Alloc: holding, Storage: holding, Capacity: 0}}),
	})
	err := stageOne(t, sp, []byte("payload"))
	if !errors.Is(err, boom) {
		t.Fatalf("Drain = %v; want the landing Record fault %v", err, boom)
	}
	holding.mu.Lock()
	reclaimed := holding.reclaimed
	holding.mu.Unlock()
	if reclaimed != 0 {
		t.Fatalf("holding reclaimed %d times; a failed landing copy must never reclaim the staged copy", reclaimed)
	}
}

// TestDrainReclaimFaultAborts lets the copy to the landing succeed, then faults the holding-disk
// reclaim: the archive is safely on the landing but the drain still reports the reclaim failure so
// the run fails loud rather than silently leaving a stale staged copy.
func TestDrainReclaimFaultAborts(t *testing.T) {
	boom := errors.New("reclaim boom")
	holding := newMemStore("holding")
	holding.reclaimErr = boom
	landing := newMemStore("landing")
	sp := New(context.Background(), Config{
		Backings: []Backing{{Name: "landing", Allocs: []archiveio.PartAllocator{landing}, Rec: landing, Writers: 1}},
		Holding:  NewPool([]Disk{{Name: "hd0", Alloc: holding, Storage: holding, Capacity: 0}}),
	})
	err := stageOne(t, sp, []byte("payload"))
	if !errors.Is(err, boom) {
		t.Fatalf("Drain = %v; want the reclaim fault %v", err, boom)
	}
	if landing.recordCount() != 1 {
		t.Fatalf("landing recorded %d archives; the copy must have landed before the reclaim faulted", landing.recordCount())
	}
}

// TestDrainSuccessReclaimsAndLands is the happy path the fault tests bracket: one staged archive
// drains to the landing (recorded there) and its holding copy is reclaimed exactly once.
func TestDrainSuccessReclaimsAndLands(t *testing.T) {
	holding := newMemStore("holding")
	landing := newMemStore("landing")
	sp := New(context.Background(), Config{
		Backings: []Backing{{Name: "landing", Allocs: []archiveio.PartAllocator{landing}, Rec: landing, Writers: 1}},
		Holding:  NewPool([]Disk{{Name: "hd0", Alloc: holding, Storage: holding, Capacity: 0}}),
	})
	if err := stageOne(t, sp, []byte("the quick brown fox")); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if landing.recordCount() != 1 {
		t.Fatalf("landing recorded %d archives; want 1", landing.recordCount())
	}
	holding.mu.Lock()
	reclaimed := holding.reclaimed
	holding.mu.Unlock()
	if reclaimed != 1 {
		t.Fatalf("holding reclaimed %d times; want exactly 1", reclaimed)
	}
}

// TestOrchestratorSerializesControlCalls proves the load-bearing concurrency invariant: two producers
// writing direct to one shared landing store run their byte I/O concurrently (the volume sees >1
// append in flight) yet their control calls — NextPart/PlaceRecord/Record, the catalog/librarian
// touching ones — never overlap, because they all funnel onto the single orchestrator goroutine.
func TestOrchestratorSerializesControlCalls(t *testing.T) {
	landing := newMemStore("landing")
	landing.vol.barrier = newPairGate() // force the first two producers' Write calls to overlap
	sp := New(context.Background(), Config{
		// Writers: 2 over a single shared store => two producers write it at once; only the
		// orchestrator serialises their control calls.
		Backings: []Backing{{Name: "landing", Allocs: []archiveio.PartAllocator{landing}, Rec: landing, Writers: 2}},
		Holding:  NewPool(nil), // no holding disk => every write goes direct to the landing
	})
	body := bytes.Repeat([]byte("abcdefgh"), 4096)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			aw, err := sp.Ingest("landing").NewArchive(archSpec, 1<<20)
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = transferArchive(aw, body)
		}(i)
	}
	wg.Wait()
	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("producer %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&landing.maxInCtl); got != 1 {
		t.Fatalf("peak concurrent control calls = %d; the orchestrator must serialize them to 1", got)
	}
	if got := atomic.LoadInt32(&landing.vol.maxInFile); got < 2 {
		t.Fatalf("peak concurrent byte appends = %d; the two producers' byte I/O must overlap (else the test proves nothing)", got)
	}
	if landing.recordCount() != 4 {
		t.Fatalf("landing recorded %d archives; want 4", landing.recordCount())
	}
}

// TestCancelAbortsViaWatcher covers the setAbort path driven by the ctx-cancel watcher New starts:
// canceling the run's context aborts the holding pool (waking any blocked producer) and Aborted
// reports the cancel.
func TestCancelAbortsViaWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	holding := newMemStore("holding")
	pool := NewPool([]Disk{{Name: "hd0", Alloc: holding, Storage: holding, Capacity: 1 << 30}})
	landing := newMemStore("landing")
	sp := New(ctx, Config{
		Backings: []Backing{{Name: "landing", Allocs: []archiveio.PartAllocator{landing}, Rec: landing, Writers: 1}},
		Holding:  pool,
	})
	cancel()
	// The watcher runs setAbort asynchronously; wait briefly for it to propagate.
	deadline := time.Now().Add(2 * time.Second)
	for sp.Aborted() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(sp.Aborted(), context.Canceled) {
		t.Fatalf("Aborted = %v; want context.Canceled after cancel", sp.Aborted())
	}
	if !errors.Is(pool.Err(), context.Canceled) {
		t.Fatalf("pool.Err = %v; setAbort must abort the pool so blocked producers wake", pool.Err())
	}
	_ = sp.Drain()
}

// TestNewCopyRoutesAndCommits exercises the NewCopy ingest path (nb copy / drain re-author): a
// pre-sealed archive re-authored onto the landing preserves its identity and records a placement.
func TestNewCopyRoutesAndCommits(t *testing.T) {
	landing := newMemStore("landing")
	sp := New(context.Background(), Config{
		Backings: []Backing{{Name: "landing", Allocs: []archiveio.PartAllocator{landing}, Rec: landing, Writers: 1}},
		Holding:  NewPool(nil),
	})
	body := []byte("copy me verbatim")
	// A NewCopy verifies the metered checksum against the source's, so give it the sha of body.
	src := shaOf(body)
	arch := record.Archive{Run: "run-2026-07-02.001", DLE: "localhost:/data", Host: "localhost", Path: "/data",
		Compress: "none", Level: 0, SHA256: src, Uncompressed: int64(len(body))}
	aw, err := sp.Ingest("landing").NewCopy(arch, int64(len(body)))
	if err != nil {
		t.Fatalf("NewCopy: %v", err)
	}
	if err := transferArchive(aw, body); err != nil {
		t.Fatalf("transfer copy: %v", err)
	}
	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if landing.recordCount() != 1 {
		t.Fatalf("landing recorded %d copies; want 1", landing.recordCount())
	}
	if got := landing.records[0].Archive.SHA256; got != src {
		t.Fatalf("copy recorded sha %q; want the source's %q", got, src)
	}
}

// TestNewArchiveAfterAbortReturnsRunError proves a producer that arrives after the run has aborted
// gets the run's error from Ingest rather than starting a doomed write.
func TestNewArchiveAfterAbortReturnsRunError(t *testing.T) {
	boom := errors.New("already down")
	landing := newMemStore("landing")
	sp := New(context.Background(), Config{
		Backings: []Backing{{Name: "landing", Allocs: []archiveio.PartAllocator{landing}, Rec: landing, Writers: 1}},
		Holding:  NewPool(nil),
	})
	sp.setAbort(boom)
	if _, err := sp.Ingest("landing").NewArchive(archSpec, 1<<20); !errors.Is(err, boom) {
		t.Fatalf("NewArchive after abort = %v; want %v", err, boom)
	}
	_ = sp.Drain()
}

// TestLandingVolume covers the label-joining helper: distinct part labels are comma-joined in order,
// duplicates collapse, and an address-identified archive (no labels) yields "".
func TestLandingVolume(t *testing.T) {
	cases := []struct {
		name  string
		parts []archiveio.FilePos
		want  string
	}{
		{"no labels (disk/cloud)", []archiveio.FilePos{{Pos: 1}, {Pos: 2}}, ""},
		{"single volume", []archiveio.FilePos{{Label: "tape-1", Pos: 1}}, "tape-1"},
		{"spanned, deduped in order", []archiveio.FilePos{{Label: "tape-1"}, {Label: "tape-2"}, {Label: "tape-1"}}, "tape-1,tape-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := landingVolume(archiveio.ArchivePos{Parts: tc.parts}); got != tc.want {
				t.Errorf("landingVolume = %q; want %q", got, tc.want)
			}
		})
	}
}
