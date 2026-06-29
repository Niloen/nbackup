package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/config"
)

func newCheckEngine(t *testing.T, sources []config.DLE) *Engine {
	t.Helper()
	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Sources:  sources,
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// hostLines finds a host's check lines in the report.
func hostLines(rep *CheckReport, host string) (HostCheck, bool) {
	for _, h := range rep.Hosts {
		if h.Host == host {
			return h, true
		}
	}
	return HostCheck{}, false
}

func anyMsg(lines []CheckLine, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l.Msg, sub) {
			return true
		}
	}
	return false
}

// TestCheckOfflineProbesLocalSkipsRemote verifies that --offline still probes a localhost
// DLE through the Local executor (the unified path) but only reports — does not connect to
// — a remote host.
func TestCheckOfflineProbesLocalSkipsRemote(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := newCheckEngine(t, []config.DLE{
		{Host: "localhost", Path: src},
		{Host: "app01", Path: "/data"},
	})
	if m, err := eng.archiverFor(config.DefaultDumpType, "localhost"); err != nil || m.Check() != nil {
		t.Skip("GNU tar not available")
	}

	rep := eng.Check(false) // offline
	if rep.Failures != 0 {
		t.Fatalf("offline check should not fail: %+v", rep)
	}
	local, ok := hostLines(rep, "localhost")
	if !ok || local.Remote {
		t.Fatal("localhost should be present and local")
	}
	if !anyMsg(local.Lines, "GNU tar present") || !anyMsg(local.Lines, "readable") {
		t.Fatalf("localhost should be probed via the Local executor: %+v", local.Lines)
	}
	app, ok := hostLines(rep, "app01")
	if !ok || !app.Remote || app.Target == "" {
		t.Fatalf("app01 should be remote with a target: %+v", app)
	}
	if !anyMsg(app.Lines, "not probed") {
		t.Fatalf("offline should not probe a remote host: %+v", app.Lines)
	}
}

// TestCheckFlagsUnreadableSource confirms a missing source path is a hard failure (probed
// locally, no network).
func TestCheckFlagsUnreadableSource(t *testing.T) {
	eng := newCheckEngine(t, []config.DLE{{Host: "localhost", Path: filepath.Join(t.TempDir(), "does-not-exist")}})
	if m, err := eng.archiverFor(config.DefaultDumpType, "localhost"); err != nil || m.Check() != nil {
		t.Skip("GNU tar not available")
	}
	rep := eng.Check(true)
	if rep.Failures == 0 {
		t.Fatalf("an unreadable source must fail the check: %+v", rep)
	}
	local, _ := hostLines(rep, "localhost")
	if !anyMsg(local.Lines, "not readable") {
		t.Fatalf("expected an unreadable-source line: %+v", local.Lines)
	}
}

// TestCheckWarnsOnRelativePaths verifies the cron footgun advisory: a relative workdir
// or state_dir resolves against nb's working directory, so a cron job started elsewhere
// re-fulls; nb check flags both as warnings (not failures).
func TestCheckWarnsOnRelativePaths(t *testing.T) {
	tmp := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": filepath.Join(tmp, "m")}}},
		Sources:  []config.DLE{{Host: "localhost", Path: tmp}},
		Workdir:  "rel-catalog",
		StateDir: "rel-state",
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, "localhost"); err != nil || m.Check() != nil {
		t.Skip("GNU tar not available")
	}
	rep := eng.Check(true)
	if rep.Failures != 0 {
		t.Errorf("relative paths are advisories, not failures: %+v", rep)
	}
	if !anyMsg(rep.Server, `workdir "rel-catalog" is relative`) {
		t.Errorf("expected a relative-workdir advisory: %+v", rep.Server)
	}
	local, _ := hostLines(rep, "localhost")
	if !anyMsg(local.Lines, `state_dir "rel-state" is relative`) {
		t.Errorf("expected a relative-state_dir advisory: %+v", local.Lines)
	}
}
