package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleRun(cmd Command, out Outcome) Run {
	return Run{
		Command:   cmd,
		StartedAt: time.Date(2026, 6, 24, 1, 0, 0, 0, time.UTC),
		EndedAt:   time.Date(2026, 6, 24, 1, 5, 0, 0, time.UTC),
		Outcome:   out,
	}
}

func TestAppendAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := []Run{
		sampleRun(CommandDump, OutcomeSuccess),
		sampleRun(CommandSync, OutcomeFailure),
		sampleRun(CommandDrill, OutcomeSuccess),
	}
	for _, r := range want {
		if err := Append(dir, r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Load returned %d runs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Command != want[i].Command || got[i].Outcome != want[i].Outcome {
			t.Errorf("run %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAppendWritesSummary(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, sampleRun(CommandDump, OutcomeSuccess)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := Append(dir, sampleRun(CommandDrill, OutcomeFailure)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, SummaryFile))
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	var latest Run
	if err := json.Unmarshal(data, &latest); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	if latest.Command != CommandDrill || latest.Outcome != OutcomeFailure {
		t.Errorf("summary = %+v, want the latest (drill/failure)", latest)
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	runs, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load on empty dir: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("Load on empty dir = %d runs, want 0", len(runs))
	}
}

func TestLoadSkipsTornTrailingLine(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, sampleRun(CommandDump, OutcomeSuccess)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Simulate a reader catching a mid-append write: a partial trailing JSON line.
	f, err := os.OpenFile(filepath.Join(dir, LogFile), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	if _, err := f.WriteString(`{"command":"sync","outcome":`); err != nil {
		t.Fatalf("write torn line: %v", err)
	}
	f.Close()

	runs, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with torn trailing line: %v", err)
	}
	if len(runs) != 1 || runs[0].Command != CommandDump {
		t.Errorf("Load = %+v, want just the one complete dump record", runs)
	}
}

func TestLoadReportsInteriorCorruption(t *testing.T) {
	dir := t.TempDir()
	// An unparseable line that is NOT the last one is real corruption, not a race.
	bad := "{not json}\n" + `{"command":"dump","outcome":"success"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, LogFile), []byte(bad), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Errorf("Load: expected error on corrupt interior line, got nil")
	}
}

func TestLast(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		if err := Append(dir, sampleRun(CommandDump, OutcomeSuccess)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := Last(dir, 2)
	if err != nil {
		t.Fatalf("Last: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Last(2) = %d runs, want 2", len(got))
	}
	all, err := Last(dir, 0)
	if err != nil {
		t.Fatalf("Last(0): %v", err)
	}
	if len(all) != 5 {
		t.Errorf("Last(0) = %d runs, want all 5", len(all))
	}
}

func TestCompaction(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxLogLines+50; i++ {
		if err := Append(dir, sampleRun(CommandDump, OutcomeSuccess)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	runs, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(runs) != maxLogLines {
		t.Errorf("after overflow, history holds %d runs, want compaction to %d", len(runs), maxLogLines)
	}
}
