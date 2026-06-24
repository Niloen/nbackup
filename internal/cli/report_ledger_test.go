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
	"github.com/Niloen/nbackup/internal/report"
)

func TestRenderDrillLedger(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	ledger := &drill.Ledger{Records: map[string]drill.Record{
		"app01-home":  {DLE: "app01-home", OK: false, Class: "pipeline", LastDrill: now.Add(-2 * 24 * time.Hour)},
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
		"app01-home",         // failing
		"pipeline",           // its class
		"decrypt/decompress", // remedy text for pipeline class
		"app01-etc",          // stale
		"stale",
		"never drilled: web01-srv",
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
// loadRO via --catalog and reading the seeded run history.
func TestReportCommand(t *testing.T) {
	dir := t.TempDir()
	for _, r := range []report.Run{
		{Command: report.CommandDump, Outcome: report.OutcomeSuccess, SlotID: "slot-2026-06-24", BytesMoved: 2048,
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
	for _, want := range []string{"2 run(s)", "1 run(s) FAILED", "slot-2026-06-24", "sync-error", "offline"} {
		if !strings.Contains(out, want) {
			t.Errorf("report output missing %q\n---\n%s", want, out)
		}
	}
}

func TestReportDumpCommand(t *testing.T) {
	dir := t.TempDir()
	older := report.Run{Command: report.CommandDump, Outcome: report.OutcomeSuccess, SlotID: "slot-2026-06-23",
		StartedAt: time.Now().Add(-24 * time.Hour), EndedAt: time.Now().Add(-24 * time.Hour),
		DumpStats: []report.DLEStat{{DLE: "old-dle", Level: 0, Orig: 1 << 20, Out: 1 << 19, Files: 5, Seconds: 2}}}
	latest := report.Run{Command: report.CommandDump, Outcome: report.OutcomeSuccess, SlotID: "slot-2026-06-24",
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
	if !strings.Contains(out, "slot-2026-06-24") || !strings.Contains(out, "app01-home") {
		t.Errorf("--dump should report the latest dump, got:\n%s", out)
	}
	if strings.Contains(out, "old-dle") {
		t.Errorf("--dump reported a stale dump:\n%s", out)
	}

	// --slot selects a specific (older) dump.
	out = captureStdout(t, func() {
		root := NewRootCmd()
		root.SetArgs([]string{"--catalog", dir, "report", "--slot", "slot-2026-06-23"})
		if err := root.Execute(); err != nil {
			t.Fatalf("report --slot: %v", err)
		}
	})
	if !strings.Contains(out, "slot-2026-06-23") || !strings.Contains(out, "old-dle") {
		t.Errorf("--slot should report the named dump, got:\n%s", out)
	}

	// An unknown slot is a clear error pointing at `nb slot show`.
	root := NewRootCmd()
	root.SetArgs([]string{"--catalog", dir, "report", "--slot", "slot-9999-99-99"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "nb slot show") {
		t.Errorf("unknown --slot error = %v, want a pointer to `nb slot show`", err)
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
