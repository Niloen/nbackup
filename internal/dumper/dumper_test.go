package dumper

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
)

// This file unit-tests the producer in isolation: a fake Archiver stands in for GNU tar
// (its Stage is a trivial `true`, so no real tar runs — engine's e2e tests already cover
// that) and a fake Ingest hands out a real archiveio.ArchiveWriter over an in-memory
// WriteStore. That lets the tests drive the full dumpItem/dumpArchive path — the tracker
// status switch, the missing-base and open-failure branches, the promote-after-commit
// invariant, and the gate/worker-permit accounting — without touching a disk or a process
// pipeline.

// --- fake archiver -------------------------------------------------------------------

type fakeArchiver struct {
	hasBase    bool
	backupErr  error // BackupSource returns this (source-open failure)
	finishErr  error // the Finish hook returns this (a fatal producer error)
	unreadable []string
	promoted   *bool // set true when Promote runs; nil = don't track
	promoteErr error
}

func (f *fakeArchiver) Name() string                                   { return "fake" }
func (f *fakeArchiver) Check() error                                   { return nil }
func (f *fakeArchiver) Estimate(archiver.BackupRequest) (int64, error) { return 0, nil }
func (f *fakeArchiver) HasBase(string, int, archiver.Scope) bool       { return f.hasBase }
func (f *fakeArchiver) RestoreStage(string, []string) programs.Cmd     { return programs.Cmd{} }
func (f *fakeArchiver) List(io.Reader) ([]record.Member, error)        { return nil, nil }
func (f *fakeArchiver) SpliceTrailer() []byte                          { return nil }
func (f *fakeArchiver) CheckSource(string) error                       { return nil }
func (f *fakeArchiver) Expand(p archiver.SourcePattern) ([]archiver.Scope, error) {
	return []archiver.Scope{{Source: p.Pattern, Exclude: p.Exclude}}, nil
}
func (f *fakeArchiver) DestIsDir() bool                            { return true }
func (f *fakeArchiver) SourceIsPath() bool                         { return true }
func (f *fakeArchiver) Ext() string                                { return ".tar" }
func (f *fakeArchiver) CanList() bool                              { return true }
func (f *fakeArchiver) StockExtract() string                       { return "cat > /dev/null" }
func (f *fakeArchiver) RestoreIsCombine() bool                     { return false }
func (f *fakeArchiver) CombineStage(string, []string) programs.Cmd { return programs.Cmd{} }
func (f *fakeArchiver) Assembler() archiver.Assembler              { return nil }
func (f *fakeArchiver) Exporter() archiver.Exporter                { return nil }

func (f *fakeArchiver) BackupSource(r archiver.BackupRequest) (*archiver.BackupSource, error) {
	if f.backupErr != nil {
		return nil, f.backupErr
	}
	finish := func() (*archiver.BackupResult, error) {
		if f.finishErr != nil {
			return nil, f.finishErr
		}
		return &archiver.BackupResult{Uncompressed: 0, FileCount: 3, Members: []record.Member{{Path: "a", Off: 0}, {Path: "b", Off: 512}, {Path: "c", Off: 1024}}, Unreadable: f.unreadable}, nil
	}
	promote := func() error {
		if f.promoteErr != nil {
			return f.promoteErr
		}
		if f.promoted != nil {
			*f.promoted = true
		}
		return nil
	}
	// `true` emits an empty stream and exits 0 — a stand-in producer. The dumper is
	// archiver-neutral, so the archive payload being empty is irrelevant here.
	return &archiver.BackupSource{
		Stage:   programs.Cmd{Name: "true"},
		Exec:    programs.Local(),
		Finish:  finish,
		Promote: promote,
		Cleanup: func() {},
	}, nil
}

// --- in-memory WriteStore + Volume ---------------------------------------------------

type memVol struct {
	hdrs map[int]record.Header
	data map[int][]byte
	next int
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

func (w *memFW) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *memFW) Pos() int                    { return w.pos }
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

func (v *memVol) ReadFile(pos int, _ media.Range) (record.Header, io.ReadCloser, error) {
	d, ok := v.data[pos]
	if !ok {
		return record.Header{}, nil, fmt.Errorf("no file at %d", pos)
	}
	return v.hdrs[pos], io.NopCloser(bytes.NewReader(d)), nil
}

func (v *memVol) Files() ([]record.FileInfo, error) { return nil, nil }
func (v *memVol) RemoveFile(int) error              { return nil }

// memStore is a single-volume, unbounded allocator+recorder. recordErr, when set, fails Record
// (an archive that streamed fine but could not be committed) — the hook for the
// promote-only-after-commit invariant.
type memStore struct {
	vol       *memVol
	recordErr error
	recorded  bool
	recCh     chan struct{} // if set, a successful Record signals here (cross-goroutine handshake)
}

