package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/conductor"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
)

// TestRunRestoreEndToEnd exercises the full engine over the disk store:
// full backup, incremental with a deletion, then a chain restore that must match
// the live tree.
func TestRunRestoreEndToEnd(t *testing.T) {
	src := t.TempDir()
	catalogDir := t.TempDir()

	write(t, filepath.Join(src, "keep.txt"), "v1")
	write(t, filepath.Join(src, "gone.txt"), "temp")

	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": catalogDir}}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(), // catalog state lives separately from the storage medium
	}
	cfg.Compress.Scheme = "none" // exercise the pipeline without depending on a compressor binary

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	day1 := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(context.Background(), day1, nil); err != nil {
		t.Fatalf("day1 run: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)
	write(t, filepath.Join(src, "keep.txt"), "v2")
	if err := os.Remove(filepath.Join(src, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	day2 := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	s2, err := eng.Run(context.Background(), day2, nil)
	if err != nil {
		t.Fatalf("day2 run: %v", err)
	}
	if got := s2.Archives[0].Level; got != 1 {
		t.Fatalf("day2 should be L1, got L%d", got)
	}

	dest := t.TempDir()
	name := config.DLE{Host: "localhost", Path: src}.Name()
	if err := eng.Restore(s2.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertContent(t, filepath.Join(dest, "keep.txt"), "v2")
	if _, err := os.Stat(filepath.Join(dest, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt should be deleted after restore, stat err = %v", err)
	}
}

// TestRunCanceledMarksStatusCanceled exercises the cancel path: a run whose context is
// already canceled must abort with conductor.ErrCanceled (wrapping context.Canceled),
// seal nothing, and leave the run-status file at a terminal "canceled" phase rather than
// frozen at "running" — the bug where canceling a dump left status showing it running.
func TestRunCanceledMarksStatusCanceled(t *testing.T) {
	src := t.TempDir()
	catalogDir := t.TempDir()
	write(t, filepath.Join(src, "keep.txt"), "v1")

	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": catalogDir}}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the dump starts: the prelude check must abort the run

	_, runErr := eng.Run(ctx, time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if !errors.Is(runErr, conductor.ErrCanceled) {
		t.Fatalf("run error = %v; want conductor.ErrCanceled", runErr)
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("run error = %v; want it to wrap context.Canceled", runErr)
	}

	snap, err := progress.Load(cfg.WorkdirPath())
	if err != nil {
		t.Fatalf("load run status: %v", err)
	}
	if snap.Phase != progress.PhaseCanceled {
		t.Fatalf("status phase = %q; want %q", snap.Phase, progress.PhaseCanceled)
	}
	if !snap.Phase.Terminal() {
		t.Fatalf("canceled phase must be terminal so `nb status` stops showing the run as live")
	}
}

// TestRepeatedLevelRestore exercises the bump scheme's defining behavior: a DLE
// sits at level 1 across several runs (re-dumping everything since the full) rather
// than climbing a level per run, and a point-in-time restore replaying the full
// plus the superseding level-1 dumps reconstructs the correct state — including a
// deletion that an earlier incremental made. bump_percent is pinned high so no
// saving ever justifies a climb, isolating the repeat-level path.
func TestRepeatedLevelRestore(t *testing.T) {
	src := t.TempDir()
	catalogDir := t.TempDir()

	write(t, filepath.Join(src, "keep.txt"), "v1")
	write(t, filepath.Join(src, "gone.txt"), "temp")

	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": catalogDir}}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
		BumpPct:  100, // a saving can never reach 100% of the full, so never bump
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	day1 := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(context.Background(), day1, nil); err != nil {
		t.Fatalf("day1 run: %v", err)
	}

	// Day 2: change keep, delete gone, add new — a first level-1 dump.
	time.Sleep(1100 * time.Millisecond)
	write(t, filepath.Join(src, "keep.txt"), "v2")
	if err := os.Remove(filepath.Join(src, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "new.txt"), "n1")
	day2 := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	if s, err := eng.Run(context.Background(), day2, nil); err != nil {
		t.Fatalf("day2 run: %v", err)
	} else if got := s.Archives[0].Level; got != 1 {
		t.Fatalf("day2 should be L1, got L%d", got)
	}

	// Day 3: change keep again, add new2 — must repeat level 1, not climb to L2.
	time.Sleep(1100 * time.Millisecond)
	write(t, filepath.Join(src, "keep.txt"), "v3")
	write(t, filepath.Join(src, "new2.txt"), "n2")
	day3 := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	s3, err := eng.Run(context.Background(), day3, nil)
	if err != nil {
		t.Fatalf("day3 run: %v", err)
	}
	if got := s3.Archives[0].Level; got != 1 {
		t.Fatalf("day3 should repeat L1, got L%d", got)
	}

	// Restore as of day 3: keep=v3, gone still deleted, both new files present.
	dest := t.TempDir()
	name := config.DLE{Host: "localhost", Path: src}.Name()
	if err := eng.Restore(s3.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertContent(t, filepath.Join(dest, "keep.txt"), "v3")
	assertContent(t, filepath.Join(dest, "new.txt"), "n1")
	assertContent(t, filepath.Join(dest, "new2.txt"), "n2")
	if _, err := os.Stat(filepath.Join(dest, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt should stay deleted after restore, stat err = %v", err)
	}
}

// TestResetForcesFullNextRun verifies `nb reset` as a planner directive: after a full it
// schedules the DLE for a level 0 on the next run (touching no incremental state), the
// forced run dumps a full, and the directive is consumed so the run after that returns to
// the ordinary incremental schedule.
func TestResetForcesFullNextRun(t *testing.T) {
	src := t.TempDir()
	catalogDir := t.TempDir()
	write(t, filepath.Join(src, "keep.txt"), "v1")

	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": catalogDir}}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	day1 := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(context.Background(), day1, nil); err != nil {
		t.Fatalf("day1 run: %v", err)
	}

	// Without a reset, day2 would be an incremental (a usable base exists).
	day2 := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	if got := eng.Plan(day2).Items[0].Level; got != 1 {
		t.Fatalf("precondition: day2 should plan L1, got L%d", got)
	}

	id, err := eng.ForceFull(config.DLE{Host: "localhost", Path: src}.ID())
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if id == "" {
		t.Fatal("reset should return the DLE identity")
	}

	// The directive forces the next plan to a full, labeled as such.
	plan := eng.Plan(day2)
	if got := plan.Items[0].Level; got != 0 {
		t.Fatalf("after reset, day2 should plan L0, got L%d", got)
	}
	if !strings.Contains(plan.Items[0].Reason, "forced full") {
		t.Fatalf("expected a forced-full reason, got %q", plan.Items[0].Reason)
	}

	// A real run honors it: day2 dumps a full, not an incremental.
	time.Sleep(1100 * time.Millisecond)
	s2, err := eng.Run(context.Background(), day2, nil)
	if err != nil {
		t.Fatalf("day2 run: %v", err)
	}
	if got := s2.Archives[0].Level; got != 0 {
		t.Fatalf("after reset, day2 run should be L0, got L%d", got)
	}

	// The directive is consumed: the next plan is back on the ordinary schedule (an
	// incremental on the fresh full), not stuck forcing fulls forever.
	day3 := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	if got := eng.Plan(day3).Items[0].Level; got != 1 {
		t.Fatalf("after the forced full ran, day3 should plan L1 again, got L%d", got)
	}

	// Forcing an unknown DLE is an error (you can only reset something configured).
	if _, err := eng.ForceFull("nope:/x"); err == nil {
		t.Fatal("reset of an unconfigured DLE should error")
	}
}

// TestValidatePlan checks that a plan preview surfaces the config problems the
// size estimates would otherwise swallow: an unknown archiver is fatal, a
// missing source path warns but does not fail, and a clean config is silent.
func TestValidatePlan(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "v1")

	base := func() *config.Config {
		c := &config.Config{
			Landing:  "disk",
			Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
			Sources:  []config.DLE{{Host: "localhost", Path: src}},
			Workdir:  t.TempDir(),
			StateDir: t.TempDir(),
		}
		c.Compress.Scheme = "none"
		return c
	}

	eng, err := New(base())
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	if w, err := eng.ValidatePlan(); err != nil || len(w) != 0 {
		t.Fatalf("clean config: want no warnings/err, got warnings=%v err=%v", w, err)
	}

	// A source path that does not exist is a warning, not a hard failure — it may
	// be an unmounted volume the real run will mount.
	missing := base()
	missing.Sources = []config.DLE{{Host: "localhost", Path: filepath.Join(src, "does-not-exist")}}
	eng, err = New(missing)
	if err != nil {
		t.Fatal(err)
	}
	w, err := eng.ValidatePlan()
	if err != nil {
		t.Fatalf("missing source path should warn, not error: %v", err)
	}
	if len(w) != 1 || !strings.Contains(w[0], "missing or unreadable") {
		t.Fatalf("expected one missing-path warning, got %v", w)
	}

	// An unknown archiver is an unrunnable config: fail the preview.
	badArchiver := base()
	badArchiver.DumpTypes = map[string]config.DumpType{config.DefaultDumpType: {Archiver: "rsync"}}
	eng, err = New(badArchiver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ValidatePlan(); err == nil || !strings.Contains(err.Error(), "unknown archiver") {
		t.Fatalf("unknown archiver should fail validation, got err=%v", err)
	}
}

// TestParallelWorkers runs several DLEs with workers > 1, exercising concurrent
// writes into one slot, and verifies every archive is present and restorable.
func TestParallelWorkers(t *testing.T) {
	catalogDir := t.TempDir()
	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": catalogDir}}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none" // no compressor-binary dependency in tests
	cfg.Parallelism.Workers = 3

	names := []string{"alpha", "bravo", "charlie", "delta"}
	for _, n := range names {
		dir := t.TempDir()
		write(t, filepath.Join(dir, n+".txt"), "content-"+n)
		cfg.Sources = append(cfg.Sources, config.DLE{Host: "localhost", Path: dir})
	}

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("parallel run: %v", err)
	}
	if len(s.Archives) != len(cfg.Sources) {
		t.Fatalf("expected %d archives, got %d", len(cfg.Sources), len(s.Archives))
	}

	// Each DLE restores to its original content.
	for i, d := range cfg.Sources {
		dest := t.TempDir()
		if err := eng.Restore(s.ID, d.Name(), dest, false, nil); err != nil {
			t.Fatalf("restore %s: %v", d.Name(), err)
		}
		assertContent(t, filepath.Join(dest, names[i]+".txt"), "content-"+names[i])
	}
}

// TestPlanWithProgress verifies the estimate phase reports progress: the sink sees
// every DLE start and finish and a terminal snapshot, and the plan still estimates
// each source's size (the parallel estimate produces the same result as serial).
func TestPlanWithProgress(t *testing.T) {
	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	cfg.Parallelism.Workers = 3
	names := []string{"alpha", "bravo", "charlie", "delta"}
	for _, n := range names {
		dir := t.TempDir()
		write(t, filepath.Join(dir, n+".txt"), "content-"+n)
		cfg.Sources = append(cfg.Sources, config.DLE{Host: "localhost", Path: dir})
	}

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	var mu sync.Mutex
	var terminal bool
	started, finished := map[string]bool{}, map[string]bool{}
	sink := func(s progress.Snapshot, _ bool) {
		mu.Lock()
		defer mu.Unlock()
		if s.Phase.Terminal() {
			terminal = true
		}
		for _, d := range s.DLEs {
			switch d.State {
			case progress.StateDumping:
				started[d.Name] = true
			case progress.StateDone:
				finished[d.Name] = true
			}
		}
	}

	plan := eng.PlanWithProgress(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), sink)

	if !terminal {
		t.Error("sink never saw a terminal snapshot")
	}
	for _, d := range cfg.Sources {
		if !started[d.ID()] || !finished[d.ID()] {
			t.Errorf("DLE %s: started=%v finished=%v, want both", d.ID(), started[d.ID()], finished[d.ID()])
		}
	}
	if len(plan.Items) != len(cfg.Sources) {
		t.Fatalf("expected %d planned items, got %d", len(cfg.Sources), len(plan.Items))
	}
	for _, it := range plan.Items {
		if it.EstBytes <= 0 {
			t.Errorf("DLE %s estimated %d bytes, want > 0", it.Name, it.EstBytes)
		}
	}
}

// TestRunStatusSpansEstimatePhase verifies `nb status` sees the whole dump cycle:
// the run-status file is written during the estimate prelude (phase "estimating")
// and — crucially — never reads terminal there, so a `nb status --watch` does not
// stop before the dump it is waiting for begins. The file ends terminal only once
// the slot is sealed.
func TestRunStatusSpansEstimatePhase(t *testing.T) {
	workdir := t.TempDir()
	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Workdir:  workdir,
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	cfg.Parallelism.Workers = 2
	for _, n := range []string{"alpha", "bravo"} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, n+".txt"), "content-"+n)
		cfg.Sources = append(cfg.Sources, config.DLE{Host: "localhost", Path: dir})
	}

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	// The estimate sink fires for every estimate-phase snapshot. The status file is
	// written first (MultiSink order), so by the time we observe it here it already
	// reflects this snapshot — and must read "estimating", never terminal.
	var sawEstimating bool
	eng.SetEstimateProgress(func(progress.Snapshot, bool) {
		snap, err := progress.Load(workdir)
		if err != nil {
			t.Errorf("load run-status during estimate: %v", err)
			return
		}
		if snap.Phase == progress.PhaseEstimating {
			sawEstimating = true
		}
		if snap.Phase.Terminal() {
			t.Errorf("run-status reached terminal phase %q during the estimate phase", snap.Phase)
		}
	})

	s, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sawEstimating {
		t.Error("run-status file never showed the estimating phase")
	}

	snap, err := progress.Load(workdir)
	if err != nil {
		t.Fatalf("load final run-status: %v", err)
	}
	if snap.Phase != progress.PhaseDone {
		t.Errorf("final phase = %q, want done", snap.Phase)
	}
	if snap.SlotID != s.ID {
		t.Errorf("final slot = %q, want %q", snap.SlotID, s.ID)
	}
}

// TestThroughputCapThrottlesDump verifies the acceptance criterion: a configured
// per-medium throughput cap holds the measured dump rate at/under the limit, and
// an uncapped run of the same source is faster (so the delay is the cap, not slow
// tar).
func TestThroughputCapThrottlesDump(t *testing.T) {
	src := t.TempDir()
	// ~3.2 MiB so the transfer dominates the token bucket's initial burst
	// (ratelimit.maxBurst = 1 MiB), making the throttle observable in a short test.
	big := strings.Repeat("nbackup-bandwidth-throttle-test\n", 100_000)
	write(t, filepath.Join(src, "big.txt"), big)

	run := func(throughput string) (time.Duration, int64) {
		t.Helper()
		cfg := &config.Config{
			Landing: "disk",
			Media: map[string]config.Media{
				"disk": {Type: "disk", Throughput: throughput, Params: map[string]string{"path": t.TempDir()}},
			},
			Sources:  []config.DLE{{Host: "localhost", Path: src}},
			Workdir:  t.TempDir(),
			StateDir: t.TempDir(),
		}
		cfg.Compress.Scheme = "none" // bytes on the medium ≈ the tar stream, no compressor binary
		eng, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
			t.Skipf("GNU tar not available")
		}
		start := time.Now()
		s, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
		if err != nil {
			t.Fatalf("run (throughput=%q): %v", throughput, err)
		}
		return time.Since(start), s.TotalBytes()
	}

	const rate = 2 << 20 // 2 MiB/s
	capped, bytes := run("2MB/s")
	uncapped, _ := run("")

	// The bucket starts with at most one burst (ratelimit.maxBurst = 1 MiB) of slack, so
	// the run can be no faster than (bytes - 1 MiB) / rate. A 10% margin absorbs
	// scheduling jitter.
	floor := time.Duration(float64(bytes-(1<<20)) / rate * float64(time.Second) * 0.9)
	if capped < floor {
		t.Errorf("capped dump took %v; a 2MB/s cap over %d bytes implies at least %v", capped, bytes, floor)
	}
	if capped <= uncapped {
		t.Errorf("throughput cap had no effect: capped %v <= uncapped %v", capped, uncapped)
	}
}

// TestThroughputCapThrottlesRestore verifies the read side honors the cap: a slot
// dumped uncapped, then restored through an engine whose medium carries a cap, is
// paced to that cap (the read peer of the dump throttle).
func TestThroughputCapThrottlesRestore(t *testing.T) {
	src := t.TempDir()
	diskDir := t.TempDir()
	workdir := t.TempDir()
	big := strings.Repeat("nbackup-bandwidth-throttle-test\n", 100_000) // ~3.2 MiB
	write(t, filepath.Join(src, "big.txt"), big)

	mk := func(throughput string) *Engine {
		t.Helper()
		cfg := &config.Config{
			Landing: "disk",
			Media: map[string]config.Media{
				"disk": {Type: "disk", Throughput: throughput, Params: map[string]string{"path": diskDir}},
			},
			Sources:  []config.DLE{{Host: "localhost", Path: src}},
			Workdir:  workdir,
			StateDir: t.TempDir(),
		}
		cfg.Compress.Scheme = "none"
		eng, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return eng
	}

	// Dump uncapped.
	eng := mk("")
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}

	// Restore through a capped engine over the same medium/catalog.
	const rate = 2 << 20 // 2 MiB/s
	capped := mk("2MB/s")
	name := config.DLE{Host: "localhost", Path: src}.Name()
	start := time.Now()
	if err := capped.Restore(s.ID, name, t.TempDir(), false, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	elapsed := time.Since(start)

	floor := time.Duration(float64(s.TotalBytes()-(1<<20)) / rate * float64(time.Second) * 0.9)
	if elapsed < floor {
		t.Errorf("capped restore took %v; a 2MB/s read cap over %d bytes implies at least %v", elapsed, s.TotalBytes(), floor)
	}
}

// TestCopyToTapeAndRestore dumps to disk, copies the slot to a (virtual) tape
// medium, then restores it from the tape alone — exercising CopySlot and a tape
// Volume end to end.
func TestCopyToTapeAndRestore(t *testing.T) {
	src := t.TempDir()
	diskDir := t.TempDir()
	tapeDir := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "copy me to tape")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": diskDir}},
			"tape": {Type: "tape", Params: map[string]string{"dir": tapeDir}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	day := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	s, err := eng.Run(context.Background(), day, nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	// Copy is a write, so the tape must be labeled first.
	if err := eng.LabelVolume("tape", "tape-0001", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label tape: %v", err)
	}
	if err := eng.CopySlot(s.ID, "", "tape", false, nil); err != nil {
		t.Fatalf("copy to tape: %v", err)
	}

	// Restore from the tape alone: a fresh engine landed on the tape rebuilds its
	// catalog from the volume, then restores.
	tcfg := &config.Config{
		Landing:  "tape",
		Media:    map[string]config.Media{"tape": {Type: "tape", Params: map[string]string{"dir": tapeDir}}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(), // separate catalog cache, forcing a rebuild from tape
	}
	tcfg.Compress.Scheme = "none"
	teng, err := New(tcfg)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := teng.RebuildCatalog(nil); err != nil || n != 1 {
		t.Fatalf("rebuild from tape: n=%d err=%v", n, err)
	}
	dest := t.TempDir()
	name := config.DLE{Host: "localhost", Path: src}.Name()
	if err := teng.Restore(s.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore from tape: %v", err)
	}
	assertContent(t, filepath.Join(dest, "f.txt"), "copy me to tape")
}

// TestTapeLabelVerify exercises the label protocol on a tape landing: a dump is
// refused on a blank tape, succeeds after `nb label`, and is refused when the
// catalog expects a different label than the one mounted (a swapped tape).
func TestTapeLabelVerify(t *testing.T) {
	src := t.TempDir()
	tapeDir := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "data")

	cfg := &config.Config{
		Landing:  "lto",
		Media:    map[string]config.Media{"lto": {Type: "tape", Params: map[string]string{"dir": tapeDir}}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	day := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)

	// Blank tape: dump refused.
	if _, err := eng.Run(context.Background(), day, nil); err == nil {
		t.Fatal("expected dump to be refused on a blank/unlabeled tape")
	}

	// Label it, then a dump succeeds.
	if err := eng.LabelVolume("lto", "lto-0001", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label: %v", err)
	}
	if _, err := eng.Run(context.Background(), day, nil); err != nil {
		t.Fatalf("dump after label: %v", err)
	}

	// Out-of-band relabel of the loaded tape (same name, bumped epoch) makes the
	// catalog stale for it; a dump must refuse until `nb rebuild`. (Loading
	// a genuinely different tape from the pool is not an error under a changer.)
	lv := eng.vol.(media.Labeled)
	if err := lv.WriteLabel(record.Label{Name: "lto-0001", Pool: "lto", Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	day2 := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(context.Background(), day2, nil); err == nil {
		t.Fatal("expected dump to be refused when the mounted tape was relabeled since the catalog was updated")
	}
}

// TestDecryptOptsForPerDumptype locks the H1 fix: the decrypt key reference is resolved
// per-DLE from its dumptype's encrypt block (a per-dumptype passphrase_file), falling back
// to the config-wide block — mirroring the dump side. Without it a per-dumptype passphrase
// is dropped on read-back and recover/verify --deep/drill cannot decrypt.
func TestDecryptOptsForPerDumptype(t *testing.T) {
	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
		Encrypt:  config.EncryptConfig{Scheme: "gpg", PassphraseFile: "/keys/wide"},
		DumpTypes: map[string]config.DumpType{
			"secure": {Archiver: "gnutar", Encrypt: &config.EncryptConfig{Scheme: "gpg", PassphraseFile: "/keys/secure"}},
		},
		Sources: []config.DLE{
			{Host: "localhost", Path: "/a", DumpType: "secure"},
			{Host: "localhost", Path: "/b"}, // default dumptype → config-wide encrypt
		},
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	secure := config.DLE{Host: "localhost", Path: "/a", DumpType: "secure"}
	if got := eng.decryptOptsFor(secure.Name()); got.PassphraseFile != "/keys/secure" {
		t.Errorf("per-dumptype DLE: passphrase = %q, want /keys/secure", got.PassphraseFile)
	}
	plain := config.DLE{Host: "localhost", Path: "/b"}
	if got := eng.decryptOptsFor(plain.Name()); got.PassphraseFile != "/keys/wide" {
		t.Errorf("default-dumptype DLE: passphrase = %q, want the config-wide /keys/wide", got.PassphraseFile)
	}
	// An unknown DLE falls back to the config-wide block.
	if got := eng.decryptOptsFor("nope"); got.PassphraseFile != "/keys/wide" {
		t.Errorf("unknown DLE: passphrase = %q, want the config-wide /keys/wide", got.PassphraseFile)
	}
}

// TestRelabelRefusesForeignPool locks the guard that a `nb label --relabel` of a
// readable NBackup reel from another pool is refused without --force — the same foreign
// reel the dump write-path refuses. Without the guard the relabel would silently clobber
// a wrong-pool tape, contradicting the "a foreign or wrong-pool reel is never clobbered"
// promise (the corrupt/non-NBackup guards miss it because the label parses cleanly).
func TestRelabelRefusesForeignPool(t *testing.T) {
	tapeDir := t.TempDir()
	cfg := &config.Config{
		Landing:  "lto",
		Media:    map[string]config.Media{"lto": {Type: "tape", Params: map[string]string{"dir": tapeDir}}},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Label it once so the landing volume is open (it opens lazily), then plant a
	// readable label from a different pool directly on the loaded volume.
	if err := eng.LabelVolume("lto", "lto-0001", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("initial label: %v", err)
	}
	lv := eng.vol.(media.Labeled)
	if err := lv.WriteLabel(record.Label{Name: "FOREIGN-01", Pool: "otherpool", Epoch: 1}); err != nil {
		t.Fatal(err)
	}

	// A relabel without --force must refuse it, naming the foreign pool.
	err = eng.LabelVolume("lto", "STOLEN-01", true, false, time.Now().UTC(), nil)
	if err == nil {
		t.Fatal("relabel of a wrong-pool reel should be refused without --force")
	}
	if !strings.Contains(err.Error(), "otherpool") {
		t.Errorf("refusal should name the foreign pool; got: %v", err)
	}

	// --force is the documented escape hatch and proceeds.
	if err := eng.LabelVolume("lto", "STOLEN-01", true, true, time.Now().UTC(), nil); err != nil {
		t.Fatalf("relabel --force should proceed: %v", err)
	}
	if got, ok, err := lv.ReadLabel(); err != nil || !ok || got.Name != "STOLEN-01" || got.Pool != "lto" {
		t.Errorf("after --force relabel: got %+v ok=%v err=%v, want STOLEN-01 in pool lto", got, ok, err)
	}
}

// TestCopyRecordsPlacementAndFailover dumps to disk, copies to a second medium,
// confirms the slot now has two placements, then physically removes the primary
// copy and restores — proving restore falls over to the recorded copy.
func TestCopyRecordsPlacementAndFailover(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "two homes")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"archive": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := eng.CopySlot(s.ID, "", "archive", false, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}

	// A second copy to the same medium is refused (idempotent) unless forced.
	if err := eng.CopySlot(s.ID, "", "archive", false, nil); err == nil {
		t.Fatal("expected re-copy to the same medium to be refused without --force")
	}
	if err := eng.CopySlot(s.ID, "", "archive", true, nil); err != nil {
		t.Fatalf("forced re-copy: %v", err)
	}

	if got := len(eng.cat.Placements(s.ID)); got != 2 {
		t.Fatalf("expected 2 placements after copy, got %d", got)
	}
	if eng.cat.MediumBytes("archive") == 0 {
		t.Errorf("archive medium should report stored bytes")
	}

	// Physically remove the primary copy but leave its placement recorded: restore
	// must try it, fail, and fall over to the archive copy.
	removeSlotFiles(t, eng, s.ID)
	dest := t.TempDir()
	name := config.DLE{Host: "localhost", Path: src}.Name()
	if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore (failover to copy): %v", err)
	}
	assertContent(t, filepath.Join(dest, "f.txt"), "two homes")
}

// TestRunWritesStatus confirms a dump leaves a terminal run-status file in the
// catalog workdir, reflecting the sealed slot — the input `nb status` reads.
func TestRunWritesStatus(t *testing.T) {
	src := t.TempDir()
	workdir := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "status me")

	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  workdir,
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}

	snap, err := progress.Load(workdir)
	if err != nil {
		t.Fatalf("load status: %v", err)
	}
	if snap.SlotID != s.ID {
		t.Errorf("status slot = %q, want %q", snap.SlotID, s.ID)
	}
	if snap.Phase != progress.PhaseDone {
		t.Errorf("status phase = %s, want done", snap.Phase)
	}
	if _, done, failed, _ := snap.Counts(); done != 1 || failed != 0 {
		t.Errorf("counts done=%d failed=%d, want 1/0", done, failed)
	}
	if snap.DLEs[0].DoneBytes == 0 {
		t.Error("status should record archived bytes")
	}
}

func boolp(b bool) *bool { return &b }

// removeSlotFiles deletes every file of a slot from the landing volume by position —
// the test peer of a whole-slot reclamation now that the Volume seam is per-file. Used
// to simulate a copy going missing from one medium.
func removeSlotFiles(t *testing.T, eng *Engine, slotID string) {
	t.Helper()
	vol, err := eng.landing()
	if err != nil {
		t.Fatal(err)
	}
	files, err := vol.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Header.Slot != slotID {
			continue
		}
		if err := vol.RemoveFile(f.Pos); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTapeLibraryRestore copies two slots onto two different tapes in a library,
// removes the disk copies, then restores both — proving the changer auto-mounts
// the bay holding each slot's tape on the read side.
// TestHoldingDiskBuffersTape exercises the holding-disk path: a disk marked holding: true
// buffers a tape landing. Several DLEs dump to the disk in parallel; the drain copies each to
// tape and reclaims the disk; afterward the disk is empty, the slot lives on tape, and the
// chain restores from tape alone.
func TestHoldingDiskBuffersTape(t *testing.T) {
	srcA := t.TempDir()
	srcB := t.TempDir()
	write(t, filepath.Join(srcA, "a.txt"), "alpha payload")
	write(t, filepath.Join(srcB, "b.txt"), "bravo payload")
	scratchDir := t.TempDir()

	cfg := &config.Config{
		Landing: "lto",
		Media: map[string]config.Media{
			"lto":     {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "4"}},
			"scratch": {Type: "disk", Holding: true, Capacity: "500MB", Params: map[string]string{"path": scratchDir}},
		},
		Sources: []config.DLE{
			{Host: "localhost", Path: srcA},
			{Host: "localhost", Path: srcB},
		},
		Workdir:   t.TempDir(),
		StateDir:  t.TempDir(),
		AutoLabel: true,
	}
	cfg.Compress.Scheme = "none"
	cfg.Parallelism.Workers = 2

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("holding-disk dump: %v", err)
	}

	// The authoritative copy is on tape; the holding disk has been fully reclaimed.
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "scratch" {
			t.Errorf("holding disk should hold no placement after the run, got %v", p)
		}
	}
	scratchVol, _, _, err := eng.mediumVolume("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := scratchVol.Files(); len(files) != 0 {
		t.Errorf("holding disk must be empty after the run, has %d file(s)", len(files))
	}

	// Restore both DLEs from tape alone — the holding disk is empty.
	for _, tc := range []struct{ src, file, want string }{
		{srcA, "a.txt", "alpha payload"},
		{srcB, "b.txt", "bravo payload"},
	} {
		dest := t.TempDir()
		name := config.DLE{Host: "localhost", Path: tc.src}.Name()
		if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
			t.Fatalf("restore %s from tape: %v", name, err)
		}
		assertContent(t, filepath.Join(dest, tc.file), tc.want)
	}
}

