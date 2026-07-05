package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
)

// fanoutFixture builds a config whose landing route fans out to two media, with or
// without a holding disk in between, and returns the engine plus the DLE slug.
func fanoutFixture(t *testing.T, holding bool) (*Engine, string) {
	t.Helper()
	src := t.TempDir()
	write(t, filepath.Join(src, "hello.txt"), "landed twice")
	media := map[string]config.Media{
		"a": {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
		"b": {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
	}
	if holding {
		media["scratch"] = config.Media{Type: "disk", Holding: true, Params: map[string]string{"path": t.TempDir()}}
	}
	cfg := &config.Config{
		Landing:  config.MediumList{"a", "b"},
		Media:    media,
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
	return eng, config.DLE{Host: "localhost", Path: src}.Name()
}

// assertFanout runs one dump against a two-landing route and asserts both media hold
// the DLE's archive under ONE run, the primary stays the accounted medium, and the
// archive restores.
func assertFanout(t *testing.T, eng *Engine, dle string) {
	t.Helper()
	s, err := eng.Run(context.Background(), time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	got := map[string]int{}
	for _, p := range eng.Catalog().Placements(s.ID) {
		got[p.Medium] = len(p.Archives)
	}
	if got["a"] != 1 || got["b"] != 1 {
		t.Fatalf("placements = %v; want the archive on BOTH a and b", got)
	}
	if eng.Landing() != "a" {
		t.Fatalf("primary landing = %q; want a (first in the route)", eng.Landing())
	}
	dest := t.TempDir()
	if err := eng.Restore(s.ID, dle, dest, false, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertContent(t, filepath.Join(dest, "hello.txt"), "landed twice")
}

// TestFanoutViaHolding: `landing: [a, b]` with a holding disk — the archive stages
// once, drains to both media, and the holding disk ends empty (reclaimed only after
// both copies committed).
func TestFanoutViaHolding(t *testing.T) {
	eng, dle := fanoutFixture(t, true)
	assertFanout(t, eng, dle)
	vol, _, _, err := eng.dep.MediumVolume("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := vol.Files(); len(files) != 0 {
		t.Fatalf("holding disk must be empty after both drains, has %d file(s)", len(files))
	}
}

// TestFanoutDirectTee: the same route with NO holding disk — the dump stream tees
// straight to both media in lockstep parts.
func TestFanoutDirectTee(t *testing.T) {
	eng, dle := fanoutFixture(t, false)
	assertFanout(t, eng, dle)
}

// TestFanoutFlushCompletesRoute: crash-recovery for a fan-out — an archive stranded
// on a scratch disk (staged, never drained) is flushed to EVERY landing on its route
// once scratch is reconfigured as holding for `landing: [a, b]`, and the disk ends
// empty.
func TestFanoutFlushCompletesRoute(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "stranded.txt"), "complete my route")
	scratchDir := t.TempDir()
	workdir := t.TempDir()
	stateDir := t.TempDir()
	sources := []config.DLE{{Host: "localhost", Path: src}}

	stageCfg := &config.Config{
		Landing:  config.MediumList{"scratch"},
		Media:    map[string]config.Media{"scratch": {Type: "disk", Params: map[string]string{"path": scratchDir}}},
		Sources:  sources,
		Workdir:  workdir,
		StateDir: stateDir,
	}
	stageCfg.Compress.Scheme = "none"
	stageEng, err := New(stageCfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := stageEng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := stageEng.Run(context.Background(), time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("stage dump: %v", err)
	}

	flushCfg := &config.Config{
		Landing: config.MediumList{"a", "b"},
		Media: map[string]config.Media{
			"a":       {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
			"b":       {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
			"scratch": {Type: "disk", Holding: true, Params: map[string]string{"path": scratchDir}},
		},
		Sources:  sources,
		Workdir:  workdir,
		StateDir: stateDir,
	}
	flushCfg.Compress.Scheme = "none"
	flushEng, err := New(flushCfg)
	if err != nil {
		t.Fatal(err)
	}
	n, err := flushEng.Flush(time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), logfDiscard)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 1 {
		t.Fatalf("flush moved %d archives, want 1", n)
	}
	got := map[string]int{}
	for _, p := range flushEng.Catalog().Placements(s.ID) {
		got[p.Medium] = len(p.Archives)
	}
	if got["a"] != 1 || got["b"] != 1 || got["scratch"] != 0 {
		t.Fatalf("placements = %v; want the archive on a AND b, scratch reclaimed", got)
	}
	vol, _, _, err := flushEng.dep.MediumVolume("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := vol.Files(); len(files) != 0 {
		t.Fatalf("holding disk must be empty after the fan-out flush, has %d file(s)", len(files))
	}
}

// openFail is a medium type that always fails to open — a landing that is down
// before the run starts. Registered once for the tests below.
func init() {
	media.Register(media.Spec{
		Type: "openfail",
		New: func(media.Options, string) (media.Volume, error) {
			return nil, errors.New("medium down: connection refused")
		},
		Params:          []string{"path"},
		ConcurrentWrite: true,
	})
}

// TestFanoutSurvivesUnopenableSecondary: one landing on the route is down at
// window-open. Any-lane-suffices applies there too: the run proceeds on the
// survivor, lands everything on it, and succeeds.
func TestFanoutSurvivesUnopenableSecondary(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "hello.txt"), "one landing down")
	cfg := &config.Config{
		Landing: config.MediumList{"a", "down"},
		Media: map[string]config.Media{
			"a":    {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
			"down": {Type: "openfail", Params: map[string]string{"path": t.TempDir()}},
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
	s, err := eng.Run(context.Background(), time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump = %v; a down secondary must not fail the run", err)
	}
	got := map[string]int{}
	for _, p := range eng.Catalog().Placements(s.ID) {
		got[p.Medium] = len(p.Archives)
	}
	if got["a"] != 1 || got["down"] != 0 {
		t.Fatalf("placements = %v; want the archive on the survivor only", got)
	}
}

// TestFanoutFailsWhenWholeRouteUnopenable: every landing on the route is down at
// window-open — the run must fail (nothing could land anywhere).
func TestFanoutFailsWhenWholeRouteUnopenable(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "hello.txt"), "nowhere to go")
	cfg := &config.Config{
		Landing: config.MediumList{"down1", "down2"},
		Media: map[string]config.Media{
			"down1": {Type: "openfail", Params: map[string]string{"path": t.TempDir()}},
			"down2": {Type: "openfail", Params: map[string]string{"path": t.TempDir()}},
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
	_, err = eng.Run(context.Background(), time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), nil)
	// A down PRIMARY surfaces at the depot's catalog bootstrap ("cannot open landing
	// medium" — the primary is the catalog's home, so its open failure ends the run
	// before routing); a route whose SECONDARIES are all down too would surface via
	// the conductor's whole-route check. Either way the run must fail.
	if err == nil || (!strings.Contains(err.Error(), "no landing on its route could open") && !strings.Contains(err.Error(), "cannot open landing medium")) {
		t.Fatalf("dump = %v; want a landing-down failure", err)
	}
}
