package recovery

import (
	"io"
	"path"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/record"
)

// fakeAssembler models postgres's delta naming: "INCREMENTAL.<name>" is a
// delta version of "<name>". Assemble is unused by tree building.
type fakeAssembler struct{}

func (fakeAssembler) Logical(member string) (string, bool) {
	dir, name := path.Split(member)
	if rest, ok := strings.CutPrefix(name, "INCREMENTAL."); ok {
		return dir + rest, true
	}
	return member, false
}

func (fakeAssembler) Assemble([]archiver.Version) (io.ReadCloser, error) {
	return nil, nil
}

// pgScenario: a full and an incremental of a postgres-shaped DLE. The
// incremental re-enumerates every live file (the assembler census contract):
// 2619 changed (a delta), 3000 unchanged (a zero-block delta stub), config
// copied whole; 4000 was DELETED (absent), and 5000 is new (stored whole).
func pgScenario() []record.Archive {
	return []record.Archive{{
		Run: "run-2026-06-21.001", DLE: "db", Level: 0, Archiver: "postgres", Compress: "none",
		Members: []record.Member{
			{Path: "base/", Off: 0},
			{Path: "base/5/", Off: 512},
			{Path: "base/5/2619", Off: 1024},
			{Path: "base/5/3000", Off: 2048},
			{Path: "base/5/4000", Off: 3072},
			{Path: "postgresql.conf", Off: 4096},
		},
	}, {
		Run: "run-2026-06-22.001", DLE: "db", Level: 1, Archiver: "postgres", Compress: "none",
		Members: []record.Member{
			{Path: "base/", Off: 0},
			{Path: "base/5/", Off: 512},
			{Path: "base/5/INCREMENTAL.2619", Off: 1024},
			{Path: "base/5/INCREMENTAL.3000", Off: 2048},
			{Path: "base/5/5000", Off: 3072},
			{Path: "postgresql.conf", Off: 4096},
		},
	}}
}

func assemblerFor(string) archiver.Assembler { return fakeAssembler{} }

func pgTree(t *testing.T) *Tree {
	t.Helper()
	tree, err := BuildTreeForRun(pgScenario(), "db", "run-2026-06-22.001", membersOf(pgScenario(), "db"), assemblerFor)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// TestAssemblerTreeLogicalPaths: delta members fold onto their logical file —
// one node, whose versions span the chain, never a raw INCREMENTAL.* entry.
func TestAssemblerTreeLogicalPaths(t *testing.T) {
	tree := pgTree(t)
	if _, ok := tree.Lookup("base/5/INCREMENTAL.2619"); ok {
		t.Fatal("raw delta path must not appear in the tree")
	}
	n, ok := tree.Lookup("base/5/2619")
	if !ok {
		t.Fatal("logical path missing")
	}
	if !n.NeedsAssembly() {
		t.Fatal("delta-tipped file must need assembly")
	}
	vs := n.Versions()
	if len(vs) != 2 || vs[0].Delta || !vs[1].Delta {
		t.Fatalf("versions = %+v (want whole then delta)", vs)
	}
	if vs[0].Src.RunID != "run-2026-06-21.001" || vs[1].Src.Member != "base/5/INCREMENTAL.2619" {
		t.Fatalf("version sources wrong: %+v", vs)
	}
	// A file stored whole in the newest level needs no assembly.
	if n, ok := tree.Lookup("postgresql.conf"); !ok || n.NeedsAssembly() {
		t.Fatalf("whole file: ok=%v needsAssembly=%v", ok, ok && n.NeedsAssembly())
	}
}

// TestAssemblerCensus: the newest level's member list is authoritative — the
// deleted file is pruned, the new one present.
func TestAssemblerCensus(t *testing.T) {
	tree := pgTree(t)
	if _, ok := tree.Lookup("base/5/4000"); ok {
		t.Fatal("file deleted before the newest level must be pruned")
	}
	if _, ok := tree.Lookup("base/5/5000"); !ok {
		t.Fatal("file new in the newest level missing")
	}
	// Without an assembler the union view keeps the deleted file (the
	// documented gnutar caveat) — pin the contrast.
	union, err := BuildTreeForRun(pgScenario(), "db", "run-2026-06-22.001", membersOf(pgScenario(), "db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := union.Lookup("base/5/4000"); !ok {
		t.Fatal("default union tree should keep the older file")
	}
}

// TestCollectAssemblies: selecting a delta-tipped file yields an Assembly with
// the chain versions in order, not a plain step; whole files still collect as
// steps.
func TestCollectAssemblies(t *testing.T) {
	tree := pgTree(t)
	steps, asms, err := tree.Collect([]string{"base/5/2619", "postgresql.conf"})
	if err != nil {
		t.Fatal(err)
	}
	if len(asms) != 1 || asms[0].Path != "base/5/2619" {
		t.Fatalf("assemblies = %+v", asms)
	}
	if vs := asms[0].Versions; len(vs) != 2 || vs[0].Delta || !vs[1].Delta {
		t.Fatalf("assembly versions = %+v", asms[0].Versions)
	}
	// postgresql.conf extracts plainly from the newest archive.
	found := false
	for _, st := range steps {
		for _, m := range st.Members {
			if m == "postgresql.conf" && st.RunID == "run-2026-06-22.001" {
				found = true
			}
			if strings.Contains(m, "INCREMENTAL.") {
				t.Fatalf("a delta member leaked into plain steps: %q", m)
			}
		}
	}
	if !found {
		t.Fatal("whole file missing from steps")
	}
}

// TestCollectDirectoryWithAssemblies: selecting a directory sweeps both kinds.
func TestCollectDirectoryWithAssemblies(t *testing.T) {
	tree := pgTree(t)
	steps, asms, err := tree.Collect([]string{"base"})
	if err != nil {
		t.Fatal(err)
	}
	if len(asms) != 2 { // 2619 and 3000 are delta-tipped
		t.Fatalf("assemblies = %+v", asms)
	}
	// 5000 (whole in newest) rides a plain step.
	found := false
	for _, st := range steps {
		for _, m := range st.Members {
			if m == "base/5/5000" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("whole new file missing from steps")
	}
}