// TestHoldingDisksSpread runs a buffered dump across TWO holding disks: the dumpers spread their
// archives over both (round-robin), the single drainer copies each from the disk it landed on, and
// both disks are reclaimed. Capacities are sized so neither disk alone could hold all the DLEs, so
// the run depends on using both. Every DLE must land on tape, both disks must end empty, and all
// must restore from tape alone.
func TestHoldingDisksSpread(t *testing.T) {
	var sources []config.DLE
	type want struct{ file, body string }
	bodies := map[string]want{} // src dir -> expected file + content
	for _, n := range []string{"a", "b", "c", "d"} {
		src := t.TempDir()
		body := "payload-" + n
		write(t, filepath.Join(src, n+".txt"), body)
		sources = append(sources, config.DLE{Host: "localhost", Path: src})
		bodies[src] = want{file: n + ".txt", body: body}
	}

	cfg := &config.Config{
		Landing: "lto",
		Media: map[string]config.Media{
			"lto": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "4"}},
			"s1":  {Type: "disk", Holding: true, Capacity: "500MB", Params: map[string]string{"path": t.TempDir()}},
			"s2":  {Type: "disk", Holding: true, Capacity: "500MB", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources:   sources,
		Workdir:   t.TempDir(),
		StateDir:  t.TempDir(),
		AutoLabel: true,
	}
	cfg.Compress.Scheme = "none"
	cfg.Parallelism.Workers = 2

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("multi-disk holding dump: %v", err)
	}

	// Every authoritative copy is on tape; both holding disks have been fully reclaimed.
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "s1" || p.Medium == "s2" {
			t.Errorf("holding disk %q should hold no placement after the run, got %v", p.Medium, p)
		}
	}
	for _, h := range []string{"s1", "s2"} {
		vol, _, _, err := eng.mediumVolume(h)
		if err != nil {
			t.Fatal(err)
		}
		if files, _ := vol.Files(); len(files) != 0 {
			t.Errorf("holding disk %q must be empty after the run, has %d file(s)", h, len(files))
		}
	}

	for src, w := range bodies {
		dest := t.TempDir()
		name := config.DLE{Host: "localhost", Path: src}.Name()
		if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
			t.Fatalf("restore %s from tape: %v", name, err)
		}
		assertContent(t, filepath.Join(dest, w.file), w.body)
	}
}

