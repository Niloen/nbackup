// Package recovery is NBackup's pure recovery planning: everything needed to decide
// *what to extract* to get data back, working over archive metadata only — the I/O
// and extraction live in the restorer. It computes the restore chain of a DLE as of
// a target run (chain.go), resolves an as-of date/time to that run (asof.go),
// reconstructs a browsable virtual filesystem and turns a file selection into the
// minimal per-archive extractions (this file), and holds the navigable state of an
// interactive browse (session.go).
//
// Browsing needs no separate index server: the "index" is each archive's
// per-archive member index (a gzip file on the medium, cached server-side and loaded
// lazily by the clerk — the catalog cache itself stays member-free). BuildTree takes a
// loader for it and merges the member lists of the restore chain (the full plus every
// later incremental up to the target) so each path resolves to the most recent archive
// that holds it — exactly the file content as of the target date.
//
// Fidelity caveat: GNU tar records deletions in its snapshot, not in the member
// index, so the merged tree is a union (most-recent-wins per path). A file deleted
// at a later incremental still appears in the browse view; a whole-DLE chain
// restore (engine.Restore) applies deletions, but selected-file recovery extracts
// each chosen file from the archive that last held it and does not delete.
package recovery

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/record"
)

// Source identifies the archive a path's as-of-date content lives in, plus the
// raw tar member name to hand to the extractor.
type Source struct {
	Step
	Member string // the producing archiver's verbatim member token, replayed to it on extract (e.g. "./etc/hosts")
}

// Node is a file or directory in the reconstructed virtual filesystem. A node has
// a Source when an archive in the chain holds it as a member; purely structural
// parent directories (implied by a deeper member) have none.
type Node struct {
	name     string
	path     string // clean path from the DLE root, e.g. "etc/hosts"
	dir      bool
	src      *Source
	children map[string]*Node
}

// Name returns the node's base name.
func (n *Node) Name() string { return n.name }

// Path returns the node's clean path relative to the DLE root.
func (n *Node) Path() string { return n.path }

// IsDir reports whether the node is a directory.
func (n *Node) IsDir() bool { return n.dir }

// Children returns the node's entries sorted directories-first, then by name.
func (n *Node) Children() []*Node {
	out := make([]*Node, 0, len(n.children))
	for _, c := range n.children {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dir != out[j].dir {
			return out[i].dir // directories first
		}
		return out[i].name < out[j].name
	})
	return out
}

// Tree is a DLE's reconstructed filesystem as of a target date.
type Tree struct {
	DLE       string
	TargetRun string // the run resolved for the as-of date
	AsOf      string // the requested as-of date (YYYY-MM-DD)
	chainLen  int    // archives in the restore chain (1 = full only, no incrementals)
	root      *Node
}

// HasIncrementals reports whether the restore chain includes any incremental beyond
// the full. The file-level deletion caveat only applies then — with a full-only chain
// there is no later incremental that could have deleted a file — so callers gate the
// note on it.
func (t *Tree) HasIncrementals() bool { return t.chainLen > 1 }

// BuildTree reconstructs the filesystem of dle as of asOf (YYYY-MM-DD) by merging
// the member lists of the restore chain in run order, so each path resolves to the
// most recent archive that holds it. The member lists are loaded via members (the
// catalog cache holds the run index, not the member lists — those are loaded lazily).
func BuildTree(archives []record.Archive, dle, asOf string, members func(runID string, level int) ([]record.Member, error)) (*Tree, error) {
	target, err := AsOf(archives, asOf)
	if err != nil {
		return nil, err
	}
	steps, err := Chain(archives, dle, target)
	if err != nil {
		return nil, err
	}
	t := &Tree{
		DLE:       dle,
		TargetRun: target,
		AsOf:      asOf,
		chainLen:  len(steps),
		root:      &Node{dir: true, children: map[string]*Node{}},
	}
	for _, st := range steps {
		ms, err := members(st.RunID, st.Level)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			t.insert(m.Path, &Source{Step: st, Member: m.Path})
		}
	}
	return t, nil
}

