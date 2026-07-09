package restorer

import (
	"errors"
	"fmt"
	"github.com/Niloen/nbackup/internal/archiveio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
)

// scriptArchiver drives a caller-supplied RestoreStage so a test can make one
// archive in a chain write a marker and a later one fail.
type scriptArchiver struct {
	archiver.Archiver
	stage func(dest string, members []string) programs.Cmd
}

func (s scriptArchiver) RestoreStage(dest string, members []string) programs.Cmd {
	return s.stage(dest, members)
}

// Tree-style, so the rollback tests exercise the guard/rollback paths.
func (scriptArchiver) DestIsDir() bool    { return true }
func (scriptArchiver) SourceIsPath() bool { return true }

// The defaults (the embedded interface is nil; see fakeArchiver).
func (scriptArchiver) RestoreIsCombine() bool                     { return false }
func (scriptArchiver) CombineStage(string, []string) programs.Cmd { return programs.Cmd{} }
func (scriptArchiver) Assembler() archiver.Assembler              { return nil }
func (scriptArchiver) Exporter() archiver.Exporter                { return nil }

// scriptDeps returns Deps whose ArchiverFor hands out the given per-archive
// RestoreStages in order (one per archive extracted).
func scriptDeps(store *fakeStore, archives []record.Archive, stages ...func(dest string, members []string) programs.Cmd) Deps {
	d := testDeps(store, archives)
	i := 0
	probes := 0
	d.ArchiverFor = func(typeName, _, dle, host string) (archiver.Archiver, error) {
		// The chain's first resolutions are the capability probes (destIsDir,
		// then combineFor), before any extraction; they must not consume a
		// scripted stage.
		if probes < 2 {
			probes++
			return scriptArchiver{stage: drainStage}, nil
		}
		st := stages[i]
		i++
		return scriptArchiver{stage: st}, nil
	}
	return d
}

func drainStage(_ string, _ []string) programs.Cmd {
	return programs.Cmd{Name: "sh", Args: []string{"-c", "cat >/dev/null"}}
}

func markerStage(dest string, _ []string) programs.Cmd {
	return programs.Cmd{Name: "sh", Args: []string{"-c", fmt.Sprintf("cat >/dev/null; touch %s/landed.txt", dest)}}
}

func failStage(_ string, _ []string) programs.Cmd {
	return programs.Cmd{Name: "sh", Args: []string{"-c", "cat >/dev/null; exit 1"}}
}

// TestSecondArchiveFailsRollsBackEmptyDest is the load-bearing whole-DLE
// guarantee: in a 2-archive chain the first archive lands (writing files into a
// fresh empty dest), then the SECOND fails to decode. Because the dest was empty
// at the start (no --force, local), everything in it is ours, so a failed chain
// must clear it — never leave a half-restored tree a user could mistake for a
// complete restore.
func TestSecondArchiveFailsRollsBackEmptyDest(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{
		ref("run-2026-06-01.001", dle, 0): []byte("l0"),
		ref("run-2026-06-02.001", dle, 1): []byte("l1"),
	}}
	r := New(scriptDeps(store, chainArchives(dle), markerStage, failStage))
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-02.001", Dest: dest}, nil)
	if err == nil {
		t.Fatal("want an error when the chain's second archive fails")
	}
	if !strings.Contains(err.Error(), "was cleared") {
		t.Fatalf("a rolled-back empty dest should report it was cleared, got: %v", err)
	}
	entries, derr := os.ReadDir(dest)
	if derr != nil {
		t.Fatal(derr)
	}
	if len(entries) != 0 {
		t.Fatalf("dest must be cleared after a failed chain, still holds: %v", entries)
	}
}