// TestHoldingDisksFlush drains leftover archives staged across TWO holding disks. It builds the
// post-crash state directly — two slots, one stranded on each disk — then runs Flush and confirms
// it gathers the slots across both disks (the union), drains each from the right disk, reclaims
// both, and the chains restore from tape. This exercises the multi-disk Flush path (per-disk holdVol
// resolution, per-disk placement reclaim) that a single-disk Flush cannot.
func TestHoldingDisksFlush(t *testing.T) {
	workdir, stateDir := t.TempDir(), t.TempDir()
	d1, d2 := t.TempDir(), t.TempDir()
	src1, src2 := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src1, "one.txt"), "stranded on disk one")
	write(t, filepath.Join(src2, "two.txt"), "stranded on disk two")

	// Stage: dump each DLE onto its own scratch disk acting as a landing, leaving a catalogued slot
	// stranded on each — the state a crashed multi-disk holding run leaves behind.
	stage := func(disk, path string, src config.DLE, date time.Time) *catalog.Slot {
		cfg := &config.Config{
			Landing:  disk,
			Media:    map[string]config.Media{disk: {Type: "disk", Params: map[string]string{"path": path}}},
			Sources:  []config.DLE{src},
			Workdir:  workdir,
			StateDir: stateDir,
		}
		cfg.Compress.Scheme = "none"
		eng, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
			t.Skipf("GNU tar not available")
		}
		s, err := eng.Run(context.Background(), date, nil)
		if err != nil {
			t.Fatalf("stage dump on %s: %v", disk, err)
		}
		return s
	}
	s1 := stage("s1", d1, config.DLE{Host: "localhost", Path: src1}, time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC))
	s2 := stage("s2", d2, config.DLE{Host: "localhost", Path: src2}, time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC))

	// Now treat both scratch disks as holding disks for a tape landing (same workdir, so the catalog
	// still holds both scratch placements) and flush.
	flushCfg := &config.Config{
		Landing: "lto",
		Media: map[string]config.Media{
			"lto": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "2"}},
			"s1":  {Type: "disk", Holding: true, Params: map[string]string{"path": d1}},
			"s2":  {Type: "disk", Holding: true, Params: map[string]string{"path": d2}},
		},
		Sources:   []config.DLE{{Host: "localhost", Path: src1}, {Host: "localhost", Path: src2}},
		Workdir:   workdir,
		StateDir:  stateDir,
		AutoLabel: true,
	}
	flushCfg.Compress.Scheme = "none"
	flushEng, err := New(flushCfg)
	if err != nil {
		t.Fatal(err)
	}
	n, err := flushEng.Flush(time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), logfDiscard)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 2 {
		t.Fatalf("flush moved %d archives, want 2 (one per disk)", n)
	}

	for h := range map[string]struct{}{"s1": {}, "s2": {}} {
		vol, _, _, err := flushEng.mediumVolume(h)
		if err != nil {
			t.Fatal(err)
		}
		if files, _ := vol.Files(); len(files) != 0 {
			t.Errorf("holding disk %q must be empty after flush, has %d file(s)", h, len(files))
		}
	}
	for _, tc := range []struct {
		s    *catalog.Slot
		src  string
		file string
		want string
	}{
		{s1, src1, "one.txt", "stranded on disk one"},
		{s2, src2, "two.txt", "stranded on disk two"},
	} {
		if !placedOnLanding(flushEng, tc.s.ID, config.DLE{Host: "localhost", Path: tc.src}.Name()) {
			t.Errorf("archive %s must be on the tape landing after flush", tc.s.ID)
		}
		dest := t.TempDir()
		if err := flushEng.Restore(tc.s.ID, config.DLE{Host: "localhost", Path: tc.src}.Name(), dest, false, nil); err != nil {
			t.Fatalf("restore %s from tape after flush: %v", tc.s.ID, err)
		}
		assertContent(t, filepath.Join(dest, tc.file), tc.want)
	}
}

