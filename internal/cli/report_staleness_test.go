package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/record"
)

// seedArchive writes one committed archive straight into the catalog cache (no
// medium involved — the same catalog-only shortcut the engine tests use), so a
// staleness test can control CreatedAt precisely.
func seedArchive(t *testing.T, dir string, dle config.DLE, createdAt time.Time) {
	t.Helper()
	cat, err := catalog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	arch := record.Archive{Run: record.IDFromTime(createdAt), DLE: dle.Name(), Host: dle.Host, Path: dle.Path,
		Level: 0, Compressed: 10, CreatedAt: createdAt}
	pos := archiveio.ArchivePos{Parts: []archiveio.FilePos{{Label: "disk", Pos: 1}}, Commit: archiveio.FilePos{Label: "disk", Pos: 2}}
	if err := cat.AddArchive(arch, "disk", pos); err != nil {
		t.Fatal(err)
	}
}

// TestRenderStalenessDisabledByDefault pins the opt-in default: with no
// `staleness.window` configured, the section renders nothing even though a
// configured DLE has never been backed up.
func TestRenderStalenessDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Workdir: dir, Sources: config.Sources{{Host: "web01", Path: "/srv"}}}
	var sb strings.Builder
	renderStaleness(&sb, cfg, time.Now())
	if sb.Len() != 0 {
		t.Errorf("renderStaleness with no window configured should print nothing, got %q", sb.String())
	}
}

// TestRenderStaleness exercises the enabled case: one DLE within the window (not
// reported), one whose last backup predates it, and one never backed up at all.
func TestRenderStaleness(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	seedArchive(t, dir, config.DLE{Host: "app01", Path: "/home"}, now.Add(-1*time.Hour))
	seedArchive(t, dir, config.DLE{Host: "db01", Path: "/data"}, now.Add(-10*24*time.Hour))

	cfg := &config.Config{Workdir: dir, Staleness: config.StalenessConfig{Window: "3d"}, Sources: config.Sources{
		{Host: "app01", Path: "/home"},
		{Host: "db01", Path: "/data"},
		{Host: "web01", Path: "/srv"},
	}}

	var sb strings.Builder
	renderStaleness(&sb, cfg, now)
	out := sb.String()
	for _, want := range []string{"STALE DLEs", "db01:/data", "never", "web01:/srv"} {
		if !strings.Contains(out, want) {
			t.Errorf("staleness render missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "app01:/home") {
		t.Errorf("app01:/home was backed up within the window, should not be reported:\n%s", out)
	}
}

// TestRenderStalenessAllCurrent pins the all-clear line.
func TestRenderStalenessAllCurrent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	seedArchive(t, dir, config.DLE{Host: "app01", Path: "/home"}, now.Add(-time.Hour))
	cfg := &config.Config{Workdir: dir, Staleness: config.StalenessConfig{Window: "3d"}, Sources: config.Sources{
		{Host: "app01", Path: "/home"},
	}}
	var sb strings.Builder
	renderStaleness(&sb, cfg, now)
	if !strings.Contains(sb.String(), "all configured DLE(s) backed up within") {
		t.Errorf("expected the all-current line, got %q", sb.String())
	}
}

// TestReportJSONOmitsStaleWhenDisabled verifies `nb report --json` (via --catalog,
// which yields a bare config with no staleness.window) omits the stale key
// entirely (omitempty) rather than an empty list — mirroring how the text render
// prints nothing when the alert is unset.
func TestReportJSONOmitsStaleWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	seedArchive(t, dir, config.DLE{Host: "web01", Path: "/srv"}, now.Add(-10*24*time.Hour))

	out := captureStdout(t, func() {
		root := NewRootCmd()
		root.SetArgs([]string{"--catalog", dir, "report", "--json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("report --json: %v", err)
		}
	})
	if strings.Contains(out, `"stale"`) {
		t.Errorf("stale key should be omitted when staleness is unset:\n%s", out)
	}
}

// TestStaleDLEsHelper exercises the cli staleDLEs helper directly (the shared
// computation behind both the text render and --json), including its Display
// fallback for a DLE with no catalog entry at all.
func TestStaleDLEsHelper(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	seedArchive(t, dir, config.DLE{Host: "db01", Path: "/data"}, now.Add(-10*24*time.Hour))

	cfg := &config.Config{Workdir: dir, Staleness: config.StalenessConfig{Window: "3d"}, Sources: config.Sources{
		{Host: "db01", Path: "/data"},
		{Host: "web01", Path: "/srv"},
	}}
	stale, window := staleDLEs(cfg, now)
	if window != 3*24*time.Hour {
		t.Fatalf("window = %v, want 72h", window)
	}
	if len(stale) != 2 {
		t.Fatalf("staleDLEs = %+v, want 2 entries", stale)
	}
	byDisplay := map[string]catalog.StaleDLE{}
	for _, s := range stale {
		byDisplay[s.Display] = s
	}
	if _, ok := byDisplay["db01:/data"]; !ok {
		t.Errorf("expected db01:/data (display resolved from the never-in-catalog slug map): %+v", stale)
	}
	if s, ok := byDisplay["web01:/srv"]; !ok || !s.LastBackup.IsZero() {
		t.Errorf("expected web01:/srv never backed up: %+v", stale)
	}
}
