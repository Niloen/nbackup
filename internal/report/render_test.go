package report

import (
	"strings"
	"testing"
	"time"
)

func TestRenderDigest(t *testing.T) {
	now := time.Date(2026, 6, 24, 2, 0, 0, 0, time.UTC)
	runs := []Run{
		{Command: CommandDump, Outcome: OutcomeSuccess, SlotID: "slot-2026-06-24", Archives: 3, BytesMoved: 1 << 30,
			StartedAt: now.Add(-time.Hour), EndedAt: now.Add(-time.Hour).Add(2 * time.Minute)},
		{Command: CommandSync, Outcome: OutcomeFailure, ExitClass: "sync-error", Error: "target full",
			StartedAt: now.Add(-30 * time.Minute), EndedAt: now.Add(-29 * time.Minute)},
		{Command: CommandDrill, Outcome: OutcomeSuccess, Overdue: 1, NeverDrilled: []string{"db01-postgres"},
			DrillHealth: []DrillHealth{{DLE: "app01-home", OK: false, Class: "pipeline", WasOK: true, Drilled: true}},
			StartedAt:   now, EndedAt: now.Add(time.Minute)},
	}
	var sb strings.Builder
	Render(&sb, runs, now)
	out := sb.String()

	for _, want := range []string{
		"3 run(s)",
		"1 run(s) FAILED",
		"FAILURES",
		"sync-error",
		"target full",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("digest missing %q\n---\n%s", want, out)
		}
	}
	// The run-history digest itself does not carry recovery health — that is rendered
	// from the live ledger by the caller (cli.renderDrillLedger).
	if strings.Contains(out, "RECOVERY HEALTH") {
		t.Errorf("digest should not embed recovery health:\n%s", out)
	}
}

func TestRenderRunRecovery(t *testing.T) {
	r := Run{
		Command: CommandDrill, Outcome: OutcomeFailure, ExitClass: "drill-failures",
		Error: "1 drill failure(s)", Failures: 1, Overdue: 1, NeverDrilled: []string{"db01-postgres"},
		DrillHealth: []DrillHealth{{DLE: "app01-home", OK: false, Class: "pipeline", WasOK: true, Drilled: true}},
		StartedAt:   time.Now(), EndedAt: time.Now().Add(time.Minute),
	}
	var sb strings.Builder
	RenderRun(&sb, r)
	out := sb.String()
	for _, want := range []string{"RECOVERY HEALTH", "DEGRADING", "app01-home", "never drilled: db01-postgres", "1 DLE(s) overdue"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderRun recovery missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderEmpty(t *testing.T) {
	var sb strings.Builder
	Render(&sb, nil, time.Now())
	if !strings.Contains(sb.String(), "No runs recorded") {
		t.Errorf("empty digest = %q", sb.String())
	}
}

func TestRenderRunFailure(t *testing.T) {
	r := Run{
		Command: CommandDump, Outcome: OutcomeFailure, ExitClass: "dump-failed", Error: "tar exited 2",
		StartedAt: time.Now(), EndedAt: time.Now().Add(time.Minute),
	}
	var sb strings.Builder
	RenderRun(&sb, r)
	out := sb.String()
	if !strings.Contains(out, "dump FAILED") || !strings.Contains(out, "tar exited 2") {
		t.Errorf("RenderRun failure = %q", out)
	}
}

func dumpRunFixture() Run {
	return Run{
		Command: CommandDump, Outcome: OutcomeSuccess, SlotID: "slot-2026-06-24",
		StartedAt: time.Date(2026, 6, 24, 2, 0, 0, 0, time.UTC), EndedAt: time.Date(2026, 6, 24, 2, 12, 0, 0, time.UTC),
		DumpStats: []DLEStat{
			{DLE: "app01-home", Level: 0, Orig: 20 << 30, Out: 5 << 30, Files: 1240, Seconds: 724},
			{DLE: "app01-etc", Level: 1, Orig: 120 << 10, Out: 40 << 10, Files: 9, Seconds: 1},
			{DLE: "db01-pg", Level: 0, Orig: 0, Out: 0, Files: 0}, // empty / no timing → dashes
		},
	}
}

func TestRenderDump(t *testing.T) {
	var sb strings.Builder
	RenderDump(&sb, dumpRunFixture())
	out := sb.String()
	for _, want := range []string{
		"DUMP REPORT  slot-2026-06-24",
		"DLE", "ORIG", "OUT", "COMP%", "FILES", "TIME", "RATE",
		"app01-home", "21.47 GB", "1240",
		"app01-etc",
		"FULL: 2 dle(s)", // two level-0 archives (one empty)
		"INCR: 1 dle(s)",
		"db01-pg", // every DLE listed, even the empty one
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump report missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderDumpNoStats(t *testing.T) {
	var sb strings.Builder
	RenderDump(&sb, Run{Command: CommandDump, SlotID: "slot-x"})
	if !strings.Contains(sb.String(), "no per-DLE statistics") {
		t.Errorf("expected a no-stats note, got %q", sb.String())
	}
}

func TestRenderRunDumpIncludesTable(t *testing.T) {
	// A dump notification body carries the full per-DLE report, not just the summary.
	var sb strings.Builder
	RenderRun(&sb, dumpRunFixture())
	out := sb.String()
	if !strings.Contains(out, "dump OK") {
		t.Errorf("RenderRun missing summary header:\n%s", out)
	}
	for _, want := range []string{"COMP%", "app01-home", "FULL:"} {
		if !strings.Contains(out, want) {
			t.Errorf("dump notification body missing per-DLE detail %q\n---\n%s", want, out)
		}
	}
}
