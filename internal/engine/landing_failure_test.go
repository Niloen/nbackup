package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// listFailVolume opens fine but fails its Files() scan — an address-identified store
// that is reachable to open yet errors when the catalog bootstrap tries to index it
// (an access-denied bucket LIST). Registered as the "listfail" medium type.
type listFailVolume struct{ media.Volume }

func (listFailVolume) Files() ([]record.FileInfo, error) {
	return nil, errors.New("bucket list failed: access denied")
}

func init() {
	media.Register(media.Spec{
		Type: "listfail",
		New: func(opts media.Options, _ string) (media.Volume, error) {
			v, err := media.OpenVolume("disk", opts, "")
			if err != nil {
				return nil, err
			}
			return listFailVolume{v}, nil
		},
		Params:          []string{"path"},
		ConcurrentWrite: true,
	})
}

// TestRunFailsWhenLandingCannotBeIndexed exercises landing()'s catalog-bootstrap
// (EnsureFresh) failure branch: a landing that opens but cannot be scanned fails the
// run with a "cannot reach landing medium … to index existing backups" error, distinct
// from the open-failure branch.
func TestRunFailsWhenLandingCannotBeIndexed(t *testing.T) {
	cfg := &config.Config{
		Landing:  config.MediumList{"listfail"},
		Media:    map[string]config.Media{"listfail": {Type: "listfail", Params: map[string]string{"path": t.TempDir()}}},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if runErr == nil {
		t.Fatal("a landing that cannot be indexed must fail the run")
	}
	if !strings.Contains(runErr.Error(), "to index existing backups") {
		t.Errorf("error should point at the catalog bootstrap: %v", runErr)
	}
}

// capturingLogf collects log lines so a test can assert on the diagnostics a
// command emits (warnings, skips) without them going to /dev/null.
type capturingLogf struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturingLogf) log(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (c *capturingLogf) contains(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func (c *capturingLogf) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

// TestRunFailsWhenCloudLandingUnreachable exercises the landing() credential-wrapping
// path: a cloud landing whose bucket cannot be opened must fail the run with the
// medium named and the SDK-credential hint, not a bare provider error.
func TestRunFailsWhenCloudLandingUnreachable(t *testing.T) {
	cfg := &config.Config{
		Landing: config.MediumList{"cloud"},
		Media: map[string]config.Media{
			// A bogus URL scheme: gocloud registers no driver for it, so opening the
			// bucket (which landing() does on first use) fails deterministically with
			// no network.
			"cloud": {Type: "cloud", Params: map[string]string{"url": "bogusscheme://nope"}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New should succeed (the bucket is not opened until first use): %v", err)
	}

	_, runErr := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if runErr == nil {
		t.Fatal("a run onto an unreachable cloud landing must fail")
	}
	if !strings.Contains(runErr.Error(), `landing medium "cloud"`) {
		t.Errorf("error should name the landing medium: %v", runErr)
	}
	// The wrapper points the operator at the SDK credential environment rather than
	// leaking the raw provider error alone.
	if !strings.Contains(runErr.Error(), "AWS_") {
		t.Errorf("cloud open failure should carry the credential hint: %v", runErr)
	}
}

// TestRunFailsWhenDiskLandingUnwritable exercises the non-cloud open-failure branch:
// a disk landing whose path is an existing regular file (so the run root cannot be
// created) fails with "cannot open landing medium", not the cloud credential hint.
func TestRunFailsWhenDiskLandingUnwritable(t *testing.T) {
	// A regular file where a directory is required: MkdirAll(<file>/runs) fails.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Landing:  config.MediumList{"disk"},
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": blocker}}},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New should defer the open: %v", err)
	}
	_, runErr := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if runErr == nil {
		t.Fatal("a run onto an unwritable disk landing must fail")
	}
	if !strings.Contains(runErr.Error(), "cannot open landing medium") {
		t.Errorf("non-cloud open failure should say 'cannot open landing medium': %v", runErr)
	}
	if strings.Contains(runErr.Error(), "AWS_") {
		t.Errorf("a disk failure must not carry the cloud credential hint: %v", runErr)
	}
}

// TestRebuildSkipsUnopenableMedium exercises RebuildCatalog's skip-on-unopenable-medium
// branch: a config with a landing that opens fine and a second medium that cannot be
// opened must warn-and-skip the bad one and still rebuild from the good one, rather than
// aborting the whole rebuild.
func TestRebuildSkipsUnopenableMedium(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "rebuild me")

	cfg := &config.Config{
		Landing: config.MediumList{"disk"},
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			// A second medium that fails to open (bogus cloud URL). It is not the
			// landing, so RebuildCatalog reaches it in the per-medium loop and must
			// skip it.
			"broken": {Type: "cloud", Params: map[string]string{"url": "bogusscheme://nope"}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
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
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("dump: %v", err)
	}

	cap := &capturingLogf{}
	rep, err := eng.RebuildCatalog(true, cap.log)
	if err != nil {
		t.Fatalf("rebuild must not abort on one unopenable medium: %v", err)
	}
	if rep.Runs != 1 {
		t.Fatalf("rebuild indexed %d runs, want 1 (from the good landing)", rep.Runs)
	}
	if !cap.contains("skipping medium \"broken\"") {
		t.Errorf("rebuild should warn that it skipped the broken medium; log:\n%s", cap.joined())
	}
}