func (s *memStore) Record(archiveio.CommitResult) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.recorded = true
	if s.recCh != nil {
		select {
		case s.recCh <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *memStore) NextPart() (media.Volume, int64, string, int, error) {
	return s.vol, -1, "vol", 1, nil // unbounded single volume
}
func (s *memStore) PlaceFile(string, int64) (media.Volume, string, int, error) {
	return s.vol, "vol", 1, nil
}
func (s *memStore) Bounded() bool { return false }

// --- fake Ingest ---------------------------------------------------------------------

type fakeIngest struct {
	author        *archiveio.Writer
	store         *memStore
	newArchiveErr error         // NewArchive fails with this (target/sink-open failure)
	park          chan struct{} // if non-nil, NewArchive blocks on it (a full holding disk)
	parked        chan string   // if non-nil, receives the DLE id when NewArchive parks
}

func newFakeIngest() *fakeIngest {
	st := &memStore{vol: newMemVol()}
	a := archiveio.NewWriter(st, st, archiveio.RunSpec{ID: "run-x", CreatedAt: time.Unix(0, 0).UTC()}, nil, nil)
	return &fakeIngest{author: a, store: st}
}

func (f *fakeIngest) NewArchive(spec archiveio.ArchiveSpec, est int64) (archivefs.ArchiveSink, error) {
	if f.park != nil {
		if f.parked != nil {
			f.parked <- spec.DLE
		}
		<-f.park
	}
	if f.newArchiveErr != nil {
		return nil, f.newArchiveErr
	}
	return f.author.NewArchive(spec), nil
}

func (f *fakeIngest) NewCopy(record.Archive, int64) (archivefs.ArchiveSink, error) {
	return nil, fmt.Errorf("NewCopy not used")
}

// --- helpers -------------------------------------------------------------------------

func testItem(host, path string, level, baseLevel int) planner.Item {
	dle := planner.DLE{Scope: archiver.Scope{Source: path}, Host: host}
	return planner.Item{DLE: dle, Name: dle.Name(), Level: level, BaseLevel: baseLevel}
}

func newDumper(ar archiver.Archiver, compressScheme string) *Dumper {
	return New(Config{
		ArchiverFor: func(string, string) (archiver.Archiver, error) { return ar, nil },
		Exclude:     func(string) []string { return nil },
		Placement: func(string) EncodePlacement {
			return EncodePlacement{CompressScheme: compressScheme, EncryptScheme: "none"}
		},
		Threads: 1,
	})
}

func trackerFor(items ...planner.Item) *progress.Tracker {
	plans := make([]progress.Plan, len(items))
	for i, it := range items {
		plans[i] = progress.Plan{Name: it.DLE.ID(), Level: it.Level}
	}
	return progress.NewTracker("run-x", progress.PhaseRunning, 1, plans, func() time.Time { return time.Unix(0, 0).UTC() }, nil)
}

func noopLogf(string, ...any) {}

func stateOf(tr *progress.Tracker, id string) progress.State {
	for _, d := range tr.Snapshot().DLEs {
		if d.Name == id {
			return d.State
		}
	}
	return ""
}

// TestProgressReportApprox pins the live-progress inference for dumps whose
// uncompressed count cannot be metered (a client-fused remote dump): compressed
// flow with a zero uncompressed count reports an approximate DUMPED scaled up by
// the DLE's historical compression rate, while a measured count reports as-is and
// clears the mark.
func TestProgressReportApprox(t *testing.T) {
	ar := &fakeArchiver{hasBase: true}
	d := newDumper(ar, "none")
	d.comprate = func(dle string, level int) float64 { return 0.5 } // history: halves
	item := testItem("h", "/data", 0, -1)
	tr := trackerFor(item)
	report := d.progressReport(tr, item.DLE.ID(), item)

	report(0, 100) // compressed flows, no uncompressed count: infer 100/0.5
	dle := tr.Snapshot().DLEs[0]
	if !dle.DoneApprox || dle.DoneBytes != 200 || dle.OutBytes != 100 {
		t.Fatalf("inferred report = %+v, want ~200/100", dle)
	}

	report(500, 250) // a measured count wins and drops the mark
	dle = tr.Snapshot().DLEs[0]
	if dle.DoneApprox || dle.DoneBytes != 500 {
		t.Fatalf("measured report = %+v, want 500 unmarked", dle)
	}
}

// TestProgressReportNoHistory: with no comprate history (or none wired), the
// inference falls back to 1:1 rather than dividing by zero.
func TestProgressReportNoHistory(t *testing.T) {
	ar := &fakeArchiver{hasBase: true}
	d := newDumper(ar, "none") // Comprate unwired
	item := testItem("h", "/data", 0, -1)
	tr := trackerFor(item)
	d.progressReport(tr, item.DLE.ID(), item)(0, 100)
	if dle := tr.Snapshot().DLEs[0]; !dle.DoneApprox || dle.DoneBytes != 100 {
		t.Fatalf("no-history report = %+v, want ~100 (1:1)", dle)
	}
}

// --- tests: tracker status switch ----------------------------------------------------

func TestDumpItemCommitted(t *testing.T) {
	var promoted bool
	ar := &fakeArchiver{hasBase: true, promoted: &promoted}
	d := newDumper(ar, "none")
	item := testItem("h", "/data", 0, -1)
	fs := newFakeIngest()
	tr := trackerFor(item)

	err := d.dumpItem(context.Background(), fs, item, noGate, tr, noopLogf)
	if err != nil {
		t.Fatalf("dumpItem: %v", err)
	}
	if got := stateOf(tr, item.DLE.ID()); got != progress.StateDone {
		t.Errorf("state = %q, want done", got)
	}
	if !promoted {
		t.Error("a committed dump must promote its incremental state")
	}
	if !fs.store.recorded {
		t.Error("a committed dump must record its placement")
	}
}

func TestDumpItemPartial(t *testing.T) {
	var promoted bool
	ar := &fakeArchiver{hasBase: true, promoted: &promoted, unreadable: []string{"/data/secret"}}
	d := newDumper(ar, "none")
	item := testItem("h", "/data", 0, -1)
	fs := newFakeIngest()
	tr := trackerFor(item)

	err := d.dumpItem(context.Background(), fs, item, noGate, tr, noopLogf)
	if !isPartialDump(err) {
		t.Fatalf("err = %v, want a PartialDumpError", err)
	}
	// A partial dump committed a valid archive of what was readable: it settles as DONE
	// (its bytes are real) and it DID promote — the archive stands.
	if got := stateOf(tr, item.DLE.ID()); got != progress.StateDone {
		t.Errorf("state = %q, want done (a partial commits)", got)
	}
	if !promoted {
		t.Error("a partial dump commits, so it must still promote")
	}
	if !fs.store.recorded {
		t.Error("a partial dump records its placement")
	}
}

func TestDumpItemFailed(t *testing.T) {
	var promoted bool
	ar := &fakeArchiver{hasBase: true, promoted: &promoted, backupErr: fmt.Errorf("boom")}
	d := newDumper(ar, "none")
	item := testItem("h", "/data", 0, -1)
	fs := newFakeIngest()
	tr := trackerFor(item)

	err := d.dumpItem(context.Background(), fs, item, noGate, tr, noopLogf)
	if err == nil {
		t.Fatal("dumpItem should fail when BackupSource errors")
	}
	if got := stateOf(tr, item.DLE.ID()); got != progress.StateFailed {
		t.Errorf("state = %q, want failed", got)
	}
	if promoted {
		t.Error("a failed dump must NOT promote its incremental state")
	}
}

func TestDumpItemCanceled(t *testing.T) {
	// Same failure as TestDumpItemFailed, but the run's context is canceled — the tracker
	// switch keys on ctx.Err() to report it as canceled rather than a scary failure.
	var promoted bool
	ar := &fakeArchiver{hasBase: true, promoted: &promoted, backupErr: fmt.Errorf("boom")}
	d := newDumper(ar, "none")
	item := testItem("h", "/data", 0, -1)
	fs := newFakeIngest()
	tr := trackerFor(item)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := d.dumpItem(ctx, fs, item, noGate, tr, noopLogf)
	if err == nil {
		t.Fatal("dumpItem should still return the underlying error")
	}
	if got := stateOf(tr, item.DLE.ID()); got != progress.StateCanceled {
		t.Errorf("state = %q, want canceled", got)
	}
	if promoted {
		t.Error("a canceled dump must NOT promote")
	}
}

// --- tests: missing base, open failures ---------------------------------------------

func TestBackupSpecMissingBase(t *testing.T) {
	ar := &fakeArchiver{hasBase: false} // no L0 state present
	d := newDumper(ar, "none")
	item := testItem("h", "/data", 1, 0) // incremental needs an L0 base

	_, err := d.backupSpec(item)
	if err == nil {
		t.Fatal("an incremental with no base must error")
	}
	if !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "incremental") {
		t.Errorf("error = %q, want it to name the missing incremental state", err)
	}
}

