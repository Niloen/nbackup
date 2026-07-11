package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/recovery"
)

// cloudCostEngine lands one DLE on a file://-backed cloud medium (no network), with
// an optional cost override, and returns the engine after a single dump. It skips
// when GNU tar is unavailable, like the other cloud round-trip tests.
func cloudCostEngine(t *testing.T, runDate time.Time, costCfg *config.CostConfig) (*Engine, string) {
	t.Helper()
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "price me")

	cfg := &config.Config{
		Landing: config.MediumList{"cloud"},
		Media: map[string]config.Media{
			"cloud": {Type: "cloud", Cost: costCfg, Params: map[string]string{"url": "file://" + t.TempDir()}},
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	if _, err := eng.Run(context.Background(), runDate, nil); err != nil {
		t.Fatalf("dump: %v", err)
	}
	return eng, config.DLE{Host: "localhost", Path: src}.Name()
}

// TestCostSummaryCloud prices the footprint and the marginal next run on a cloud
// medium: a file:// bucket infers the generic-cloud table, so it is priced.
func TestCostSummaryCloud(t *testing.T) {
	now := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	eng, _ := cloudCostEngine(t, now, nil)

	plan, err := eng.Plan(now)
	if err != nil {
		t.Fatal(err)
	}
	cs := eng.CostSummary(plan)
	if !cs.Priced {
		t.Fatal("cloud medium should be priced")
	}
	if cs.Provider != "generic-cloud" {
		t.Errorf("provider = %q, want generic-cloud (inferred from file://)", cs.Provider)
	}
	if cs.Bytes <= 0 || cs.Monthly <= 0 {
		t.Errorf("footprint should cost something: bytes=%d monthly=%v", cs.Bytes, cs.Monthly)
	}
	if cs.RunBytes <= 0 || cs.Marginal <= 0 {
		t.Errorf("a planned run should have a marginal cost: runBytes=%d marginal=%v", cs.RunBytes, cs.Marginal)
	}
}

// TestCostSummaryDiskUnpriced verifies a local disk has no recurring bill, so the
// CLI suppresses its cost block.
func TestCostSummaryDiskUnpriced(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "local")
	cfg := &config.Config{
		Landing:  config.MediumList{"disk"},
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if eng.CostSummary(nil).Priced {
		t.Error("disk medium should be unpriced")
	}
}

// TestRestoreCostEgress prices a restore as the egress of the chain it would read.
func TestRestoreCostEgress(t *testing.T) {
	runDate := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	eng, dle := cloudCostEngine(t, runDate, nil)

	est := eng.RestoreCost([]string{dle}, "2026-06-21")
	if !est.Priced || est.Bytes <= 0 {
		t.Fatalf("restore should be priced with bytes: %+v", est)
	}
	if est.Cost <= 0 {
		t.Errorf("a cloud restore should cost egress: %v", est.Cost)
	}
}

// TestCostOverride checks that a per-medium cost block overrides the inferred
// provider and rates: forcing aws-s3 with a steep egress rate raises the read cost.
func TestCostOverride(t *testing.T) {
	runDate := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	egress := 0.50
	eng, dle := cloudCostEngine(t, runDate, &config.CostConfig{Provider: "aws-s3", EgressPerGB: &egress})

	est := eng.RestoreCost([]string{dle}, "2026-06-21")
	if est.Provider != "aws-s3" {
		t.Errorf("provider = %q, want aws-s3 (overridden)", est.Provider)
	}
	// Egress alone is bytes/GiB * 0.50; the GET requests add a hair on top.
	wantEgress := float64(est.Bytes) / (1 << 30) * egress
	if est.Cost < wantEgress {
		t.Errorf("cost %v should be at least the overridden egress %v", est.Cost, wantEgress)
	}
}

// TestSelectionCostEgress exercises SelectionPlan: a file-level recovery is priced as
// the egress of the archives its selected members are extracted from, and its per-archive
// plan row carries the same read for the EXPLAIN display.
func TestSelectionCostEgress(t *testing.T) {
	runDate := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	eng, dle := cloudCostEngine(t, runDate, nil)

	run := eng.cat.Runs()[0]
	a := run.Archives[0]
	steps := []recovery.ExtractStep{
		{Step: recovery.Step{RunID: run.ID, DLE: dle, Level: a.Level}, Members: []string{"f.txt"}},
	}

	rows, est := eng.SelectionPlan(steps)
	if !est.Priced {
		t.Fatalf("a cloud selection recovery should be priced: %+v", est)
	}
	if est.Bytes <= 0 {
		t.Errorf("selection cost should carry the archive's egress bytes: %+v", est)
	}
	if est.Cost <= 0 {
		t.Errorf("a cloud selection recovery should cost egress: %v", est.Cost)
	}
	if len(rows) != 1 {
		t.Fatalf("want one plan row for one archive, got %d", len(rows))
	}
	if r := rows[0]; r.Files != 1 || r.Read <= 0 || !r.Priced || r.Whole <= 0 {
		t.Errorf("plan row should describe the read: %+v", r)
	}
}

// TestForecastCostReclaims exercises the forecast's per-archive reclamation (dropArchive):
// a small-capacity, daily-full landing must, over the projected days, reclaim superseded
// fulls once they age past the retention floor — so at least one point reports reclaimed
// bytes rather than a monotonically growing footprint.
func TestForecastCostReclaims(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), strings.Repeat("forecast-reclaim-", 512)) // a non-trivial full

	cfg := &config.Config{
		Landing: config.MediumList{"cloud"},
		Media: map[string]config.Media{
			// Capacity fits one run (make-room refuses a run that could NEVER fit)
			// but not the forecast's accumulating dailies, and a short floor lets
			// superseded fulls be reclaimed rather than pinned.
			"cloud": {Type: "cloud", Capacity: "30000", MinimumAge: "1s", Params: map[string]string{"url": "file://" + t.TempDir()}},
		},
		Cycle:    "1d", // every simulated run is a fresh full, so older fulls become reclaimable
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	start := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(context.Background(), start, nil); err != nil {
		t.Fatalf("dump: %v", err)
	}

	plans, err := eng.Simulate(start.AddDate(0, 0, 1), 6)
	if err != nil {
		t.Fatal(err)
	}
	curve := eng.ForecastCost(start.AddDate(0, 0, 1), plans)
	if len(curve) != 6 {
		t.Fatalf("curve has %d points, want 6", len(curve))
	}
	reclaimedSomewhere := false
	for _, p := range curve {
		if p.Reclaimed > 0 {
			reclaimedSomewhere = true
		}
	}
	if !reclaimedSomewhere {
		t.Fatalf("a capacity-bounded daily-full forecast should reclaim superseded fulls; curve=%+v", curve)
	}
	// A bounded landing carries its capacity on every point (the capacity-headroom signal).
	for _, p := range curve {
		if p.Capacity <= 0 {
			t.Fatalf("bounded forecast point missing Capacity: %+v", p)
		}
	}
}