// TestHoldingDiskDrainSpansVolumes exercises the drain's volume-roll path: the tape landing has a
// small volume_size, so each staged archive spans several reels while it copies. Every roll runs
// the real WriteSink — and its catalog write — on the orchestrator via the sink Client, while the
// drainer goroutine streams the bytes and the other DLE's commit is recorded concurrently. The run
// must complete, each archive must land spanning >= 2 parts, the disk must be reclaimed, and both
// chains must restore from the spanned tapes.
func TestHoldingDiskDrainSpansVolumes(t *testing.T) {
	srcA, srcB := t.TempDir(), t.TempDir()
	// Bodies well over the 160 KiB reel so each archive rolls across volumes mid-drain.
	write(t, filepath.Join(srcA, "a.txt"), strings.Repeat("alpha-spanning-payload-", 16*1024))
	write(t, filepath.Join(srcB, "b.txt"), strings.Repeat("bravo-spanning-payload-", 16*1024))
	scratchDir := t.TempDir()

	cfg := &config.Config{
		Landing:   "lto",
		AutoLabel: true,
		Media: map[string]config.Media{
			"lto":     {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "12", "volume_size": "163840"}},
			"scratch": {Type: "disk", Holding: true, Capacity: "500MB", Params: map[string]string{"path": scratchDir}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: srcA}, {Host: "localhost", Path: srcB}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	cfg.Parallelism.Workers = 2

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s, err := eng.Run(context.Background(), time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("holding-disk dump with spanning landing: %v", err)
	}

	// Each archive landed on tape spanning >= 2 parts (it rolled mid-drain), and the holding disk
	// is fully reclaimed.
	var tape catalog.Placement
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "scratch" {
			t.Errorf("holding disk should hold no placement after the run, got %v", p)
		}
		if p.Medium == "lto" {
			tape = p
		}
	}
	for _, src := range []string{srcA, srcB} {
		name := config.DLE{Host: "localhost", Path: src}.Name()
		if parts, _ := tape.Parts(name, 0); len(parts) < 2 {
			t.Fatalf("archive %s landed in %d part(s), want >= 2 (must span)", name, len(parts))
		}
	}
	if scratchVol, _, _, err := eng.mediumVolume("scratch"); err != nil {
		t.Fatal(err)
	} else if files, _ := scratchVol.Files(); len(files) != 0 {
		t.Errorf("holding disk must be empty after the run, has %d file(s)", len(files))
	}

	// Both chains restore from the spanned tapes alone.
	for _, tc := range []struct{ src, file, want string }{
		{srcA, "a.txt", strings.Repeat("alpha-spanning-payload-", 16*1024)},
		{srcB, "b.txt", strings.Repeat("bravo-spanning-payload-", 16*1024)},
	} {
		dest := t.TempDir()
		name := config.DLE{Host: "localhost", Path: tc.src}.Name()
		if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
			t.Fatalf("restore %s from spanned tape: %v", name, err)
		}
		assertContent(t, filepath.Join(dest, tc.file), tc.want)
	}
}

// TestHoldingDiskRoutesOversizedDirect: with a holding capacity between a small and a large DLE,
// the large one is dumped straight to the landing (it would not fit the disk) while the small one
// buffers and drains. Both land on tape, the disk ends empty, and both restore from tape.
func TestHoldingDiskRoutesOversizedDirect(t *testing.T) {
	srcSmall, srcBig := t.TempDir(), t.TempDir()
	smallBody := "small payload"
	bigBody := strings.Repeat("oversized-direct-payload-", 48*1024) // ~1.2 MiB, over the 512 KiB cap
	write(t, filepath.Join(srcSmall, "s.txt"), smallBody)
	write(t, filepath.Join(srcBig, "b.txt"), bigBody)
	scratchDir := t.TempDir()

	cfg := &config.Config{
		Landing:   "lto",
		AutoLabel: true,
		Media: map[string]config.Media{
			"lto":     {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "4"}},
			"scratch": {Type: "disk", Holding: true, Capacity: "512KB", Params: map[string]string{"path": scratchDir}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: srcSmall}, {Host: "localhost", Path: srcBig}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	cfg.Parallelism.Workers = 2

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	// Guard the premise: the big DLE's estimate must exceed the capacity (routed direct) and the
	// small one's must not — otherwise the test wouldn't exercise the direct path.
	plan := eng.Plan(time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC))
	capBytes, _ := cfg.Media["scratch"].CapacityBytes()
	var directCount int
	for _, it := range plan.Items {
		if capBytes > 0 && it.EstBytes >= capBytes { // too big for the single holding disk
			directCount++
		}
	}
	if directCount != 1 {
		t.Skipf("estimates didn't split as intended (%d of %d route direct); skipping", directCount, len(plan.Items))
	}

	s, err := eng.Run(context.Background(), time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("mixed direct/buffered dump: %v", err)
	}
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "scratch" {
			t.Errorf("holding disk should hold no placement after the run, got %v", p)
		}
	}
	if scratchVol, _, _, err := eng.mediumVolume("scratch"); err != nil {
		t.Fatal(err)
	} else if files, _ := scratchVol.Files(); len(files) != 0 {
		t.Errorf("holding disk must be empty after the run, has %d file(s)", len(files))
	}
	for _, tc := range []struct{ src, file, want string }{
		{srcSmall, "s.txt", smallBody},
		{srcBig, "b.txt", bigBody},
	} {
		dest := t.TempDir()
		name := config.DLE{Host: "localhost", Path: tc.src}.Name()
		if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
			t.Fatalf("restore %s from tape: %v", name, err)
		}
		assertContent(t, filepath.Join(dest, tc.file), tc.want)
	}
}

