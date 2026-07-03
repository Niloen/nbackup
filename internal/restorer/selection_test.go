package restorer

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
)

// TestOpenRecoverBuildsTree browses a full+incremental chain as of a date: the
// merged member lists (loaded lazily via the store) form an as-of filesystem
// where each path resolves to the archive that last held it — no media touched.
func TestOpenRecoverBuildsTree(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{members: map[record.Ref][]string{
		ref("run-2026-06-01.001", dle, 0): {"./etc/", "./etc/hosts"},
		ref("run-2026-06-02.001", dle, 1): {"./var/", "./var/log"},
	}}
	r := New(testDeps(store, chainArchives(dle)))
	tree, err := r.OpenRecover(dle, "2026-06-02")
	if err != nil {
		t.Fatalf("OpenRecover: %v", err)
	}
	if _, ok := tree.Lookup("etc/hosts"); !ok {
		t.Fatal("browse tree should hold etc/hosts from the L0")
	}
	if _, ok := tree.Lookup("var/log"); !ok {
		t.Fatal("browse tree should hold var/log from the L1 (most-recent-wins merge)")
	}
}

// TestDecryptOptsForPerDumptype: a per-dumptype passphrase_file is honored on
// read-back (mirroring the dump side); a DLE not in the config falls back to the
// config-wide decrypt reference.
func TestDecryptOptsForPerDumptype(t *testing.T) {
	d := testDeps(&fakeStore{}, nil)
	d.DecryptOpts.PassphraseFile = "/config-wide/pass"
	d.EncryptionFor = func(dle string) (config.EncryptConfig, bool) {
		if dle == "db01-pg" {
			return config.EncryptConfig{PassphraseFile: "/dumptype/pass"}, true
		}
		return config.EncryptConfig{}, false
	}
	r := New(d)
	if got := r.DecryptOptsFor("db01-pg").PassphraseFile; got != "/dumptype/pass" {
		t.Fatalf("per-dumptype passphrase_file not honored: got %q", got)
	}
	if got := r.DecryptOptsFor("other-dle").PassphraseFile; got != "/config-wide/pass" {
		t.Fatalf("a DLE not in config should fall back to the config-wide ref: got %q", got)
	}
}

// TestFriendlyDLEErrRewritesSlug: a chain-planning error names the DLE as the
// host:path form the user passed, not the internal catalog slug.
func TestFriendlyDLEErrRewritesSlug(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[record.Ref][]byte{}}
	// The target run exists (another DLE is in it) but holds no backup for our DLE,
	// so Chain fails with a message that quotes the DLE slug.
	other := []record.Archive{{Run: "run-2026-06-02.001", DLE: "other-dle", Level: 0}}
	d := testDeps(store, other)
	d.DisplayDLE = func(slug string) string {
		if slug == dle {
			return "app01:/data"
		}
		return slug
	}
	r := New(d)
	err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-02.001", Dest: filepath.Join(t.TempDir(), "out")}, nil)
	if err == nil {
		t.Fatal("want a chain-planning error for an unknown DLE")
	}
	if strings.Contains(err.Error(), `"`+dle+`"`) {
		t.Fatalf("error should not expose the internal slug: %v", err)
	}
	if !strings.Contains(err.Error(), "app01:/data") {
		t.Fatalf("error should name the host:path display form: %v", err)
	}
}

func extractStep(run, dle string, level int, members ...string) recovery.ExtractStep {
	return recovery.ExtractStep{
		Step:    recovery.Step{RunID: run, DLE: dle, Level: level, Archiver: "gnutar", Compress: "none"},
		Members: members,
	}
}

// TestExtractSelectionSkipsEmptyStep: an archive in the selection that holds none
// of the chosen files (only directory entries, countFiles == 0) contributes
// nothing and is skipped silently — its stream is never opened — while the
// archive that does hold files is extracted and counted.
func TestExtractSelectionSkipsEmptyStep(t *testing.T) {
	dle := "app01-data"
	emptyRef := ref("run-2026-06-01.001", dle, 0)
	fileRef := ref("run-2026-06-02.001", dle, 1)
	store := &fakeStore{payloads: map[record.Ref][]byte{
		emptyRef: []byte("l0"),
		fileRef:  []byte("l1"),
	}}
	r := New(testDeps(store, chainArchives(dle)))
	steps := []recovery.ExtractStep{
		extractStep("run-2026-06-01.001", dle, 0, "./etc/"),            // dirs only → 0 files
		extractStep("run-2026-06-02.001", dle, 1, "./var/", "./var/x"), // 1 real file
	}
	n, err := r.ExtractSelection(steps, filepath.Join(t.TempDir(), "out"), nil)
	if err != nil {
		t.Fatalf("ExtractSelection: %v", err)
	}
	if n != 1 {
		t.Fatalf("extracted count = %d, want 1 (only the file member)", n)
	}
	if len(store.opened) != 1 || store.opened[0] != fileRef {
		t.Fatalf("only the file-bearing archive should be opened, opened: %v", store.opened)
	}
}

// TestExtractSelectionMissingCopy: a selected archive with no available copy fails
// the whole recovery with archivefs.ErrMissingCopy (for the drill's classification),
// rather than silently extracting a partial set.
func TestExtractSelectionMissingCopy(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[record.Ref][]byte{}} // no copies
	r := New(testDeps(store, chainArchives(dle)))
	steps := []recovery.ExtractStep{extractStep("run-2026-06-02.001", dle, 1, "./var/x")}
	_, err := r.ExtractSelection(steps, filepath.Join(t.TempDir(), "out"), nil)
	if !errors.Is(err, archivefs.ErrMissingCopy) {
		t.Fatalf("want ErrMissingCopy for a selection with no available copy, got: %v", err)
	}
}

// TestExtractSelectionClientKeyGate: file-level recovery always decodes
// server-side, so a client-only symmetric key is infeasible and fails fast before
// any media is read (browse stays keyless; only extraction needs the key).
func TestExtractSelectionClientKeyGate(t *testing.T) {
	dle := "app01-data"
	store := &fakeStore{payloads: map[record.Ref][]byte{
		ref("run-2026-06-02.001", dle, 1): []byte("l1"),
	}}
	d := testDeps(store, chainArchives(dle))
	d.EncryptionFor = func(string) (config.EncryptConfig, bool) {
		return config.EncryptConfig{Scheme: "gpg", At: "client", PassphraseFile: "/keys/pass"}, true
	}
	r := New(d)
	steps := []recovery.ExtractStep{extractStep("run-2026-06-02.001", dle, 1, "./var/x")}
	_, err := r.ExtractSelection(steps, filepath.Join(t.TempDir(), "out"), nil)
	if err == nil || !strings.Contains(err.Error(), "passphrase never leaves the client") {
		t.Fatalf("want a client-key infeasibility error, got: %v", err)
	}
	if len(store.opened) != 0 {
		t.Fatalf("the gate must fire before any media read, opened: %v", store.opened)
	}
}
