package restorer

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/Niloen/nbackup/internal/archiveio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/xfer"
)

// fakeStore is an in-memory archivefs.ReadStore: the restorer runs over it with
// no media, catalog, or clerk — the point of the ReadStore seam.
type fakeStore struct {
	payloads    map[archiveio.Ref][]byte
	members     map[archiveio.Ref][]record.Member
	frames      map[archiveio.Ref][]record.Frame
	atomSeals   map[archiveio.Ref][]record.PartSeal
	ranged      bool            // the fake medium supports ranged opens
	rangedBytes int64           // total bytes served through OpenRange (the egress a test asserts)
	opened      []archiveio.Ref // refs actually opened, in order
}

func (f *fakeStore) OpenArchive(ref archiveio.Ref, medium string) (io.ReadCloser, error) {
	b, ok := f.payloads[ref]
	if !ok {
		return nil, fmt.Errorf("%w of %s %s L%d", archivefs.ErrMissingCopy, ref.Run, ref.DLE, ref.Level)
	}
	f.opened = append(f.opened, ref)
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeStore) OpenArchives(refs []archiveio.Ref, medium string, fn func(ref archiveio.Ref, open func() (io.ReadCloser, error)) error) (missing []archiveio.Ref, err error) {
	for _, ref := range refs {
		if _, ok := f.payloads[ref]; !ok {
			missing = append(missing, ref)
		}
	}
	for _, ref := range refs {
		ref := ref
		if _, ok := f.payloads[ref]; !ok {
			continue
		}
		if e := fn(ref, func() (io.ReadCloser, error) { return f.OpenArchive(ref, medium) }); e != nil {
			return missing, e
		}
	}
	return missing, nil
}

func (f *fakeStore) Members(ref archiveio.Ref) ([]record.Member, error) { return f.members[ref], nil }

// Index serves the fake's members (with any frames a test injected); OpenRange counts
// the ranged bytes actually fetched, so a test can assert selective restore's egress.
func (f *fakeStore) Index(ref archiveio.Ref) (record.Index, error) {
	return record.Index{Members: f.members[ref], Frames: f.frames[ref]}, nil
}

func (f *fakeStore) OpenRange(ref archiveio.Ref, medium string, rng media.Range) (io.ReadCloser, error) {
	if !f.ranged {
		return nil, media.ErrRangeUnsupported
	}
	b, ok := f.payloads[ref]
	if !ok {
		return nil, fmt.Errorf("%w of %s %s L%d", archivefs.ErrMissingCopy, ref.Run, ref.DLE, ref.Level)
	}
	bounded, err := rng.Bound(int64(len(b)))
	if err != nil {
		return nil, err
	}
	f.rangedBytes += bounded.Len
	return io.NopCloser(bytes.NewReader(b[bounded.Off : bounded.Off+bounded.Len])), nil
}

func (f *fakeStore) AtomSeals(ref archiveio.Ref) ([]record.PartSeal, error) {
	return f.atomSeals[ref], nil
}

// fakeArchiver implements only the read side (RestoreStage); the restorer never
// touches the produce side.
type fakeArchiver struct{ archiver.Archiver }

func (fakeArchiver) RestoreStage(destDir string, members []string) programs.Cmd {
	return programs.Cmd{Name: "sh", Args: []string{"-c", "cat >/dev/null"}}
}

// The fake consumes anything, so it may honestly declare its streams spliceable.
func (fakeArchiver) SpliceTrailer() []byte { return []byte{} }

// A tree-style fake: the tests exercise the directory guard/rollback paths.
func (fakeArchiver) DestIsDir() bool { return true }
func (fakeArchiver) CanList() bool   { return true }

func testDeps(store *fakeStore, archives []record.Archive) Deps {
	return Deps{
		Store:    store,
		Archives: func() []record.Archive { return archives },
		Exec:     func(host string) programs.Executor { return programs.Local() },
		ArchiverFor: func(typeName, dle, host string) (archiver.Archiver, error) {
			return fakeArchiver{}, nil
		},
		EncryptionFor: func(dle string) (config.EncryptConfig, bool) { return config.EncryptConfig{}, false },
		KnownHosts:    func() []string { return []string{"app01"} },
		DisplayDLE:    func(slug string) string { return slug },
	}
}

func chainArchives(dle string) []record.Archive {
	return []record.Archive{
		{Run: "run-2026-06-01.001", DLE: dle, Level: 0, Archiver: "gnutar", Compress: "none"},
		{Run: "run-2026-06-02.001", DLE: dle, Level: 1, Archiver: "gnutar", Compress: "none", BaseRun: "run-2026-06-01.001"},
	}
}