// TestHoldingDiskAllDirect: a holding capacity smaller than every DLE routes them all direct — the
// run degenerates to serial landing dumps with the disk untouched, and still restores from tape.
func TestHoldingDiskAllDirect(t *testing.T) {
	srcA, srcB := t.TempDir(), t.TempDir()
	write(t, filepath.Join(srcA, "a.txt"), "alpha payload")
	write(t, filepath.Join(srcB, "b.txt"), "bravo payload")

	cfg := &config.Config{
		Landing:   "lto",
		AutoLabel: true,
		Media: map[string]config.Media{
			"lto":     {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "4"}},
			"scratch": {Type: "disk", Holding: true, Capacity: "1KB", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: srcA}, {Host: "localhost", Path: srcB}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	cfg.Parallelism.Workers = 2

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("all-direct dump: %v", err)
	}
	for _, tc := range []struct{ src, file, want string }{
		{srcA, "a.txt", "alpha payload"},
		{srcB, "b.txt", "bravo payload"},
	} {
		dest := t.TempDir()
		name := config.DLE{Host: "localhost", Path: tc.src}.Name()
		if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
			t.Fatalf("restore %s from tape: %v", name, err)
		}
		assertContent(t, filepath.Join(dest, tc.file), tc.want)
	}
}

// A holding disk must be a medium that accepts concurrent writes and per-archive reclaim
// (disk, cloud) — a tape sink is neither, so New rejects it. The capability is a media-layer
// property (media.ConcurrentWrite), not a hardcoded type list in config.
func TestHoldingDiskRejectsTape(t *testing.T) {
	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":  {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"vault": {Type: "tape", Holding: true, Params: map[string]string{"dir": t.TempDir()}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	_, err := New(cfg)
	if err == nil || !strings.Contains(err.Error(), "requires a disk or cloud medium") {
		t.Fatalf("want disk/cloud requirement error, got %v", err)
	}
}

// TestHoldingDiskFlush drains leftover holding-disk archives to the landing. It builds the
// post-crash state directly — archives staged on a disk and recorded in the catalog — then
// runs Flush (as `nb flush`/auto-flush would) and confirms they move to tape, the disk is
// reclaimed, and the chain restores from tape.
func TestHoldingDiskFlush(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "stranded on the holding disk")
	scratchDir := t.TempDir()
	workdir := t.TempDir()
	stateDir := t.TempDir()
	sources := []config.DLE{{Host: "localhost", Path: src}}

	// Stage: dump onto the scratch disk as a landing (leaves a catalogued slot on scratch —
	// the state a holding-disk run leaves behind when it crashes before flushing).
	stageCfg := &config.Config{
		Landing:  "scratch",
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
	if m, err := stageEng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := stageEng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("stage dump: %v", err)
	}

	// Now treat that scratch disk as a holding disk for a tape landing (same workdir, so the
	// catalog still holds the scratch placement) and flush.
	flushCfg := &config.Config{
		Landing: "lto",
		Media: map[string]config.Media{
			"lto":     {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "2"}},
			"scratch": {Type: "disk", Holding: true, Params: map[string]string{"path": scratchDir}},
		},
		Sources:   sources,
		Workdir:   workdir,
		StateDir:  stateDir,
		AutoLabel: true,
	}
	flushCfg.Compress.Scheme = "none"
	flushEng, err := New(flushCfg)
	if err != nil {
		t.Fatal(err)
	}
	n, err := flushEng.Flush(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), logfDiscard)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 1 {
		t.Fatalf("flush moved %d archives, want 1", n)
	}

	// The archive is now on tape and gone from the holding disk.
	if !placedOnLanding(flushEng, s.ID, config.DLE{Host: "localhost", Path: src}.Name()) {
		t.Errorf("archive must be on the tape landing after flush")
	}
	scratchVol, _, _, err := flushEng.mediumVolume("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := scratchVol.Files(); len(files) != 0 {
		t.Errorf("holding disk must be empty after flush, has %d file(s)", len(files))
	}
	dest := t.TempDir()
	if err := flushEng.Restore(s.ID, config.DLE{Host: "localhost", Path: src}.Name(), dest, false, nil); err != nil {
		t.Fatalf("restore from tape after flush: %v", err)
	}
	assertContent(t, filepath.Join(dest, "f.txt"), "stranded on the holding disk")
}

// TestHoldingDiskLandingDownFails: when the landing (tape) cannot be written (blank, no
// auto_label), the holding-disk run fails up front and records nothing — it degrades by
// refusing to proceed, never by dropping data on the disk and pretending it's safe.
func TestHoldingDiskLandingDownFails(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "needs a home")

	cfg := &config.Config{
		Landing: "lto",
		Media: map[string]config.Media{
			"lto":     {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "1"}},
			"scratch": {Type: "disk", Holding: true, Params: map[string]string{"path": t.TempDir()}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
		// AutoLabel deliberately off: the blank tape cannot be written.
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil); err == nil {
		t.Fatal("holding-disk run must fail when the landing is unwritable")
	}
	if len(eng.cat.Slots()) != 0 {
		t.Errorf("a failed holding-disk run must record no slot, got %d", len(eng.cat.Slots()))
	}
}

// TestDumpContinuesPastFailedDLE confirms a single DLE's failure no longer aborts the whole run: a
// missing source (scheduled first, so the old fail-fast would have stopped here) fails at tar time,
// but the good DLE after it is still dumped and committed, and the run reports failure overall.
func TestDumpContinuesPastFailedDLE(t *testing.T) {
	good := t.TempDir()
	write(t, filepath.Join(good, "f.txt"), "i survive")
	bad := filepath.Join(t.TempDir(), "does-not-exist") // never created -> tar fails at dump time

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		// Bad DLE first: with the default single worker it is attempted before the good one, so the
		// old "first error stops scheduling" would never reach the good DLE.
		Sources:  []config.DLE{{Host: "localhost", Path: bad}, {Host: "localhost", Path: good}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil); err == nil {
		t.Fatal("a run with a failing DLE must report failure")
	}

	// The good DLE committed despite the earlier failure: exactly one archive landed.
	slots := eng.cat.Slots()
	if len(slots) != 1 {
		t.Fatalf("expected the good DLE to commit one slot, got %d", len(slots))
	}
	if got := len(slots[0].Archives); got != 1 {
		t.Fatalf("expected 1 committed archive (the good DLE), got %d", got)
	}
	if got := slots[0].Archives[0].DLE; !strings.Contains(got, filepath.Base(good)) {
		t.Errorf("committed archive should be the good DLE, got %q", got)
	}
}

func TestTapeLibraryRestore(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "v1")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lib":  {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "2"}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s1, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	if err := eng.LabelVolume("lib", "Tape1", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Tape1: %v", err)
	}
	if err := eng.CopySlot(s1.ID, "", "lib", false, nil); err != nil {
		t.Fatalf("copy s1: %v", err)
	}

	// Sleep BEFORE writing v2 so its mtime is strictly newer than the L0 dump's
	// snapshot timestamp — GNU tar's listed-incremental only captures a file
	// modified after the base, so a v2 written in the same instant as the L0 dump
	// could be missed by the L1 and the restore would see v1 (a flaky failure).
	time.Sleep(1100 * time.Millisecond)
	write(t, filepath.Join(src, "f.txt"), "v2")
	s2, err := eng.Run(context.Background(), time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}
	if err := eng.LabelVolume("lib", "Tape2", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Tape2: %v", err)
	}
	if err := eng.CopySlot(s2.ID, "", "lib", false, nil); err != nil {
		t.Fatalf("copy s2: %v", err)
	}

	// The two copies must live on different tapes.
	if v := eng.cat.Placements(s1.ID); len(v) < 2 {
		t.Fatalf("s1 should have a tape copy, placements=%v", v)
	}

	// Drop the disk copies so restore must fall over to the tapes (different bays).
	removeSlotFiles(t, eng, s1.ID)
	removeSlotFiles(t, eng, s2.ID)

	name := config.DLE{Host: "localhost", Path: src}.Name()
	d1 := t.TempDir()
	if err := eng.Restore(s1.ID, name, d1, false, nil); err != nil {
		t.Fatalf("restore s1 (auto-mount Tape1): %v", err)
	}
	assertContent(t, filepath.Join(d1, "f.txt"), "v1")
	d2 := t.TempDir()
	if err := eng.Restore(s2.ID, name, d2, false, nil); err != nil {
		t.Fatalf("restore s2 (auto-mount Tape2): %v", err)
	}
	assertContent(t, filepath.Join(d2, "f.txt"), "v2")
}

// TestTapeAppendableFalse: a one-run-per-tape medium refuses a second run on a
// tape that already holds one; a fresh tape accepts it.
func TestTapeAppendableFalse(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "data")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lib":  {Type: "tape", Appendable: boolp(false), Params: map[string]string{"dir": t.TempDir(), "bays": "2"}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s1, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	// Sleep BEFORE writing data2 so its mtime is strictly newer than the L0 dump's
	// snapshot timestamp — otherwise the L1 could miss a same-instant change.
	time.Sleep(1100 * time.Millisecond)
	write(t, filepath.Join(src, "f.txt"), "data2")
	s2, err := eng.Run(context.Background(), time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}

	if err := eng.LabelVolume("lib", "Tape1", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Tape1: %v", err)
	}
	if err := eng.CopySlot(s1.ID, "", "lib", false, nil); err != nil {
		t.Fatalf("copy s1 to fresh tape: %v", err)
	}
	// Tape1 now holds a run; a non-appendable medium refuses a second run on it.
	if err := eng.CopySlot(s2.ID, "", "lib", false, nil); err == nil {
		t.Fatal("expected copy onto a non-appendable tape that already holds a run to be refused")
	}
	// A fresh tape accepts it.
	if err := eng.LabelVolume("lib", "Tape2", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Tape2: %v", err)
	}
	if err := eng.CopySlot(s2.ID, "", "lib", false, nil); err != nil {
		t.Fatalf("copy s2 to fresh tape: %v", err)
	}
}

