package report

import (
	"strings"
	"testing"
	"time"
)

func TestRenderDigest(t *testing.T) {
	now := time.Date(2026, 6, 24, 2, 0, 0, 0, time.UTC)
	runs := []Run{
		{Command: CommandDump, Outcome: OutcomeSuccess, RunID: "run-2026-06-24.001", Archives: 3, BytesMoved: 1 << 30,
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

// TestRenderPruneSummarySaysArchives pins prune's summary unit: prune reclaims
// archives (a DLE's image within a run), not runs — 5 pruned archives may come
// from 3 runs, so "run(s) pruned" misstated what happened.
func TestRenderPruneSummarySaysArchives(t *testing.T) {
	r := Run{
		Command: CommandPrune, Outcome: OutcomeSuccess, ArchivesPruned: 5, BytesMoved: 1 << 20,
		StartedAt: time.Now(), EndedAt: time.Now().Add(time.Minute),
	}
	var sb strings.Builder
	Render(&sb, []Run{r}, time.Now())
	out := sb.String()
	if !strings.Contains(out, "5 archive(s) pruned") {
		t.Errorf("prune summary missing %q:\n%s", "5 archive(s) pruned", out)
	}
	if strings.Contains(out, "run(s) pruned") {
		t.Errorf("prune summary still counts runs:\n%s", out)
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

// TestRenderWarnedRun: a run that succeeded with warnings (e.g. a tripped landing)
// must say WARNING — in the per-run notification body, the dump report, and the
// digest — and carry each warning line with its repair.
func TestRenderWarnedRun(t *testing.T) {
	warning := "landing c2 tripped mid-run: upload failed — repair: nb sync --run run-2026-07-08.010001 --to c2"
	r := dumpRunFixture()
	r.Warnings = []string{warning}

	if r.Status() != "WARNING" || !r.Warned() || r.Failed() {
		t.Fatalf("Status=%q Warned=%v Failed=%v; want WARNING/true/false", r.Status(), r.Warned(), r.Failed())
	}

	var sb strings.Builder
	RenderRun(&sb, r)
	out := sb.String()
	if !strings.Contains(out, "dump WARNING") || !strings.Contains(out, "WARNING: "+warning) {
		t.Errorf("RenderRun warned = %q, want WARNING status + warning line", out)
	}

	sb.Reset()
	RenderDump(&sb, r)
	out = sb.String()
	if !strings.Contains(out, "WARNING: "+warning) || !strings.Contains(out, "1 WARNING(s)") {
		t.Errorf("RenderDump warned = %q, want warning line + headline count", out)
	}

	sb.Reset()
	Render(&sb, []Run{r}, time.Now())
	out = sb.String()
	for _, want := range []string{"1 run(s) with WARNINGS", "WARNING", warning} {
		if !strings.Contains(out, want) {
			t.Errorf("digest missing %q in:\n%s", want, out)
		}
	}
	// A failed run's warnings must not soften it to WARNING.
	r.Outcome = OutcomeFailure
	if r.Status() != "FAILED" || r.Warned() {
		t.Errorf("failed run with warnings: Status=%q Warned=%v; want FAILED/false", r.Status(), r.Warned())
	}
}

func dumpRunFixture() Run {
	return Run{
		Command: CommandDump, Outcome: OutcomeSuccess, RunID: "run-2026-06-24.001",
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
		"DUMP REPORT  run-2026-06-24.001",
		"3 DLE(s) dumped OK",                  // headline
		"21.47 GB -> 5.37 GB (25%)",           // headline roll-up
		"12m00s elapsed",                      // headline wall clock
		"STATISTICS", "Total", "Full", "Incr", // stats grid
		"DLEs dumped", "Original size", "Dump time (sum)", "Run time (wall)",
		"DLE", "ORIG", "OUT", "COMP%", "FILES", "TIME", "RATE",
		"app01-home", "21.47 GB", "1240",
		"app01-etc",
		"db01-pg", // every DLE listed, even the empty one
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump report missing %q\n---\n%s", want, out)
		}
	}
}

// TestRenderDumpPromotions checks a promoted full is marked in the per-DLE table
// (LVL 0*) and explained in the PROMOTED FULLS note with its unwrapped reason —
// and that a run without promotions renders no such note.
func TestRenderDumpPromotions(t *testing.T) {
	r := dumpRunFixture()
	r.DumpStats[0].Promoted = true
	r.DumpStats[0].Reason = "promoted full (due in 2d; ~40 GB of fulls crowd the next 2d, over the ~12 GB/run balanced level)"
	var sb strings.Builder
	RenderDump(&sb, r)
	out := sb.String()
	for _, want := range []string{
		"0*", // the table marker
		"PROMOTED FULLS (*) — 1 full(s), 5.37 GB pulled forward to level the cycle",
		"app01-home — due in 2d; ~40 GB of fulls crowd the next 2d",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump report missing %q\n---\n%s", want, out)
		}
	}

	sb.Reset()
	RenderDump(&sb, dumpRunFixture())
	if strings.Contains(sb.String(), "PROMOTED") {
		t.Errorf("a run without promotions must render no promotions note\n---\n%s", sb.String())
	}
}

func TestHeadlineFailed(t *testing.T) {
	r := dumpRunFixture()
	r.Outcome, r.ExitClass, r.Error = OutcomeFailure, "dump-failed", "tar exited 2"
	got := headline(r)
	for _, want := range []string{"run FAILED [dump-failed]", "3 DLE(s) dumped", "elapsed"} {
		if !strings.Contains(got, want) {
			t.Errorf("failed headline missing %q: %s", want, got)
		}
	}
}

func TestRenderDumpNoStats(t *testing.T) {
	var sb strings.Builder
	RenderDump(&sb, Run{Command: CommandDump, RunID: "run-x"})
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
	for _, want := range []string{"COMP%", "app01-home", "STATISTICS"} {
		if !strings.Contains(out, want) {
			t.Errorf("dump notification body missing per-DLE detail %q\n---\n%s", want, out)
		}
	}
}

// TestRenderDumpWriteStats checks the write side of the dump report: the per-landing
// busy-time rows (time with utilization, rate over busy time — never bytes over
// wall clock) and the per-DLE flush columns, shown only when some DLE drained.
func TestRenderDumpWriteStats(t *testing.T) {
	r := dumpRunFixture()
	r.LandingStats = []LandingStat{{Landing: "s3", Bytes: 5 << 30, BusySeconds: 300, WallSeconds: 720}}
	r.DumpStats[0].FlushBytes = 5 << 30 // app01-home drained; the others were direct
	r.DumpStats[0].FlushSeconds = 300
	var sb strings.Builder
	RenderDump(&sb, r)
	out := sb.String()
	for _, want := range []string{
		"Write time (s3)", "5m00s (42% busy)",
		"Avg write rate (s3)", "17.90 MB/s", // 5 GiB over 300 busy seconds
		"FLUSH", "FL-RATE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump report missing %q\n---\n%s", want, out)
		}
	}

	// Without any drained DLE the flush columns stay out of the table.
	var direct strings.Builder
	RenderDump(&direct, dumpRunFixture())
	if strings.Contains(direct.String(), "FL-RATE") {
		t.Errorf("all-direct run must not render flush columns:\n%s", direct.String())
	}
}

// TestRenderDumpGroupsPartition checks the dump table's path arrangement: a
// partitioned source's DLEs render under one host:base header row carrying the
// group's totals, members get short base-relative labels with tree glyphs, the
// remainder is labeled "(the rest)", and unrelated DLEs stay flat.
func TestRenderDumpGroupsPartition(t *testing.T) {
	r := Run{
		Command: CommandDump, Outcome: OutcomeSuccess, RunID: "run-2026-07-09.020000",
		StartedAt: time.Date(2026, 7, 9, 2, 0, 0, 0, time.UTC), EndedAt: time.Date(2026, 7, 9, 2, 12, 0, 0, time.UTC),
		DumpStats: []DLEStat{
			{DLE: "app01-data-projects-alpha", Host: "app01", Path: "/data/projects/alpha", Level: 0, Orig: 10 << 30, Out: 5 << 30, Files: 100, Seconds: 100},
			{DLE: "app01-data", Host: "app01", Path: "/data", Rest: true, Level: 1, Orig: 2 << 30, Out: 1 << 30, Files: 10, Seconds: 20},
			{DLE: "app01-data-projects-beta", Host: "app01", Path: "/data/projects/beta", Level: 1, Orig: 4 << 30, Out: 2 << 30, Files: 40, Seconds: 40},
			{DLE: "web01-home", Host: "web01", Path: "/home", Level: 1, Orig: 1 << 30, Out: 512 << 20, Files: 5, Seconds: 10},
		},
	}
	var sb strings.Builder
	RenderDump(&sb, r)
	out := sb.String()
	for _, want := range []string{
		"app01:/data · 3 DLEs", // group header with member count
		"17.18 GB",             // header ORIG subtotal (10+2+4 GiB)
		"├─ projects/alpha",    // relative labels, long prefix written once
		"├─ projects/beta",
		"└─ (the rest)", // the remainder last, named by position
		"web01:/home",   // unrelated DLE stays flat
	} {
		if !strings.Contains(out, want) {
			t.Errorf("grouped dump table missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "app01:/data/projects/alpha") {
		t.Errorf("grouped member must not repeat its full identity\n---\n%s", out)
	}
}

// TestRenderRecoveryFoldsAndCaps checks the one-line recovery lists stay one
// line: path siblings fold to their group with a count, and a long list caps
// with a trailing "…and N more".
func TestRenderRecoveryFoldsAndCaps(t *testing.T) {
	r := Run{
		Command: CommandDrill, Outcome: OutcomeFailure, ExitClass: "drill-failures",
		StartedAt: time.Now(), EndedAt: time.Now().Add(time.Minute),
		DrillHealth: []DrillHealth{
			{DLE: "app01-data-a", Display: "app01:/data/a", OK: false, WasOK: true, Drilled: true},
			{DLE: "app01-data-b", Display: "app01:/data/b", OK: false, WasOK: true, Drilled: true},
			{DLE: "web01-home", Display: "web01:/home", OK: false, WasOK: true, Drilled: true},
		},
		NeverDrilled: []string{"h:/n1", "h2:/n2", "h3:/n3", "h4:/n4", "h5:/n5", "h6:/n6", "h7:/n7", "h8:/n8"},
	}
	var sb strings.Builder
	RenderRun(&sb, r)
	out := sb.String()
	for _, want := range []string{
		"app01:/data (2 DLEs)", // siblings folded to their group
		"web01:/home",          // a lone DLE stays itself
		"…and 3 more",          // 8 never-drilled capped at listCap
	} {
		if !strings.Contains(out, want) {
			t.Errorf("recovery note missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "app01:/data/a") {
		t.Errorf("folded sibling must not be listed individually\n---\n%s", out)
	}
}
