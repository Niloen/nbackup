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
