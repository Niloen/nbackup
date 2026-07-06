package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/record"
)

func newCheckEngine(t *testing.T, sources []config.DLE) *Engine {
	t.Helper()
	cfg := &config.Config{
		Landing:  config.MediumList{"disk"},
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, "localhost"); err != nil || m.Check() != nil {
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
	if !anyMsg(local.Lines, "archiver ready") || !anyMsg(local.Lines, "source") || !anyMsg(local.Lines, "ready") {
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, "localhost"); err != nil || m.Check() != nil {
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
		Landing:  config.MediumList{"disk"},
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, "localhost"); err != nil || m.Check() != nil {
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

// TestCheckMissingCompressorIsOneLineWithRemedy is the regression for the
// triple-redundant zstd failure line: the missing-binary case must state the
// problem once and append the remedy, not nest compress.Check's wrapped
// LookPath error ("compression zstd: scheme zstd needs zstd on PATH: exec: zstd:
// executable file not found in $PATH").
func TestCheckMissingCompressorIsOneLineWithRemedy(t *testing.T) {
	cfg := &config.Config{
		Landing:  config.MediumList{"disk"},
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "zstd"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // deterministically no zstd, wherever this runs

	rep := &CheckReport{}
	(&checker{cfg: eng.cfg, tc: eng.tc, dep: eng.dep}).checkServer(rep)
	var line string
	for _, l := range rep.Server {
		if strings.Contains(l.Msg, `compression "zstd"`) {
			line = l.Msg
		}
	}
	want := `compression "zstd": binary not found on PATH (install zstd, or set compress.scheme: gzip or none)`
	if line != want {
		t.Errorf("compression line = %q, want %q", line, want)
	}
	if rep.Failures == 0 {
		t.Errorf("missing compressor must stay a hard failure: %+v", rep)
	}
}

// TestCheckStalenessAlwaysOnDerivesFromCycle pins the zero-config exit-code path: the
// staleness window derives from the dump cycle (no top-level config key), a DLE
// older than one cycle is a hard FAILURE, a never-backed-up DLE is only a WARNING
// (a fresh install must not go red before its first dump), and a DLE within the
// cycle is not reported at all.
func TestCheckStalenessAlwaysOnDerivesFromCycle(t *testing.T) {
	eng := newCheckEngine(t, []config.DLE{
		{Host: "localhost", Path: "/stale"},
		{Host: "localhost", Path: "/never"},
		{Host: "localhost", Path: "/fresh"},
	})
	eng.cfg.Cycle = "1d"

	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now().Add(-1 * time.Hour)
	staleArch := record.Archive{Run: record.IDFromTime(old), DLE: config.DLE{Host: "localhost", Path: "/stale"}.Name(), Level: 0, Compressed: 10, CreatedAt: old}
	freshArch := record.Archive{Run: record.IDFromTime(fresh), DLE: config.DLE{Host: "localhost", Path: "/fresh"}.Name(), Level: 0, Compressed: 10, CreatedAt: fresh}
	pos := archiveio.ArchivePos{Parts: []archiveio.FilePos{{Label: "disk", Pos: 1}}, Commit: archiveio.FilePos{Label: "disk", Pos: 2}}
	if err := eng.cat.AddArchive(staleArch, "disk", pos); err != nil {
		t.Fatal(err)
	}
	if err := eng.cat.AddArchive(freshArch, "disk", pos); err != nil {
		t.Fatal(err)
	}

	rep := &CheckReport{}
	(&checker{cfg: eng.cfg, tc: eng.tc, dep: eng.dep, cat: eng.cat}).checkStaleness(rep)
	if rep.Failures != 1 {
		t.Fatalf("checkStaleness Failures = %d, want 1 (the overdue DLE): %+v", rep.Failures, rep.Server)
	}
	if rep.Warnings != 1 {
		t.Fatalf("checkStaleness Warnings = %d, want 1 (the never-backed-up DLE): %+v", rep.Warnings, rep.Server)
	}
	if !anyMsg(rep.Server, "localhost:/stale") || !anyMsg(rep.Server, "last backed up") || !anyMsg(rep.Server, "cycle") {
		t.Errorf("expected a last-backed-up line mentioning the cycle for the stale DLE: %+v", rep.Server)
	}
	if !anyMsg(rep.Server, "localhost:/never") || !anyMsg(rep.Server, "never been backed up") {
		t.Errorf("expected a never-backed-up line: %+v", rep.Server)
	}
	if anyMsg(rep.Server, "localhost:/fresh") {
		t.Errorf("the freshly backed up DLE should not be reported: %+v", rep.Server)
	}
}

// TestCheckStalenessAllCurrent pins the all-clear line, which must always run
// (there is no way to disable the check).
func TestCheckStalenessAllCurrent(t *testing.T) {
	eng := newCheckEngine(t, []config.DLE{{Host: "localhost", Path: "/fresh"}})
	fresh := time.Now().Add(-time.Hour)
	arch := record.Archive{Run: record.IDFromTime(fresh), DLE: config.DLE{Host: "localhost", Path: "/fresh"}.Name(), Level: 0, Compressed: 10, CreatedAt: fresh}
	pos := archiveio.ArchivePos{Parts: []archiveio.FilePos{{Label: "disk", Pos: 1}}, Commit: archiveio.FilePos{Label: "disk", Pos: 2}}
	if err := eng.cat.AddArchive(arch, "disk", pos); err != nil {
		t.Fatal(err)
	}

	rep := &CheckReport{}
	(&checker{cfg: eng.cfg, tc: eng.tc, dep: eng.dep, cat: eng.cat}).checkStaleness(rep)
	if rep.Failures != 0 || rep.Warnings != 0 {
		t.Fatalf("checkStaleness = %+v, want no failures/warnings", rep)
	}
	if !anyMsg(rep.Server, "one cycle") {
		t.Errorf("expected an all-clear line mentioning the cycle: %+v", rep.Server)
	}
}
