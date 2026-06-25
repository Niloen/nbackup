// Package recovery reconstructs a browsable virtual filesystem of a DLE as of a
// given date and resolves a file selection into the minimal set of per-archive
// extractions — NBackup's amrecover. It is pure: it works over slot metadata
// (the member index each seal records) and returns what to extract; the engine
// performs the I/O.
//
// The "index" amrecover keeps in a separate index server is, here, already in the
// catalog: every record.Archive carries its tar member list. A recovery tree merges
// the member lists of the restore chain (the full plus every later incremental up
// to the target) so that each path resolves to the most recent archive that holds
// it — exactly the file content as of the target date.
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
	"github.com/Niloen/nbackup/internal/restore"
)

// Source identifies the archive a path's as-of-date content lives in, plus the
// raw tar member name to hand to the extractor.
type Source struct {
	SlotID   string
	DLE      string
	Level    int
	Archiver string
	Compress string
	Encrypt  string
	Member   string // the producing archiver's verbatim member token, replayed to it on extract (e.g. "./etc/hosts")
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
	DLE        string
	TargetSlot string // the slot resolved for the as-of date
	AsOf       string // the requested as-of date (YYYY-MM-DD)
	root       *Node
}

// AsOf resolves an as-of date to the target slot: the most recent slot whose run
// date is on or before the date. Slots must be in run order.
func AsOf(slots []*record.Slot, asOf string) (string, error) {
	target := ""
	for _, s := range slots {
		if s.Date <= asOf {
			target = s.ID
		}
	}
	if target == "" {
		return "", fmt.Errorf("no backup on or before %s", asOf)
	}
	return target, nil
}

// BuildTree reconstructs the filesystem of dle as of asOf (YYYY-MM-DD) by merging
// the member lists of the restore chain in run order, so each path resolves to the
// most recent archive that holds it.
func BuildTree(slots []*record.Slot, dle, asOf string) (*Tree, error) {
	target, err := AsOf(slots, asOf)
	if err != nil {
		return nil, err
	}
	steps, err := restore.Chain(slots, dle, target)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*record.Slot, len(slots))
	for _, s := range slots {
		byID[s.ID] = s
	}
	t := &Tree{
		DLE:        dle,
		TargetSlot: target,
		AsOf:       asOf,
		root:       &Node{dir: true, children: map[string]*Node{}},
	}
	for _, st := range steps {
		ar := findArchive(byID[st.SlotID], dle, st.Level)
		if ar == nil {
			continue
		}
		for _, m := range ar.Members {
			t.insert(m, &Source{
				SlotID: st.SlotID, DLE: dle, Level: st.Level,
				Archiver: st.Archiver, Compress: st.Compress, Encrypt: st.Encrypt, Member: m,
			})
		}
	}
	return t, nil
}

func findArchive(s *record.Slot, dle string, level int) *record.Archive {
	if s == nil {
		return nil
	}
	for i := range s.Archives {
		if s.Archives[i].DLE == dle && s.Archives[i].Level == level {
			return &s.Archives[i]
		}
	}
	return nil
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
	SlotID   string
	DLE      string
	Level    int
	Archiver string
	Compress string
	Encrypt  string
	Members  []string // raw tar member names
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
		slot  string
		level int
	}
	steps := map[key]*ExtractStep{}
	var order []key
	for _, cp := range sortedKeys(chosen) {
		s := chosen[cp]
		k := key{s.SlotID, s.Level}
		st, ok := steps[k]
		if !ok {
			st = &ExtractStep{SlotID: s.SlotID, DLE: s.DLE, Level: s.Level, Archiver: s.Archiver, Compress: s.Compress, Encrypt: s.Encrypt}
			steps[k] = st
			order = append(order, k)
		}
		st.Members = append(st.Members, s.Member)
	}
	// Run order: by slot id (date-sortable), then level.
	sort.Slice(order, func(i, j int) bool {
		if order[i].slot != order[j].slot {
			return order[i].slot < order[j].slot
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
