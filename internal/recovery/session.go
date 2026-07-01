package recovery

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Session is the navigable, selectable state of an interactive recovery browse over
// one Tree: a current directory plus a set of selected paths, with path resolution
// (absolute "/a/b" or relative to cwd, honoring "." and ".."). It is pure — no
// terminal — so a CLI drives it for display while tests exercise it directly.
type Session struct {
	tree *Tree
	cwd  string          // clean path from the DLE root ("" = root)
	sel  map[string]bool // selected clean paths
}

// NewSession starts a browse at the tree's root with an empty selection.
func NewSession(tree *Tree) *Session {
	return &Session{tree: tree, sel: map[string]bool{}}
}

// Tree returns the underlying browse tree (e.g. for its TargetRun).
func (s *Session) Tree() *Tree { return s.tree }

// Cwd returns the current directory as a clean path from the DLE root ("" = root).
func (s *Session) Cwd() string { return s.cwd }

// Resolve turns a user-typed path (absolute "/a/b" or relative to cwd, with "." and
// "..") into a clean path from the DLE root.
func (s *Session) Resolve(arg string) string {
	base := "/" + s.cwd
	if strings.HasPrefix(arg, "/") {
		base = "/"
	}
	return strings.TrimPrefix(path.Clean(base+"/"+arg), "/")
}

// Lookup resolves arg against the cwd and returns the node at it.
func (s *Session) Lookup(arg string) (*Node, bool) {
	return s.tree.Lookup(s.Resolve(arg))
}

// Cd changes the current directory to arg (empty -> root). It errors if the target
// does not exist or is not a directory.
func (s *Session) Cd(arg string) error {
	if arg == "" {
		s.cwd = ""
		return nil
	}
	target := s.Resolve(arg)
	n, ok := s.tree.Lookup(target)
	if !ok {
		return fmt.Errorf("not found: /%s", target)
	}
	if !n.IsDir() {
		return fmt.Errorf("not a directory: /%s", target)
	}
	s.cwd = target
	return nil
}

// Add marks each path for recovery, returning the clean paths added and any that
// were not found (so the caller can report them).
func (s *Session) Add(args []string) (added, notFound []string) {
	for _, p := range args {
		target := s.Resolve(p)
		if _, ok := s.tree.Lookup(target); !ok {
			notFound = append(notFound, target)
			continue
		}
		s.sel[target] = true
		added = append(added, target)
	}
	return added, notFound
}

// Remove unmarks each path, returning the clean paths actually removed.
func (s *Session) Remove(args []string) (removed []string) {
	for _, p := range args {
		target := s.Resolve(p)
		if s.sel[target] {
			delete(s.sel, target)
			removed = append(removed, target)
		}
	}
	return removed
}

// Clear empties the selection.
func (s *Session) Clear() { s.sel = map[string]bool{} }

// Selection returns the selected paths, sorted.
func (s *Session) Selection() []string {
	out := make([]string, 0, len(s.sel))
	for k := range s.sel {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CollectSelection turns the current selection into the fewest per-archive
// extraction steps.
func (s *Session) CollectSelection() ([]ExtractStep, error) {
	return s.tree.Collect(s.Selection())
}