// scriptedOperator stands in for a human at a single-drive station: it loads the
// reel the engine asks for (the needed label on a read, any blank reel on a
// write) and counts how many swaps it performed.
type scriptedOperator struct{ swaps int }

func (o *scriptedOperator) Swap(r librarian.SwapRequest) (string, bool) {
	o.swaps++
	if r.Need != "" { // a read wants a specific label
		for _, b := range r.Shelf {
			if b.Label == r.Need {
				return b.ID, true
			}
		}
		return "", false
	}
	for _, b := range r.Shelf { // a write wants any writable (blank) reel
		if b.Blank {
			return b.ID, true
		}
	}
	return "", false
}

// TestManualStationWriteSwap: a copy to a single-drive station with an empty drive
// prompts the operator to load a reel; with auto_label on, the freshly loaded blank
// reel is labeled and the copy proceeds — the write-side swap path.
func TestManualStationWriteSwap(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "manual write")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lto":  {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "1"}},
		},
		Sources:   []config.DLE{{Host: "localhost", Path: src}},
		Workdir:   t.TempDir(),
		StateDir:  t.TempDir(),
		AutoLabel: true,
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	op := &scriptedOperator{}
	eng.SetOperator(op)

	s, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	// The drive is empty: the copy must prompt for a reel, then auto-label it.
	if err := eng.CopySlot(s.ID, "", "lto", false, logfDiscard); err != nil {
		t.Fatalf("copy to manual station: %v", err)
	}
	if op.swaps == 0 {
		t.Fatal("expected the operator to be prompted to load a reel")
	}
	found := false
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "lto" {
			found = true
		}
	}
	if !found {
		t.Fatal("slot should have a placement on the manual station")
	}
}

// TestManualStationReadSwap: two slots land on two reels of a single-drive station;
// with the disk copies gone, restoring each prompts the operator to swap the reel
// holding it into the one drive — the read-side swap path.
func TestManualStationReadSwap(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "v1")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lto":  {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "2"}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s1, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	// Reel A: load it, label it, copy s1 (the loaded reel is usable — no swap).
	if err := eng.LoadVolume("lto", "reel-01", false, logfDiscard); err != nil {
		t.Fatalf("load reel-01: %v", err)
	}
	if err := eng.LabelVolume("lto", "Reel-A", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Reel-A: %v", err)
	}
	if err := eng.CopySlot(s1.ID, "", "lto", false, logfDiscard); err != nil {
		t.Fatalf("copy s1: %v", err)
	}

	// Sleep BEFORE writing v2 so its mtime is strictly newer than the L0 dump's
	// snapshot timestamp — GNU tar's listed-incremental only captures a file
	// modified after the base, so a v2 written in the same instant as the L0 dump
	// could be missed by the L1 and the restore would see v1 (a flaky failure).
	time.Sleep(1100 * time.Millisecond)
	write(t, filepath.Join(src, "f.txt"), "v2")
	s2, err := eng.Run(context.Background(), time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}
	// Reel B: load (swaps A out of the one drive), label, copy s2.
	if err := eng.LoadVolume("lto", "reel-02", false, logfDiscard); err != nil {
		t.Fatalf("load reel-02: %v", err)
	}
	if err := eng.LabelVolume("lto", "Reel-B", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label Reel-B: %v", err)
	}
	if err := eng.CopySlot(s2.ID, "", "lto", false, logfDiscard); err != nil {
		t.Fatalf("copy s2: %v", err)
	}

	// Drop the disk copies so restore must read from the reels.
	removeSlotFiles(t, eng, s1.ID)
	removeSlotFiles(t, eng, s2.ID)

	op := &scriptedOperator{}
	eng.SetOperator(op)

	name := config.DLE{Host: "localhost", Path: src}.Name()
	// The drive holds Reel-B; restoring s1 must prompt to swap in Reel-A.
	d1 := t.TempDir()
	if err := eng.Restore(s1.ID, name, d1, false, logfDiscard); err != nil {
		t.Fatalf("restore s1 (swap in Reel-A): %v", err)
	}
	assertContent(t, filepath.Join(d1, "f.txt"), "v1")
	// And s2 must prompt to swap Reel-B back in.
	d2 := t.TempDir()
	if err := eng.Restore(s2.ID, name, d2, false, logfDiscard); err != nil {
		t.Fatalf("restore s2 (swap in Reel-B): %v", err)
	}
	assertContent(t, filepath.Join(d2, "f.txt"), "v2")

	if op.swaps == 0 {
		t.Fatal("expected the operator to be prompted to swap reels on read")
	}
}

// TestManualStationLandingLabel: labeling the engine's own (landing) single-drive
// station rebuilds its catalog against the freshly-labeled reel. Regression for the
// catalog rebuild treating a Station like a robotic Library — iterating bays and
// mounting a bay id, which a single-drive station has none of.
func TestManualStationLandingLabel(t *testing.T) {
	cfg := &config.Config{
		Landing: "vtape",
		Media: map[string]config.Media{
			"vtape": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "3"}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A reel must be in the drive to label it.
	if err := eng.LoadVolume("vtape", "reel-01", false, logfDiscard); err != nil {
		t.Fatalf("load reel-01: %v", err)
	}
	// Labeling the landing medium triggers a catalog rebuild against the loaded reel;
	// it must not try to bay-iterate a single-drive station.
	if err := eng.LabelVolume("vtape", "Label1", false, false, time.Now().UTC(), nil); err != nil {
		t.Fatalf("label landing manual station: %v", err)
	}
	if known, ok := eng.cat.Volume("Label1"); !ok || known.Label.Epoch != 1 {
		t.Fatalf("catalog should record Label1 at epoch 1 after rebuild (ok=%v)", ok)
	}
}

// tapeEngine builds an engine over a single-drive (manual) tape landing medium
// with the given appendability and minimum age, with no slots or volumes yet.
func tapeEngine(t *testing.T, appendable bool, minAge string) *Engine {
	t.Helper()
	cfg := &config.Config{
		Landing: "lto",
		Media: map[string]config.Media{
			"lto": {
				Type:       "tape",
				MinimumAge: minAge,
				Appendable: &appendable,
				Params:     map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "4"},
			},
		},
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

// recordVol registers a labeled volume in the catalog at a written-at time.
func recordVol(t *testing.T, eng *Engine, name string, writtenAt time.Time) {
	t.Helper()
	if err := eng.cat.RecordVolume(record.Label{Name: name, Pool: "lto", Epoch: 1, WrittenAt: writtenAt}); err != nil {
		t.Fatal(err)
	}
}

// recordFullOn records a sealed full of one DLE on a given volume.
func recordFullOn(t *testing.T, eng *Engine, date, dle, volume string) {
	t.Helper()
	recordSizedFullOn(t, eng, date, dle, volume, 0)
}

// recordSizedFullOn records a sealed full of one DLE on a given lto volume with a
// given payload size, so a reel's fill can be asserted.
func recordSizedFullOn(t *testing.T, eng *Engine, date, dle, volume string, bytes int64) {
	t.Helper()
	id := record.IDFromParts(date, 1)
	// Stamp the archive's CreatedAt at the slot's own date, not wall-clock: retention measures age
	// per archive from its landing instant, so one meant to read as dated `date` must land then.
	at, _ := record.ParseDateField(date)
	arch := record.Archive{Slot: id, DLE: dle, Level: 0, Compressed: bytes, CreatedAt: at}
	pos := catalog.ArchivePos{DLE: dle, Level: 0, Parts: []catalog.FilePos{{Label: volume, Epoch: 1, Pos: 1}}, Commit: catalog.FilePos{Label: volume, Epoch: 1, Pos: 2}}
	if err := eng.cat.AddArchive(arch, "lto", pos); err != nil {
		t.Fatal(err)
	}
}

// recordFullOnOtherMedium records a sealed full of one DLE whose only copy lives on
// a medium other than the tape pool — used to prove retention is judged per-medium.
func recordFullOnOtherMedium(t *testing.T, eng *Engine, date, dle, medium string) {
	t.Helper()
	id := record.IDFromParts(date, 1)
	at, _ := record.ParseDateField(date)
	arch := record.Archive{Slot: id, DLE: dle, Level: 0, CreatedAt: at}
	pos := catalog.ArchivePos{DLE: dle, Level: 0, Parts: []catalog.FilePos{{Label: medium, Pos: 1}}, Commit: catalog.FilePos{Label: medium, Pos: 2}}
	if err := eng.cat.AddArchive(arch, medium, pos); err != nil {
		t.Fatal(err)
	}
}

// TestExpectedTapeReusesOldest: on a one-run-per-tape medium the next run expects
// the oldest volume whose runs are all reusable (past minimum age with a newer
// recovery path).
func TestExpectedTapeReusesOldest(t *testing.T) {
	eng := tapeEngine(t, false, "10d")
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)

	recordVol(t, eng, "lto-0001", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	recordVol(t, eng, "lto-0002", time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))
	recordFullOn(t, eng, "2026-06-01", "h", "lto-0001") // old, superseded -> reusable
	recordFullOn(t, eng, "2026-06-20", "h", "lto-0002") // recent full -> protected

	exp, ok := eng.ExpectedVolume(now)
	if !ok {
		t.Fatal("a labeled medium should yield an expectation")
	}
	if exp.FreshVolume || exp.Label != "lto-0001" {
		t.Fatalf("want oldest reusable lto-0001, got %+v", exp)
	}
	if exp.Recycles != 1 {
		t.Fatalf("want 1 run recycled, got %d", exp.Recycles)
	}
}

// TestExpectedTapeRetentionIsPerMedium: a tape's old full stays protected — and the
// tape is not recycled — when the only newer full of that DLE lives on another
// medium (disk). Retention is per-medium: a copy elsewhere must never make the
// offsite tape reusable, or double storage would silently shed its redundancy.
func TestExpectedTapeRetentionIsPerMedium(t *testing.T) {
	eng := tapeEngine(t, false, "10d")
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)

	recordVol(t, eng, "lto-0001", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	recordFullOn(t, eng, "2026-06-01", "h", "lto-0001") // old full, only copy is on tape

	// A newer full of the same DLE exists, but only on disk — never on tape. Judged
	// globally this would supersede the tape's full and recycle lto-0001; judged
	// per-medium the tape still holds that DLE's last recovery path.
	recordFullOnOtherMedium(t, eng, "2026-06-20", "h", "disk")

	exp, ok := eng.ExpectedVolume(now)
	if !ok {
		t.Fatal("a labeled medium should yield an expectation")
	}
	if !exp.FreshVolume || exp.Label != "" {
		t.Fatalf("a disk copy must not recycle the tape; want a fresh tape, got %+v", exp)
	}
}

// TestExpectedTapeNeedsFresh: when every volume still holds a protected run, the
// run expects a fresh tape rather than recycling a protected one.
func TestExpectedTapeNeedsFresh(t *testing.T) {
	eng := tapeEngine(t, false, "10d")
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)

	recordVol(t, eng, "lto-0001", time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))
	recordFullOn(t, eng, "2026-06-20", "h", "lto-0001") // within minimum age -> protected

	exp, ok := eng.ExpectedVolume(now)
	if !ok {
		t.Fatal("a labeled medium should yield an expectation")
	}
	if !exp.FreshVolume || exp.Label != "" {
		t.Fatalf("want a fresh tape, got %+v", exp)
	}
}

