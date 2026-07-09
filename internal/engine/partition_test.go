package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// TestPartitionedSourceEndToEnd drives the whole partition loop through the real engine:
// a {path, partition: "*"} source resolves at plan time into one DLE per child directory
// plus "the rest" (the bare base slug), the run dumps and catalogs each under its own
// identity, the rest's carve excludes are actually applied (children are NOT double-
// dumped into it — the R2 verbatim-Scope guarantee at the e2e level), and both a child
// and the rest restore correctly.
func TestPartitionedSourceEndToEnd(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "alice", "a.txt"), "alpha")
	write(t, filepath.Join(base, "bob", "b.txt"), "beta")
	write(t, filepath.Join(base, "loose.txt"), "loose bytes")

	cfg := &config.Config{
		Landing: config.MediumList{"vault"},
		Media: map[string]config.Media{
			"vault": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: base, Partition: "*"}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s, err := eng.Run(context.Background(), time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("partitioned dump: %v", err)
	}

	// The run catalogs exactly the resolved units: alice, bob, and the rest under the
	// bare base slug — each its own archive identity.
	want := map[string]bool{
		config.Slug("localhost", filepath.Join(base, "alice")): false,
		config.Slug("localhost", filepath.Join(base, "bob")):   false,
		config.Slug("localhost", base):                         false, // the rest
	}
	for _, a := range s.Archives {
		if _, ok := want[a.DLE]; !ok {
			t.Errorf("unexpected archive DLE %q", a.DLE)
			continue
		}
		want[a.DLE] = true
	}
	for slug, seen := range want {
		if !seen {
			t.Errorf("no archive for resolved DLE %q", slug)
		}
	}

	// A child restores its own content.
	aliceSlug := config.Slug("localhost", filepath.Join(base, "alice"))
	dest := t.TempDir()
	if err := eng.Restore(s.ID, aliceSlug, dest, false, nil); err != nil {
		t.Fatalf("restore child: %v", err)
	}
	assertContent(t, filepath.Join(dest, "a.txt"), "alpha")

	// The rest holds the loose file and NOT the carved children — the anchored carves
	// were applied at dump time, so nothing is double-stored.
	restSlug := config.Slug("localhost", base)
	dest = t.TempDir()
	if err := eng.Restore(s.ID, restSlug, dest, false, nil); err != nil {
		t.Fatalf("restore the rest: %v", err)
	}
	assertContent(t, filepath.Join(dest, "loose.txt"), "loose bytes")
	for _, carved := range []string{"alice", "bob"} {
		if _, err := os.Stat(filepath.Join(dest, carved)); !os.IsNotExist(err) {
			t.Errorf("the rest must not contain carved child %q (double-dump)", carved)
		}
	}
}
