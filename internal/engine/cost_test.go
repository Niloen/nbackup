package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// cloudCostEngine lands one DLE on a file://-backed cloud medium (no network), with
// an optional cost override, and returns the engine after a single dump. It skips
// when GNU tar is unavailable, like the other cloud round-trip tests.
func cloudCostEngine(t *testing.T, runDate time.Time, costCfg *config.CostConfig) (*Engine, string) {
	t.Helper()
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "price me")

	cfg := &config.Config{
		Landing: "cloud",
		Media: map[string]config.Media{
			"cloud": {Type: "cloud", Cost: costCfg, Params: map[string]string{"url": "file://" + t.TempDir()}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	if _, err := eng.Run(runDate, nil); err != nil {
		t.Fatalf("dump: %v", err)
	}
	return eng, config.DLE{Host: "h", Path: src}.Name()
}

// TestCostSummaryCloud prices the footprint and the marginal next run on a cloud
// medium: a file:// bucket infers the generic-cloud table, so it is priced.
func TestCostSummaryCloud(t *testing.T) {
	now := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	eng, _ := cloudCostEngine(t, now, nil)

	cs := eng.CostSummary(eng.Plan(now))
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
		Landing: "disk",
		Media:   map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"
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

// TestForecastCostGrows projects the cost curve forward; with unbounded capacity the
// footprint and its monthly cost only grow as runs land.
func TestForecastCostGrows(t *testing.T) {
	start := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	eng, _ := cloudCostEngine(t, start, nil)

	curve := eng.ForecastCost(start.AddDate(0, 0, 1), 5)
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
