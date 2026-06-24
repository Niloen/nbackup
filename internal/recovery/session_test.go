package recovery

import (
	"slices"
	"testing"
)

func newTestSession(t *testing.T) *Session {
	t.Helper()
	tree, err := BuildTree(scenario(), "app", "2026-06-22")
	if err != nil {
		t.Fatal(err)
	}
	return NewSession(tree)
}

// TestSessionResolve covers absolute, relative, "." and ".." path resolution against
// the current directory.
func TestSessionResolve(t *testing.T) {
	s := newTestSession(t)
	if err := s.Cd("etc"); err != nil {
		t.Fatalf("cd etc: %v", err)
	}
	cases := map[string]string{
		"hosts":       "etc/hosts", // relative to cwd
		"./hosts":     "etc/hosts",
		"/var/log":    "var/log", // absolute ignores cwd
		"..":          "",        // up to root
		"../var":      "var",
		"../var/log/": "var/log",
	}
	for arg, want := range cases {
		if got := s.Resolve(arg); got != want {
			t.Errorf("Resolve(%q) from /etc = %q, want %q", arg, got, want)
		}
	}
}

// TestSessionCd rejects a non-existent path and a file, and leaves cwd unchanged on
// failure.
func TestSessionCd(t *testing.T) {
	s := newTestSession(t)
	if err := s.Cd("etc"); err != nil {
		t.Fatalf("cd etc: %v", err)
	}
	if err := s.Cd("hosts"); err == nil { // a file, not a dir
		t.Error("cd into a file should fail")
	}
	if s.Cwd() != "etc" {
		t.Errorf("cwd after failed cd = %q, want etc", s.Cwd())
	}
	if err := s.Cd("nope"); err == nil {
		t.Error("cd into a missing path should fail")
	}
	if err := s.Cd(""); err != nil || s.Cwd() != "" {
		t.Errorf("cd to root: err=%v cwd=%q", err, s.Cwd())
	}
}

// TestSessionSelection covers add (relative to cwd), not-found reporting, sorted
// selection, and remove.
func TestSessionSelection(t *testing.T) {
	s := newTestSession(t)
	_ = s.Cd("etc")
	added, notFound := s.Add([]string{"hosts", "new.conf", "ghost"})
	if !slices.Equal(added, []string{"etc/hosts", "etc/new.conf"}) {
		t.Errorf("added = %v", added)
	}
	if !slices.Equal(notFound, []string{"etc/ghost"}) {
		t.Errorf("notFound = %v", notFound)
	}
	if sel := s.Selection(); !slices.Equal(sel, []string{"etc/hosts", "etc/new.conf"}) {
		t.Errorf("selection = %v", sel)
	}
	if removed := s.Remove([]string{"hosts"}); !slices.Equal(removed, []string{"etc/hosts"}) {
		t.Errorf("removed = %v", removed)
	}
	if sel := s.Selection(); !slices.Equal(sel, []string{"etc/new.conf"}) {
		t.Errorf("selection after remove = %v", sel)
	}
	s.Clear()
	if sel := s.Selection(); len(sel) != 0 {
		t.Errorf("selection after clear = %v", sel)
	}
}
