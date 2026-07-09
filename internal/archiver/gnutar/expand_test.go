package gnutar

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/record"
)

// sources returns the Source of each scope, sorted.
func sources(scopes []archiver.Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = s.Source
	}
	sort.Strings(out)
	return out
}

// findScope returns the scope with the given Source, or fails.
func findScope(t *testing.T, scopes []archiver.Scope, source string) archiver.Scope {
	t.Helper()
	for _, s := range scopes {
		if s.Source == source {
			return s
		}
	}
	t.Fatalf("no scope with source %q in %+v", source, scopes)
	return archiver.Scope{}
}

func mkdirs(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestExpandPlain: a wildcard-free source resolves to exactly one scope (no enumeration).
func TestExpandPlain(t *testing.T) {
	m := newArchiver(t, t.TempDir())
	scopes, err := m.Expand(archiver.SourcePattern{Pattern: "/var/log", Exclude: []string{"*.gz"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0].Source != "/var/log" || scopes[0].Base != "" {
		t.Fatalf("plain: want one scope {Base:\"\", Source:/var/log}, got %+v", scopes)
	}
	if len(scopes[0].Exclude) != 1 || scopes[0].Exclude[0] != "*.gz" {
		t.Errorf("plain: configured excludes not carried: %+v", scopes[0].Exclude)
	}
}

// TestExpandSelection: a scalar wildcard yields one scope per matching directory, no rest.
func TestExpandSelection(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "web-app", "web-api", "legacy")
	if err := os.WriteFile(filepath.Join(root, "web-notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err) // a matching-name FILE must not become a scope (directories only)
	}
	m := newArchiver(t, t.TempDir())

	scopes, err := m.Expand(archiver.SourcePattern{Pattern: filepath.Join(root, "web-*")})
	if err != nil {
		t.Fatal(err)
	}
	got := sources(scopes)
	want := []string{filepath.Join(root, "web-api"), filepath.Join(root, "web-app")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("selection web-*: want %v, got %v", want, got)
	}
	for _, s := range scopes {
		if s.Base != root {
			t.Errorf("selection: want Base=%q, got %q", root, s.Base)
		}
		if s.Source == root {
			t.Errorf("selection must emit no remainder (Source==Base), got one")
		}
	}
}

// TestExpandPartition: the mapping form yields the matches plus the rest (Source==Base) with
// each match carved out as an anchored (leading-"/") exclude.
func TestExpandPartition(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "alice", "bob", "carol")
	m := newArchiver(t, t.TempDir())

	scopes, err := m.Expand(archiver.SourcePattern{Base: root, Pattern: "*", Exclude: []string{"*.log"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 4 { // alice, bob, carol + the rest
		t.Fatalf("partition: want 4 scopes, got %d: %+v", len(scopes), scopes)
	}
	// each match: Base=root, Source under root, carries the dumptype exclude only
	alice := findScope(t, scopes, filepath.Join(root, "alice"))
	if alice.Base != root {
		t.Errorf("match Base: want %q, got %q", root, alice.Base)
	}
	if len(alice.Exclude) != 1 || alice.Exclude[0] != "*.log" {
		t.Errorf("match excludes: want [*.log], got %v", alice.Exclude)
	}
	// the rest: Source == Base, excludes = dumptype glob + anchored carves for each child
	rest := findScope(t, scopes, root)
	if rest.Base != root {
		t.Errorf("rest Base: want %q, got %q", root, rest.Base)
	}
	wantExcl := map[string]bool{"*.log": true, "/alice": true, "/bob": true, "/carol": true}
	if len(rest.Exclude) != len(wantExcl) {
		t.Fatalf("rest excludes: want %v, got %v", wantExcl, rest.Exclude)
	}
	for _, e := range rest.Exclude {
		if !wantExcl[e] {
			t.Errorf("rest: unexpected exclude %q (want %v)", e, wantExcl)
		}
	}
}

// TestExpandPartitionLiteralToken: a literal (non-glob) partition token still enumerates —
// the named child becomes a match with an ABSOLUTE Source and the rest carves it. This pins
// the fix for the early-return bug that produced a relative Source and dropped the rest.
func TestExpandPartitionLiteralToken(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "media", "other")
	m := newArchiver(t, t.TempDir())

	scopes, err := m.Expand(archiver.SourcePattern{Base: root, Pattern: "media"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 { // the match + the rest
		t.Fatalf("literal token: want 2 scopes, got %d: %+v", len(scopes), scopes)
	}
	match := findScope(t, scopes, filepath.Join(root, "media"))
	if match.Base != root {
		t.Errorf("match Base: want %q, got %q", root, match.Base)
	}
	rest := findScope(t, scopes, root)
	if len(rest.Exclude) != 1 || rest.Exclude[0] != "/media" {
		t.Errorf("rest carves: want [/media], got %v", rest.Exclude)
	}
}

// TestExpandPartitionTokenMatchingNothing: a typo'd literal token yields no match and the
// rest degenerates to the whole base — coverage preserved, nothing dropped.
func TestExpandPartitionTokenMatchingNothing(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "media")
	m := newArchiver(t, t.TempDir())

	scopes, err := m.Expand(archiver.SourcePattern{Base: root, Pattern: "nosuch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0].Source != root || len(scopes[0].Exclude) != 0 {
		t.Fatalf("want the whole base as the rest with no carves, got %+v", scopes)
	}
}

// TestAnchoredCarveIsNotContentGlob proves a partition's carve exclude ("/alice") is anchored
// at the archive root: dumping the rest of /data drops top-level /data/alice but keeps the
// like-named /data/keep/alice deeper in the tree — an unanchored --exclude=alice would drop
// both. This locks the createArgs anchoring the remainder relies on.
func TestAnchoredCarveIsNotContentGlob(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "alice", "keep/alice")
	if err := os.WriteFile(filepath.Join(root, "keep", "alice", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newArchiver(t, t.TempDir())

	// the rest of /data with only /alice carved (keep is deliberately not a match here).
	rest := archiver.Scope{Base: root, Source: root, Exclude: []string{"/alice"}}
	out := filepath.Join(t.TempDir(), "rest.tar")
	res := backup(t, m, archiver.BackupRequest{DLE: "rest", Scope: rest, Level: 0, BaseLevel: -1}, out)
	var keptNested, carvedTop bool
	for _, mem := range res.Members {
		p := strings.TrimPrefix(mem.Path, "./")
		if strings.HasPrefix(p, "keep/alice") {
			keptNested = true
		}
		if p == "alice/" || p == "alice" || strings.HasPrefix(p, "alice/") {
			carvedTop = true
		}
	}
	if !keptNested {
		t.Errorf("the rest should keep keep/alice (anchored carve of /alice only); members: %v", memberPaths(res.Members))
	}
	if carvedTop {
		t.Errorf("the rest should have carved out top-level /alice; members: %v", memberPaths(res.Members))
	}
}

// TestUnexcludedSubtreeReentersChainWholesale pins the un-exclude direction of the snar
// semantics (the peer of TestNewExcludeIsNotADeletion): a subtree excluded at L0 and
// un-excluded at L1 is dumped WHOLESALE at L1 (it is not in the snar, so tar treats it as
// new), and the chain restore contains it completely. This is what lets the partition
// re-baseline guard fire on carve ADDITIONS only — a carve removed while its directory
// still exists re-enters the chain via a fat incremental, with no restore hole.
func TestUnexcludedSubtreeReentersChainWholesale(t *testing.T) {
	src := t.TempDir()
	m := newArchiver(t, t.TempDir())
	write(t, filepath.Join(src, "alice", "f1"), "a1")
	write(t, filepath.Join(src, "keep", "f2"), "k1")

	l0 := filepath.Join(t.TempDir(), "l0.tar")
	backup(t, m, archiver.BackupRequest{DLE: "app", Scope: archiver.Scope{Source: src, Exclude: []string{"/alice"}}, Level: 0, BaseLevel: -1}, l0)

	// carve removed, dir untouched: L1 without the exclude must contain alice wholesale.
	l1 := filepath.Join(t.TempDir(), "l1.tar")
	res := backup(t, m, archiver.BackupRequest{DLE: "app", Scope: archiver.Scope{Source: src}, Level: 1, BaseLevel: 0}, l1)
	var hasAliceFile bool
	for _, mem := range res.Members {
		if strings.TrimPrefix(mem.Path, "./") == "alice/f1" {
			hasAliceFile = true
		}
	}
	if !hasAliceFile {
		t.Fatalf("un-excluded subtree not dumped wholesale at L1 — the re-baseline guard must also fire on carve removals; members: %v", memberPaths(res.Members))
	}

	// and the chain restore reproduces it.
	dest := t.TempDir()
	restore(t, m, l0, dest)
	restore(t, m, l1, dest)
	got, err := os.ReadFile(filepath.Join(dest, "alice", "f1"))
	if err != nil || string(got) != "a1" {
		t.Fatalf("chain restore missing un-excluded subtree: %v (%q)", err, got)
	}
}

func memberPaths(members []record.Member) []string {
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = m.Path
	}
	sort.Strings(out)
	return out
}