// insert adds a tar member to the tree, creating parent directories as needed.
// A later call (a more recent archive) overwrites the leaf's Source, so the tree
// reflects most-recent-wins.
func (t *Tree) insert(member string, src *Source) {
	path, isDir := cleanMember(member)
	if path == "" {
		return // the archive root ("./")
	}
	parts := strings.Split(path, "/")
	n := t.root
	cur := ""
	for i, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur += "/" + p
		}
		child, ok := n.children[p]
		if !ok {
			child = &Node{name: p, path: cur, dir: true, children: map[string]*Node{}}
			n.children[p] = child
		}
		if i == len(parts)-1 {
			child.dir = isDir
			child.src = src
		}
		n = child
	}
}

// cleanMember normalizes an archive member ("./etc/hosts", "./etc/") into a clean
// path relative to the DLE root and whether it is a directory. It relies only on
// the generic Members convention (slash-separated, trailing slash = directory) and
// is tolerant of an optional leading "./"; it does not assume the producing archiver.
func cleanMember(m string) (path string, dir bool) {
	dir = strings.HasSuffix(m, "/")
	return strings.Trim(strings.TrimPrefix(m, "./"), "/"), dir
}

// Lookup finds the node at a path relative to the DLE root. An empty path, ".",
// or "/" resolves to the root.
func (t *Tree) Lookup(p string) (*Node, bool) {
	n := t.root
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" || part == "." {
			continue
		}
		c, ok := n.children[part]
		if !ok {
			return nil, false
		}
		n = c
	}
	return n, true
}

// ExtractStep is one archive to extract, with the exact member names to pull from
// it. A file selection groups into the fewest steps — one per source archive.
type ExtractStep struct {
	Step
	Members []string // raw tar member names
}

// Collect resolves a set of selected paths (files or directories) into the
// per-archive extractions that reproduce them as of the target date. Selecting a
// directory takes every descendant; each path also pulls its ancestor directory
// members so directory metadata is restored. The steps and their members are
// returned in a deterministic order.
func (t *Tree) Collect(paths []string) ([]ExtractStep, error) {
	chosen := map[string]*Source{}
	for _, p := range paths {
		n, ok := t.Lookup(p)
		if !ok {
			return nil, fmt.Errorf("not found: %s", p)
		}
		gather(n, chosen)
	}
	// Pull ancestor directory members so parent-dir ownership/permissions land.
	for cp := range chosen {
		for _, anc := range ancestors(cp) {
			if n, ok := t.Lookup(anc); ok && n.src != nil {
				chosen[anc] = n.src
			}
		}
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("nothing to extract")
	}

	type key struct {
		run   string
		level int
	}
	steps := map[key]*ExtractStep{}
	var order []key
	for _, cp := range sortedKeys(chosen) {
		s := chosen[cp]
		k := key{s.RunID, s.Level}
		st, ok := steps[k]
		if !ok {
			st = &ExtractStep{Step: s.Step}
			steps[k] = st
			order = append(order, k)
		}
		st.Members = append(st.Members, s.Member)
	}
	// Run order: by run id (date-sortable), then level.
	sort.Slice(order, func(i, j int) bool {
		if order[i].run != order[j].run {
			return order[i].run < order[j].run
		}
		return order[i].level < order[j].level
	})
	out := make([]ExtractStep, 0, len(order))
	for _, k := range order {
		out = append(out, *steps[k])
	}
	return out, nil
}

// gather walks a node's subtree, recording every node that has a Source.
func gather(n *Node, into map[string]*Source) {
	if n.src != nil {
		into[n.path] = n.src
	}
	for _, c := range n.children {
		gather(c, into)
	}
}

// ancestors returns the clean paths of a path's parent directories, nearest last.
func ancestors(p string) []string {
	parts := strings.Split(p, "/")
	var out []string
	cur := ""
	for i := 0; i < len(parts)-1; i++ {
		if cur == "" {
			cur = parts[i]
		} else {
			cur += "/" + parts[i]
		}
		out = append(out, cur)
	}
	return out
}

func sortedKeys(m map[string]*Source) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