func ref(run, dle string, level int) archiveio.Ref {
	return archiveio.Ref{Run: run, DLE: dle, Level: level}
}

// TestExtractChainHappyPath replays a full+incremental chain through the fake
// store and archiver: both archives are opened, in level order, and the restore
// succeeds — no media, no engine.
func TestExtractChainHappyPath(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{
		ref("run-2026-06-01.001", dle, 0): []byte("l0"),
		ref("run-2026-06-02.001", dle, 1): []byte("l1"),
	}}
	r := New(testDeps(store, chainArchives(dle)))
	dest := filepath.Join(t.TempDir(), "out")
	if err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-02.001", Dest: dest}, nil); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := []archiveio.Ref{ref("run-2026-06-01.001", dle, 0), ref("run-2026-06-02.001", dle, 1)}
	if len(store.opened) != 2 || store.opened[0] != want[0] || store.opened[1] != want[1] {
		t.Fatalf("opened %v, want %v (full first, then the incremental)", store.opened, want)
	}
}

// TestExtractBrokenChainIsMissingCopy: a chain whose base has no copy fails
// before anything is applied — the error carries archivefs.ErrMissingCopy for
// the drill's classification, and no archive is opened (a later incremental
// must never be extracted over a missing base).
func TestExtractBrokenChainIsMissingCopy(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{
		// Only the L1 is present; the L0 base has no copy.
		ref("run-2026-06-02.001", dle, 1): []byte("l1"),
	}}
	r := New(testDeps(store, chainArchives(dle)))
	err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-02.001", Dest: filepath.Join(t.TempDir(), "out")}, nil)
	if err == nil {
		t.Fatal("want error for a chain with a missing base")
	}
	if !errors.Is(err, archivefs.ErrMissingCopy) {
		t.Fatalf("error should wrap archivefs.ErrMissingCopy for classification; got: %v", err)
	}
	if len(store.opened) != 0 {
		t.Fatalf("no archive may be extracted over a missing base; opened %v", store.opened)
	}
}

// TestExtractRefusesNonEmptyDest guards the destructive listed-incremental
// prune: a non-empty local destination is refused without Force.
func TestExtractRefusesNonEmptyDest(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{
		ref("run-2026-06-01.001", dle, 0): []byte("l0"),
	}}
	r := New(testDeps(store, chainArchives(dle)[:1]))
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-01.001", Dest: dest}, nil)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("want a non-empty-dest refusal mentioning --force, got: %v", err)
	}
	if err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-01.001", Dest: dest, Force: true}, nil); err != nil {
		t.Fatalf("Force should override the guard: %v", err)
	}
}

// TestExtractUnknownToHost fails a --to restore up front for a host the config
// does not know, naming the known ones.
func TestExtractUnknownToHost(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{}}
	r := New(testDeps(store, chainArchives(dle)))
	err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-02.001", Dest: "/restore", Host: "typo01"}, nil)
	if err == nil || !strings.Contains(err.Error(), "not a configured host") {
		t.Fatalf("want unknown-host refusal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "app01") {
		t.Fatalf("refusal should name the known hosts, got: %v", err)
	}
}

// TestErrorContractSurvivesWrapping pins the restorer's error contract: the
// classification signals — the missing-copy sentinel (errors.Is) and the
// role-tagged xfer error (errors.As) — must survive the step wrapping and the
// decrypt hint, because the drill classifies on nothing else.
func TestErrorContractSurvivesWrapping(t *testing.T) {
	step := chainStep()
	sinkFault := &xfer.Error{Role: xfer.RoleSink, Err: errors.New("tar: directory rename conflict")}
	wrapped := stepErr(step, DecryptHint("gpg", sinkFault))
	var xe *xfer.Error
	if !errors.As(wrapped, &xe) || xe.Role != xfer.RoleSink {
		t.Fatalf("Sink role lost through wrapping: %v", wrapped)
	}

	missing := fmt.Errorf("%w of run x", archivefs.ErrMissingCopy)
	wrapped = stepErr(step, DecryptHint("gpg", missing))
	if !errors.Is(wrapped, archivefs.ErrMissingCopy) {
		t.Fatalf("missing-copy sentinel lost through wrapping: %v", wrapped)
	}
}

func chainStep() recovery.Step {
	return recovery.Step{RunID: "run-2026-06-01.001", DLE: "app01-data", Level: 0}
}
