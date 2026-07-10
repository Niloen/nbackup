package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/report"
)

func TestRenderDrillLedger(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	ledger := &drill.Ledger{Records: map[string]drill.Record{
		"app01-home":  {DLE: "app01-home", OK: false, Class: "pipeline", Detail: "gpg: decryption failed", LastDrill: now.Add(-2 * 24 * time.Hour)},
		"app01-etc":   {DLE: "app01-etc", OK: true, LastDrill: now.Add(-40 * 24 * time.Hour)},  // stale (>30d)
		"db01-pgdata": {DLE: "db01-pgdata", OK: true, LastDrill: now.Add(-1 * 24 * time.Hour)}, // healthy
	}}
	if err := ledger.Save(dir); err != nil {
		t.Fatalf("save ledger: %v", err)
	}
	// Configure a DLE that has no ledger record at all → never drilled.
	cfg := &config.Config{Workdir: dir, Sources: config.Sources{
		{Host: "web01", Path: "/srv"},
	}}

	var sb strings.Builder
	renderDrillLedger(&sb, cfg, now)
	out := sb.String()

	for _, want := range []string{
		"DRILL COVERAGE",
		"app01-home",                     // failing
		"pipeline",                       // its class
		"error:  gpg: decryption failed", // the recorded error, not just the class
		"decrypt/decompress",             // remedy text for pipeline class
		"retry:  `nb drill app01-home`",  // the re-drill that clears the warning
		"app01-etc",                      // stale
		"stale",
		"never drilled: web01:/srv", // displayed as host:path, not the internal slug
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ledger render missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "db01-pgdata") {
		t.Errorf("healthy DLE should not appear in coverage warnings:\n%s", out)
	}
}

func TestRenderDrillLedgerAllHealthy(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	ledger := &drill.Ledger{Records: map[string]drill.Record{
		"a": {DLE: "a", OK: true, LastDrill: now.Add(-time.Hour)},
	}}
	if err := ledger.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	var sb strings.Builder
	renderDrillLedger(&sb, &config.Config{Workdir: dir}, now)
	if !strings.Contains(sb.String(), "all 1 drilled DLE(s) passing and current") {
		t.Errorf("expected all-healthy line, got %q", sb.String())
	}
}

// TestReportCommand exercises the full `nb report` wiring through cobra, including
// loadOrDefaultCatalog via --catalog and reading the seeded run history.
func TestReportCommand(t *testing.T) {
	dir := t.TempDir()
	for _, r := range []report.Run{
		{Command: report.CommandDump, Outcome: report.OutcomeSuccess, RunID: "run-2026-06-24.001", BytesMoved: 2048,
			StartedAt: time.Now().Add(-time.Hour), EndedAt: time.Now().Add(-time.Hour)},
		{Command: report.CommandSync, Outcome: report.OutcomeFailure, ExitClass: "sync-error", Error: "offline",
			StartedAt: time.Now(), EndedAt: time.Now()},
	} {
		if err := report.Append(dir, r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	out := captureStdout(t, func() {
		root := NewRootCmd()
		root.SetArgs([]string{"--catalog", dir, "report"})
		if err := root.Execute(); err != nil {
			t.Fatalf("report command: %v", err)
		}
	})
	for _, want := range []string{"2 run(s)", "1 run(s) FAILED", "run-2026-06-24.001", "sync-error", "offline"} {
		if !strings.Contains(out, want) {
			t.Errorf("report output missing %q\n---\n%s", want, out)
		}
	}
}

func TestReportDumpCommand(t *testing.T) {
	dir := t.TempDir()
	older := report.Run{Command: report.CommandDump, Outcome: report.OutcomeSuccess, RunID: "run-2026-06-23.001",
		StartedAt: time.Now().Add(-24 * time.Hour), EndedAt: time.Now().Add(-24 * time.Hour),
		DumpStats: []report.DLEStat{{DLE: "old-dle", Level: 0, Orig: 1 << 20, Out: 1 << 19, Files: 5, Seconds: 2}}}
	latest := report.Run{Command: report.CommandDump, Outcome: report.OutcomeSuccess, RunID: "run-2026-06-24.001",
		StartedAt: time.Now(), EndedAt: time.Now(),
		DumpStats: []report.DLEStat{{DLE: "app01-home", Level: 0, Orig: 20 << 30, Out: 5 << 30, Files: 1240, Seconds: 724}}}
	for _, r := range []report.Run{older, latest} {
		if err := report.Append(dir, r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// Default --dump reports the latest dump.
	out := captureStdout(t, func() {
		root := NewRootCmd()
		root.SetArgs([]string{"--catalog", dir, "report", "--dump"})
		if err := root.Execute(); err != nil {
			t.Fatalf("report --dump: %v", err)
		}
	})
	if !strings.Contains(out, "run-2026-06-24.001") || !strings.Contains(out, "app01-home") {
		t.Errorf("--dump should report the latest dump, got:\n%s", out)
	}
	if strings.Contains(out, "old-dle") {
		t.Errorf("--dump reported a stale dump:\n%s", out)
	}

	// --run selects a specific (older) dump.
	out = captureStdout(t, func() {
		root := NewRootCmd()
		root.SetArgs([]string{"--catalog", dir, "report", "--run", "run-2026-06-23.001"})
		if err := root.Execute(); err != nil {
			t.Fatalf("report --run: %v", err)
		}
	})
	if !strings.Contains(out, "run-2026-06-23.001") || !strings.Contains(out, "old-dle") {
		t.Errorf("--run should report the named dump, got:\n%s", out)
	}

	// An unknown run is a clear error pointing at `nb run <id>`.
	root := NewRootCmd()
	root.SetArgs([]string{"--catalog", dir, "report", "--run", "run-9999-99-99.001"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "nb run run-9999-99-99.001") {
		t.Errorf("unknown --run error = %v, want a pointer to `nb run <id>`", err)
	}
}

// captureStdout redirects os.Stdout for the duration of f and returns what was
// written (the report command renders to os.Stdout directly, like `nb status`).
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		done <- buf.String()
	}()
	f()
	w.Close()
	os.Stdout = orig
	return <-done
}

// TestDrillRunRecordBytesMoved verifies a drill's run record carries the egress it
// actually read (the drilled targets' bytes, skips excluded) — the value the
// report/webui history shows, so a drill row is not a misleading 0 B.
func TestDrillRunRecordBytesMoved(t *testing.T) {
	dr := &engine.DrillReport{
		Targets: []engine.DrillResult{
			{DLE: "a", Class: drill.ClassNone, Bytes: 3_000_000},
			{DLE: "b", Class: drill.ClassSkipped, Bytes: 9_000_000}, // skipped: read nothing
			{DLE: "c", Class: drill.ClassIntegrity, Bytes: 1_000_000},
		},
	}
	rec := drillRunRecord(dr, nil)
	if rec.BytesMoved != 4_000_000 {
		t.Fatalf("BytesMoved = %d, want 4000000 (drilled a+c, not skipped b)", rec.BytesMoved)
	}
}