// TestForecastCostGrows projects the cost curve forward; with unbounded capacity the
// footprint and its monthly cost only grow as runs land.
func TestForecastCostGrows(t *testing.T) {
	start := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	eng, _ := cloudCostEngine(t, start, nil)

	plans, err := eng.Simulate(start.AddDate(0, 0, 1), 5)
	if err != nil {
		t.Fatal(err)
	}
	curve := eng.ForecastCost(start.AddDate(0, 0, 1), plans)
	if len(curve) != 5 {
		t.Fatalf("curve has %d points, want 5", len(curve))
	}
	for _, p := range curve {
		if p.Monthly < 0 || p.Bytes < 0 {
			t.Errorf("nonsensical point %+v", p)
		}
	}
	if !(curve[len(curve)-1].Bytes >= curve[0].Bytes) {
		t.Errorf("unbounded footprint should not shrink: first=%d last=%d", curve[0].Bytes, curve[len(curve)-1].Bytes)
	}
}

// TestForecastCapacityRoutesPerMedium checks that the per-medium forecast bills each
// DLE's projected archives to the medium its dumptype lands on — not everything to the
// landing. A "big" dumptype routes to a second medium; only its DLE should show up there.
func TestForecastCapacityRoutesPerMedium(t *testing.T) {
	srcA := t.TempDir()
	srcB := t.TempDir()
	write(t, filepath.Join(srcA, "a.txt"), strings.Repeat("home-", 400))
	write(t, filepath.Join(srcB, "b.txt"), strings.Repeat("bulk-", 400))

	cfg := &config.Config{
		Landing: config.MediumList{"cloud"},
		Media: map[string]config.Media{
			"cloud":  {Type: "cloud", Capacity: "50000", MinimumAge: "1s", Params: map[string]string{"url": "file://" + t.TempDir()}},
			"cloud2": {Type: "cloud", Capacity: "50000", MinimumAge: "1s", Params: map[string]string{"url": "file://" + t.TempDir()}},
		},
		DumpTypes: map[string]config.DumpType{
			"bulk": {Landing: config.MediumList{"cloud2"}}, // routes away from the landing
		},
		Cycle: "1d",
		Sources: []config.DLE{
			{Host: "localhost", Path: srcA},                   // default dumptype -> cloud
			{Host: "localhost", Path: srcB, DumpType: "bulk"}, // -> cloud2
		},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	start := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(context.Background(), start, nil); err != nil {
		t.Fatalf("dump: %v", err)
	}

	forecasts, err := eng.ForecastCapacityOffline(start.AddDate(0, 0, 1), 4)
	if err != nil {
		t.Fatal(err)
	}
	byMedium := map[string][]ForecastPoint{}
	for _, mf := range forecasts {
		if mf.VolumeStructured {
			t.Errorf("cloud medium %q wrongly flagged volume-structured", mf.Medium)
		}
		byMedium[mf.Medium] = mf.Points
	}
	// Both media are landing routes, so both are forecast, and each is non-empty.
	for _, name := range []string{"cloud", "cloud2"} {
		if len(byMedium[name]) == 0 {
			t.Fatalf("medium %q missing from per-medium forecast: %+v", name, forecasts)
		}
		if byMedium[name][len(byMedium[name])-1].Bytes <= 0 {
			t.Errorf("medium %q forecast has no footprint — its routed DLE was not billed to it", name)
		}
	}

	// The per-DLE footprint of the bulk DLE (routed to cloud2) carries its own retained
	// bytes forward — the per-DLE peer of the per-medium forecast.
	bulkSlug := config.DLE{Host: "localhost", Path: srcB}.Name()
	foot, ferr := eng.ForecastDLEFootprintOffline(bulkSlug, start.AddDate(0, 0, 1), 4)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if len(foot) == 0 || foot[len(foot)-1].Bytes <= 0 {
		t.Errorf("bulk DLE %q should have a projected footprint: %+v", bulkSlug, foot)
	}
}