func TestDumpArchiveFilterOpenError(t *testing.T) {
	ar := &fakeArchiver{hasBase: true}
	d := newDumper(ar, "bogus-scheme") // an unknown compress scheme fails compress.Filter
	item := testItem("h", "/data", 0, -1)
	fs := newFakeIngest()
	tr := trackerFor(item)

	err := d.dumpItem(context.Background(), fs, item, noGate, tr, noopLogf)
	if err == nil {
		t.Fatal("an unknown compress scheme must fail the dump")
	}
}

func TestDumpArchiveSinkOpenError(t *testing.T) {
	ar := &fakeArchiver{hasBase: true}
	d := newDumper(ar, "none")
	item := testItem("h", "/data", 0, -1)
	fs := newFakeIngest()
	fs.newArchiveErr = fmt.Errorf("holding disk full")
	tr := trackerFor(item)

	err := d.dumpItem(context.Background(), fs, item, noGate, tr, noopLogf)
	if err == nil {
		t.Fatal("a NewArchive failure must fail the dump")
	}
	if !strings.Contains(err.Error(), "holding disk full") {
		t.Errorf("error = %q, want the sink-open cause", err)
	}
}

// --- test: promote only after a durable commit --------------------------------------

func TestPromoteOnlyAfterCommit(t *testing.T) {
	// The stream transfers fine but the store cannot Record the placement (the commit
	// fails). The incremental state must NOT be promoted — a dump whose archive never
	// durably committed leaves the base a retry builds on untouched.
	var promoted bool
	ar := &fakeArchiver{hasBase: true, promoted: &promoted}
	d := newDumper(ar, "none")
	item := testItem("h", "/data", 0, -1)
	fs := newFakeIngest()
	fs.store.recordErr = fmt.Errorf("catalog write failed")
	tr := trackerFor(item)

	err := d.dumpItem(context.Background(), fs, item, noGate, tr, noopLogf)
	if err == nil {
		t.Fatal("a failed commit must fail the dump")
	}
	if promoted {
		t.Fatal("BUG: incremental state promoted despite the archive never committing")
	}
	if got := stateOf(tr, item.DLE.ID()); got != progress.StateFailed {
		t.Errorf("state = %q, want failed", got)
	}
}