// TestExpectedTapeAppendsToLatest: an appendable medium extends the most recently
// written volume rather than recycling an old one.
func TestExpectedTapeAppendsToLatest(t *testing.T) {
	eng := tapeEngine(t, true, "")
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)

	recordVol(t, eng, "lto-0001", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	recordVol(t, eng, "lto-0002", time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))

	exp, ok := eng.ExpectedVolume(now)
	if !ok {
		t.Fatal("a labeled medium should yield an expectation")
	}
	if exp.FreshVolume || exp.Label != "lto-0002" {
		t.Fatalf("want to append to latest lto-0002, got %+v", exp)
	}
}

// TestExpectedTapeReportsReelFill: an appendable run's expectation carries the
// landing reel's capacity (volume_size) and current fill, so a single run is
// bounded by the reel's remaining room (not the whole pool) and `nb plan` can
// show how full the tape is before it spills.
func TestExpectedTapeReportsReelFill(t *testing.T) {
	appendable := true
	cfg := &config.Config{
		Landing: "lto",
		Media: map[string]config.Media{
			"lto": {
				Type:       "tape",
				Appendable: &appendable,
				Params:     map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "2", "volume_size": "1000"},
			},
		},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)

	recordVol(t, eng, "lto-0001", time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))
	recordSizedFullOn(t, eng, "2026-06-20", "h", "lto-0001", 600)

	exp, ok := eng.ExpectedVolume(now)
	if !ok {
		t.Fatal("a labeled medium should yield an expectation")
	}
	if exp.Label != "lto-0001" {
		t.Fatalf("want append to lto-0001, got %+v", exp)
	}
	if exp.VolumeBytes != 1000 || exp.UsedBytes != 600 {
		t.Fatalf("want a 1000-byte reel with 600 used, got VolumeBytes=%d UsedBytes=%d", exp.VolumeBytes, exp.UsedBytes)
	}
}

// TestExpectedTapeDiskHasNone: an address-identified medium (disk) carries no
// label, so there is no tape to expect.
func TestExpectedTapeDiskHasNone(t *testing.T) {
	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := eng.ExpectedVolume(time.Now()); ok {
		t.Fatal("disk medium should not yield a tape expectation")
	}
}

func logfDiscard(string, ...any) {}

// labelOnMedium returns the (first) volume label a slot's copy on a medium occupies.
func labelOnMedium(t *testing.T, eng *Engine, slotID, medium string) string {
	t.Helper()
	for _, p := range eng.cat.Placements(slotID) {
		if p.Medium == medium {
			if ls := p.Labels(); len(ls) > 0 {
				return ls[0]
			}
		}
	}
	t.Fatalf("slot %s has no labeled copy on %q", slotID, medium)
	return ""
}