// TestSecondArchiveFailsForceWarnsLoud: with --force the dest held the operator's
// own content, so a failed chain must NOT auto-delete it — it warns loudly that
// the tree is partial and leaves it in place for the operator to discard.
func TestSecondArchiveFailsForceWarnsLoud(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{
		ref("run-2026-06-01.001", dle, 0): []byte("l0"),
		ref("run-2026-06-02.001", dle, 1): []byte("l1"),
	}}
	r := New(scriptDeps(store, chainArchives(dle), markerStage, failStage))
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "operator-file.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-02.001", Dest: dest, Force: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "PARTIAL") {
		t.Fatalf("a --force failed chain must warn loudly about a partial tree, got: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dest, "operator-file.txt")); serr != nil {
		t.Fatalf("--force must never delete the operator's own content: %v", serr)
	}
}

// TestExtractDestSetupFailureNoPartialWarning: when the destination cannot even
// be created (here Dest is an existing regular file), nothing landed, so the
// error is reported plainly via errDestSetup — never the misleading "partial
// restore" warning nor a rollback of a tree that was never written.
func TestExtractDestSetupFailureNoPartialWarning(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{
		ref("run-2026-06-01.001", dle, 0): []byte("l0"),
	}}
	r := New(scriptDeps(store, chainArchives(dle)[:1], drainStage))
	// A regular file where a directory is expected: MkdirAll fails.
	destFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(destFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-01.001", Dest: destFile, Force: true}, nil)
	if err == nil {
		t.Fatal("want an error when the dest cannot be created")
	}
	if !errors.Is(err, errDestSetup) {
		t.Fatalf("a dest-setup failure must carry errDestSetup, got: %v", err)
	}
	if strings.Contains(err.Error(), "PARTIAL") {
		t.Fatalf("a never-started restore must not warn about a partial tree: %v", err)
	}
}

// encChain builds a full+incremental chain, both encrypted with the given scheme.
func encChain(dle, scheme string) []record.Archive {
	a := chainArchives(dle)
	for i := range a {
		a[i].Encrypt = scheme
	}
	return a
}

// TestEnsureServerCanDecodeWarnsOnce: a server-side restore of a client-side
// public-key DLE cannot be proven infeasible (the private key may be escrowed on
// the server), so it warns — exactly once per DLE across the chain — then
// proceeds. Here the copies are absent, so the restore then fails missing-copy;
// the point is the single warning fired first.
func TestEnsureServerCanDecodeWarnsOnce(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{}} // no copies: fail after the warn
	d := testDeps(store, encChain(dle, "gpg"))
	d.EncryptionFor = func(string) (config.EncryptConfig, bool) {
		return config.EncryptConfig{Scheme: "gpg", At: "client", Recipient: "ops@x"}, true
	}
	r := New(d)
	var logs []string
	log := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-02.001", Dest: filepath.Join(t.TempDir(), "out")}, log)
	if !errors.Is(err, archivefs.ErrMissingCopy) {
		t.Fatalf("expected a missing-copy failure after the warn, got: %v", err)
	}
	warns := 0
	for _, l := range logs {
		if strings.Contains(l, "encrypted on the client") {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("client-key warning must fire exactly once per DLE, fired %d times: %v", warns, logs)
	}
}

// TestEnsureServerCanDecodeHardError: a server-side restore of a client-side
// SYMMETRIC (passphrase, no recipient) DLE is provably infeasible — the
// passphrase never leaves the client — so it fails fast before touching media.
func TestEnsureServerCanDecodeHardError(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{
		ref("run-2026-06-01.001", dle, 0): []byte("l0"),
		ref("run-2026-06-02.001", dle, 1): []byte("l1"),
	}}
	d := testDeps(store, encChain(dle, "gpg"))
	d.EncryptionFor = func(string) (config.EncryptConfig, bool) {
		return config.EncryptConfig{Scheme: "gpg", At: "client", PassphraseFile: "/keys/pass"}, true
	}
	r := New(d)
	err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-02.001", Dest: filepath.Join(t.TempDir(), "out")}, nil)
	if err == nil || !strings.Contains(err.Error(), "passphrase never leaves the client") {
		t.Fatalf("want a hard infeasibility error for a client symmetric key, got: %v", err)
	}
	if len(store.opened) != 0 {
		t.Fatalf("an infeasible restore must touch no media, opened: %v", store.opened)
	}
}