// --- test: gate / worker-permit accounting ------------------------------------------

// TestParkedDLEHoldsNoWorkerPermit proves the load-bearing concurrency invariant: a DLE
// parked waiting for its target (a full holding disk — modeled by a NewArchive that
// blocks) holds NO worker permit while parked, so the gate bounds dumps actually running,
// not waiters. With `workers` permits and `workers` DLEs all parked in NewArchive, a
// further free DLE must still acquire a permit and run to completion. If a parked DLE held
// a permit, the free DLE could never start and the test times out.
//
// NB: this uses workers=2 (not 1) because Run short-circuits to a serial, gate-less path
// when workers<=1; the gate only exists on the parallel path. Two parked DLEs against two
// permits is the tight proof — if parking consumed permits, none would be left for the
// free DLE.
func TestParkedDLEHoldsNoWorkerPermit(t *testing.T) {
	ar := &fakeArchiver{hasBase: true}
	d := newDumper(ar, "none")

	blocked1 := testItem("h", "/blocked1", 0, -1)
	blocked2 := testItem("h", "/blocked2", 0, -1)
	free := testItem("h", "/free", 0, -1)
	items := []planner.Item{blocked1, blocked2, free}
	tr := trackerFor(items...)

	park := make(chan struct{})
	parked := make(chan string, 2)
	blockedIngest := func() *fakeIngest {
		fi := newFakeIngest()
		fi.park = park
		fi.parked = parked
		return fi
	}
	bi1, bi2 := blockedIngest(), blockedIngest()
	freeIngest := newFakeIngest()
	freeIngest.store.recCh = make(chan struct{}, 1)

	route := func(it planner.Item) archivefs.Ingest {
		switch it.DLE.Source {
		case "/blocked1":
			return bi1
		case "/blocked2":
			return bi2
		default:
			return freeIngest
		}
	}

	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background(), items, 2, route, tr, noopLogf) }()

	// Wait until both blocked DLEs are actually parked in NewArchive, so the proof is that
	// the free DLE runs WHILE both permits would be held under the bug.
	<-parked
	<-parked

	// The free DLE must complete (record its placement) even though both parked DLEs are
	// waiting — its worker permit is available only if parking held none.
	select {
	case <-freeIngest.store.recCh:
	case <-time.After(3 * time.Second):
		t.Fatal("BUG: free DLE never ran — a parked DLE is holding a worker permit while waiting on its target")
	}

	// Release the parked DLEs so Run can finish.
	close(park)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after unparking")
	}
}