// TestTapeRecyclesOldestOnWrite: a non-appendable tape library whose pool is full
// (every bay holds a run) reuses the oldest Label the retention Floor clears on the
// next run — same Name, epoch+1 — rather than refusing. The recycled run's placement
// goes dead and, being that slot's only copy, the slot leaves the catalog. The run
// announces the Label it recycles, and a rebuild from the media reflects the new epoch
// only. This is the closed gap: hands-off whole-volume tape rotation (Amanda tapecycle).
func TestTapeRecyclesOldestOnWrite(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "v1")

	cfg := &config.Config{
		Landing:   "lib",
		AutoLabel: true,
		Cycle:     "1d", // full every run, so each run supersedes the prior — no live chain pins the old tape
		Media: map[string]config.Media{
			"lib": {Type: "tape", Appendable: boolp(false), Params: map[string]string{"dir": t.TempDir(), "bays": "2"}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	// Two runs fill the two-bay pool (one run per tape). Run dates sit well in the past
	// so the default one-cycle age floor never pins them — only last-recovery / chain.
	s1, err := eng.Run(context.Background(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	s2, err := eng.Run(context.Background(), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	l1 := labelOnMedium(t, eng, s1.ID, "lib")
	l2 := labelOnMedium(t, eng, s2.ID, "lib")
	if l1 == l2 {
		t.Fatalf("the two runs should land on distinct tapes, both %q", l1)
	}

	// Third run: no blank bay left. It must recycle the oldest Floor-cleared tape (l1,
	// superseded by s2's full) rather than refuse.
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	s3, err := eng.Run(context.Background(), time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC), logf)
	if err != nil {
		t.Fatalf("run 3 (should recycle %q): %v", l1, err)
	}

	// Reuse keeps the same Label name, advancing the epoch.
	if got := labelOnMedium(t, eng, s3.ID, "lib"); got != l1 {
		t.Fatalf("run 3 should reuse the oldest Label %q, landed on %q", l1, got)
	}
	if v, ok := eng.cat.Volume(l1); !ok || v.Label.Epoch != 2 {
		t.Fatalf("recycled volume %q should be at epoch 2, got %+v (ok=%v)", l1, v, ok)
	}
	// The recycled run's only copy was on l1; the slot leaves the catalog.
	if _, err := eng.cat.ReadSlot(s1.ID); err == nil {
		t.Fatalf("recycled slot %s should no longer be in the catalog", s1.ID)
	}
	// The run announced the Label it wanted.
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, l1) || !strings.Contains(joined, "recycl") {
		t.Fatalf("run should announce recycling %q; logs:\n%s", l1, joined)
	}

	// A rebuild from the media (source of truth) reflects the current epoch only: the
	// recycled tape physically holds s3 at epoch 2, and s1 is gone (its tape was wiped).
	freshCfg := *cfg
	freshCfg.Workdir = t.TempDir() // a fresh cache forces a scan of the media
	freshEng, err := New(&freshCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := freshEng.RebuildCatalog(logfDiscard); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if v, ok := freshEng.cat.Volume(l1); !ok || v.Label.Epoch != 2 {
		t.Fatalf("rebuilt volume %q should be epoch 2, got %+v (ok=%v)", l1, v, ok)
	}
	if _, err := freshEng.cat.ReadSlot(s1.ID); err == nil {
		t.Fatalf("rebuilt catalog should not contain the wiped slot %s", s1.ID)
	}
	if _, err := freshEng.cat.ReadSlot(s3.ID); err != nil {
		t.Fatalf("rebuilt catalog should contain the recycled-tape slot %s: %v", s3.ID, err)
	}
	if _, err := freshEng.cat.ReadSlot(s2.ID); err != nil {
		t.Fatalf("rebuilt catalog should still contain %s: %v", s2.ID, err)
	}
}

// TestTapeRecycleRefusedWhenAllKept: when every tape in a full pool still holds a
// protected run, a run needing a fresh tape fails loud rather than overwriting one —
// recoverability outranks capacity. Nothing is recorded.
func TestTapeRecycleRefusedWhenAllKept(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "v1")

	cfg := &config.Config{
		Landing:   "lib",
		AutoLabel: true,
		Cycle:     "30d", // incrementals between fulls — the first full stays a recovery base
		Media: map[string]config.Media{
			"lib": {Type: "tape", Appendable: boolp(false), MinimumAge: "365d", Params: map[string]string{"dir": t.TempDir(), "bays": "2"}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	// Both bays hold runs well within the 365d age floor — every tape is protected.
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	// Third run: no blank, nothing reusable — it must fail loud, never overwrite.
	_, err = eng.Run(context.Background(), time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC), nil)
	if err == nil {
		t.Fatal("expected a run with a full pool of protected tapes to fail, not overwrite")
	}
	if !strings.Contains(err.Error(), "within retention") {
		t.Fatalf("error should explain every tape is still within retention; got: %v", err)
	}
	if _, rerr := eng.cat.ReadSlot("slot-2026-06-03.001"); rerr == nil {
		t.Fatal("the refused run must not be recorded")
	}
}

// TestDumpSpanRecyclesReusableTape: a dump that spans more tapes than the pool has
// blanks rolls onto the oldest Floor-cleared Label (recycling it, ++epoch) once the
// blanks are exhausted — the deferred "whole-volume recycle on EOT".
func TestDumpSpanRecyclesReusableTape(t *testing.T) {
	src := t.TempDir()

	cfg := &config.Config{
		Landing:   "lib",
		AutoLabel: true,
		Cycle:     "1d", // full every run, so older fulls clear the Floor
		Media: map[string]config.Media{
			// Three bays, each a small reel: two fill with superseded runs, one starts blank.
			"lib": {Type: "tape", Appendable: boolp(false), Params: map[string]string{"dir": t.TempDir(), "bays": "3", "volume_size": "262144"}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	// Two small runs fill bay-01 and bay-02 (each a full superseding the last).
	write(t, filepath.Join(src, "f.txt"), "small-1")
	s1, err := eng.Run(context.Background(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	write(t, filepath.Join(src, "f.txt"), "small-2")
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	l1 := labelOnMedium(t, eng, s1.ID, "lib") // oldest, now Floor-cleared

	// A run too big for one reel: it starts on the one remaining blank bay, then — with
	// no blank left — recycles the oldest Floor-cleared tape (l1) to finish the span.
	write(t, filepath.Join(src, "big.txt"), strings.Repeat("x", 250*1024))
	s3, err := eng.Run(context.Background(), time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("spanning run should recycle %q to finish, got: %v", l1, err)
	}

	var tape catalog.Placement
	for _, p := range eng.cat.Placements(s3.ID) {
		if p.Medium == "lib" {
			tape = p
		}
	}
	labels := tape.Labels()
	if len(labels) < 2 {
		t.Fatalf("spanning run should cross >= 2 tapes, crossed %v", labels)
	}
	if !contains(labels, l1) {
		t.Fatalf("spanning run should roll onto the recycled tape %q, crossed %v", l1, labels)
	}
	if v, ok := eng.cat.Volume(l1); !ok || v.Label.Epoch != 2 {
		t.Fatalf("recycled span tape %q should be epoch 2, got %+v (ok=%v)", l1, v, ok)
	}
	// Verify reassembles the spanned archive across the blank and recycled tapes.
	if rep, err := eng.Verify([]string{s3.ID}, VerifyOptions{}, nil); err != nil || rep.Failures != 0 {
		t.Fatalf("verify spanned-onto-recycled: failures=%d err=%v", rep.Failures, err)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestDumpSpansArchiveAcrossTapes dumps a source larger than one tape directly onto
// a tape library, so a single DLE's archive must split into parts across several
// auto-labeled bays. It then verifies the slot and restores it, exercising the read
// path mounting each bay in sequence to reassemble the spanned archive.
func TestDumpSpansArchiveAcrossTapes(t *testing.T) {
	src := t.TempDir()
	// ~150 KiB in one file → one archive larger than a single 160 KiB tape (each tape
	// also spends a 32 KiB header on its label and on each part), so it must span.
	body := strings.Repeat("nbackup-spanning-", 9*1024)
	write(t, filepath.Join(src, "big.txt"), body)

	cfg := &config.Config{
		Landing:   "lib",
		AutoLabel: true,
		Media: map[string]config.Media{
			"lib": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "6", "volume_size": "163840"}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	// Seed a blank bay in the drive so the run has somewhere to start; it auto-labels
	// and rolls onto the rest as each fills.
	if err := eng.LoadVolume("lib", "bay-01", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}

	s, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}

	// The single archive must have split into several parts.
	if len(s.Archives) != 1 {
		t.Fatalf("want 1 archive, got %d", len(s.Archives))
	}
	if s.Archives[0].Parts < 2 {
		t.Fatalf("archive Parts = %d, want >= 2 (the dump must span tapes)", s.Archives[0].Parts)
	}
	// The copy must span more than one volume.
	ps := eng.cat.Placements(s.ID)
	if len(ps) != 1 {
		t.Fatalf("placements = %d, want 1", len(ps))
	}
	if vols := ps[0].Labels(); len(vols) < 2 {
		t.Fatalf("placement spans %v, want >= 2 volumes", vols)
	}

	// Verify reassembles and re-hashes the spanned archive across its tapes.
	if rep, err := eng.Verify([]string{s.ID}, VerifyOptions{}, nil); err != nil || rep.Failures != 0 {
		t.Fatalf("verify: failures=%d err=%v", rep.Failures, err)
	}

	// Restore must mount each bay in sequence to rebuild the original file.
	dest := t.TempDir()
	name := config.DLE{Host: "localhost", Path: src}.Name()
	if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore from spanned tapes: %v", err)
	}
	assertContent(t, filepath.Join(dest, "big.txt"), body)
}

// TestCopySpansArchiveAcrossTapes dumps one big archive to disk, then copies it to a
// small-tape library where the single archive must split across bays (re-splitting
// the already-compressed payload, not recompressing). It drops the disk copy and
// restores from the spanned tapes.
func TestCopySpansArchiveAcrossTapes(t *testing.T) {
	src := t.TempDir()
	body := strings.Repeat("copy-spanning-payload-", 7*1024)
	write(t, filepath.Join(src, "big.txt"), body)

	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true,
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lib":  {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "6", "volume_size": "163840"}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := eng.LoadVolume("lib", "bay-01", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}
	if err := eng.CopySlot(s.ID, "", "lib", false, nil); err != nil {
		t.Fatalf("copy disk->lib: %v", err)
	}

	var tape catalog.Placement
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "lib" {
			tape = p
		}
	}
	if parts, _ := tape.Parts(config.DLE{Host: "localhost", Path: src}.Name(), 0); len(parts) < 2 {
		t.Fatalf("copied archive parts = %d, want >= 2 (must span)", len(parts))
	}

	// Drop the disk copy so restore must read the spanned tape copy.
	removeSlotFiles(t, eng, s.ID)
	if _, err := eng.cat.RemovePlacement(s.ID, "disk"); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	name := config.DLE{Host: "localhost", Path: src}.Name()
	if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore from spanned tape copy: %v", err)
	}
	assertContent(t, filepath.Join(dest, "big.txt"), body)
}

// TestPartSizeSplitsWithinTape sets a small part_size on a roomy tape: the archive is
// chopped into several parts that all stay on the one tape (intra-volume splitting —
// the real-drive path where capacity is bounded by part_size, not a bay size). It
// must still verify and restore.
func TestPartSizeSplitsWithinTape(t *testing.T) {
	src := t.TempDir()
	body := strings.Repeat("part-size-", 12*1024) // ~120 KiB
	write(t, filepath.Join(src, "big.txt"), body)

	cfg := &config.Config{
		Landing:   "lib",
		AutoLabel: true,
		Media: map[string]config.Media{
			// One roomy 4 MiB bay, but part_size caps each part at 64 KiB.
			"lib": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "1", "volume_size": "4194304", "part_size": "65536"}},
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	if err := eng.LoadVolume("lib", "bay-01", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if s.Archives[0].Parts < 2 {
		t.Fatalf("archive Parts = %d, want >= 2 (part_size must split it)", s.Archives[0].Parts)
	}
	if vols := eng.cat.Placements(s.ID)[0].Labels(); len(vols) != 1 {
		t.Fatalf("parts should stay on one tape, got volumes %v", vols)
	}
	if rep, err := eng.Verify([]string{s.ID}, VerifyOptions{}, nil); err != nil || rep.Failures != 0 {
		t.Fatalf("verify: failures=%d err=%v", rep.Failures, err)
	}
	dest := t.TempDir()
	if err := eng.Restore(s.ID, config.DLE{Host: "localhost", Path: src}.Name(), dest, false, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertContent(t, filepath.Join(dest, "big.txt"), body)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pruneSweepEngine wires a disk-landing engine over a fresh source + catalog and runs a
// level-0 dump, returning the engine, the committed slot, the disk root, and the live
// source dir. Shared by the orphan-sweep tests below.
func pruneSweepEngine(t *testing.T) (*Engine, *catalog.Slot, string, string) {
	t.Helper()
	src := t.TempDir()
	catalogDir := t.TempDir()
	write(t, filepath.Join(src, "keep.txt"), "v1")

	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": catalogDir}}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s1, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return eng, s1, catalogDir, src
}

// TestPruneSweepsCrashOrphans exercises the full sweep through nb prune: a footer-less
// archive part and a torn append (both crash leftovers) are reported by a dry-run and
// removed by an apply, while the committed backup is left fully restorable.
func TestPruneSweepsCrashOrphans(t *testing.T) {
	eng, s1, catalogDir, src := pruneSweepEngine(t)

	// (a) a footer-less archive part: committed at the file layer (payload + .hdr) but no
	// commit footer, so no archive references it.
	vol, err := media.OpenVolume("disk", media.Options{"path": catalogDir})
	if err != nil {
		t.Fatal(err)
	}
	fw, err := vol.AppendFile(context.Background(),
		record.Header{Slot: "slot-2026-06-20.001", Kind: record.KindArchive, DLE: "ghost", Compress: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("orphan payload")); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	// (b) a torn append: a payload object at a conforming position with no .hdr sidecar.
	tornDir := filepath.Join(catalogDir, "slots", "slot-2026-06-19.001")
	if err := os.MkdirAll(tornDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tornFile := filepath.Join(tornDir, "990-torn.tar")
	if err := os.WriteFile(tornFile, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Prune through a fresh engine: like a real `nb prune` process it scans the medium
	// anew, so it sees the leftovers injected after the dump engine cached its volume.
	eng2, err := New(eng.cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	if _, swept, _, err := eng2.Prune("disk", now, false, nil); err != nil {
		t.Fatalf("dry-run prune: %v", err)
	} else if swept != 2 {
		t.Fatalf("dry-run swept=%d, want 2 (footer-less part + torn file)", swept)
	}

	_, swept, _, err := eng2.Prune("disk", now, true, nil)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if swept != 2 {
		t.Fatalf("swept=%d, want 2", swept)
	}
	if _, err := os.Stat(tornFile); !os.IsNotExist(err) {
		t.Errorf("torn file survived the sweep, stat err = %v", err)
	}

	// The committed backup is untouched and still restores.
	dest := t.TempDir()
	name := config.DLE{Host: "localhost", Path: src}.Name()
	if err := eng2.Restore(s1.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore after sweep: %v", err)
	}
	assertContent(t, filepath.Join(dest, "keep.txt"), "v1")
}

// TestPruneSweepKeepsCommittedWhenCacheLost is the safety regression: even with the
// catalog cache wiped (the danger that broke a cache-based design — a stale cache makes
// every committed file look unreferenced), an apply-prune sweeps nothing and the backup
// survives, because orphan detection reads the medium's own commit footers, not the cache.
func TestPruneSweepKeepsCommittedWhenCacheLost(t *testing.T) {
	eng, s1, _, src := pruneSweepEngine(t)

	// Lose the cache, then prune through a fresh engine that starts with an empty catalog.
	if err := os.Remove(filepath.Join(eng.cfg.WorkdirPath(), catalog.CacheFile)); err != nil {
		t.Fatal(err)
	}
	eng2, err := New(eng.cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	_, swept, _, err := eng2.Prune("disk", now, true, nil)
	if err != nil {
		t.Fatalf("prune with lost cache: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept=%d, want 0 — a committed archive must never be swept on a lost cache", swept)
	}

	// The committed backup still restores through the fresh engine.
	dest := t.TempDir()
	name := config.DLE{Host: "localhost", Path: src}.Name()
	if err := eng2.Restore(s1.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore after lost-cache prune: %v", err)
	}
	assertContent(t, filepath.Join(dest, "keep.txt"), "v1")
}

// placedOnLanding reports whether the slot has a placement for dle on the engine's landing medium.
func placedOnLanding(e *Engine, slotID, dle string) bool {
	for _, p := range e.cat.Placements(slotID) {
		if p.Medium != e.mediumName {
			continue
		}
		for _, a := range p.Archives {
			if a.DLE == dle {
				return true
			}
		}
	}
	return false
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}
