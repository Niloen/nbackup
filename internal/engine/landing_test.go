package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// TestPerDumptypeLandingRoutes drives one run with two DLEs whose dumptypes route them to
// different landing media (the config-wide default vs a dumptype `landing:` override), then asserts
// each DLE's archive landed on its own medium — one run, two placements, disjoint archive sets —
// and that both restore. This is the per-DLE landing feature: heterogeneous sources to heterogeneous
// media within a single run, recorded as the medium-independent run the catalog already models.
func TestPerDumptypeLandingRoutes(t *testing.T) {
	srcMain := t.TempDir()
	write(t, filepath.Join(srcMain, "main.txt"), "lands on main")
	srcBulk := t.TempDir()
	write(t, filepath.Join(srcBulk, "bulk.txt"), "lands on bulk")

	cfg := &config.Config{
		Landing: "main",
		Media: map[string]config.Media{
			"main": {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
			"bulk": {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
		},
		DumpTypes: map[string]config.DumpType{
			"bulky": {Landing: "bulk"},
		},
		Sources: []config.DLE{
			{Host: "localhost", Path: srcMain},                    // default dumptype -> main
			{Host: "localhost", Path: srcBulk, DumpType: "bulky"}, // -> bulk
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

	s, err := eng.Run(context.Background(), time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}

	mainDLE := config.DLE{Host: "localhost", Path: srcMain}.Name()
	bulkDLE := config.DLE{Host: "localhost", Path: srcBulk}.Name()

	// One run, a placement per medium, each holding only its routed DLE.
	got := map[string][]string{} // medium -> DLEs landed there
	for _, p := range eng.Catalog().Placements(s.ID) {
		for _, a := range p.Archives {
			got[p.Medium] = append(got[p.Medium], a.DLE)
		}
	}
	if len(got["main"]) != 1 || got["main"][0] != mainDLE {
		t.Fatalf("medium main: got %v, want [%s]", got["main"], mainDLE)
	}
	if len(got["bulk"]) != 1 || got["bulk"][0] != bulkDLE {
		t.Fatalf("medium bulk: got %v, want [%s]", got["bulk"], bulkDLE)
	}

	// Both restore from their respective media.
	destMain := t.TempDir()
	if err := eng.Restore(s.ID, mainDLE, destMain, false, nil); err != nil {
		t.Fatalf("restore main: %v", err)
	}
	assertContent(t, filepath.Join(destMain, "main.txt"), "lands on main")
	destBulk := t.TempDir()
	if err := eng.Restore(s.ID, bulkDLE, destBulk, false, nil); err != nil {
		t.Fatalf("restore bulk: %v", err)
	}
	assertContent(t, filepath.Join(destBulk, "bulk.txt"), "lands on bulk")
}

// TestFlushRoutesPerDumptypeLanding proves crash-recovery flush drains each staged archive back to
// its own landing: it stages two DLEs onto a scratch disk (the state a crashed holding-disk run
// leaves), then flushes with the scratch disk reconfigured as holding for two real landings whose
// dumptype routing sends one DLE to each. A flush that ignored routing would pile both onto one medium.
func TestFlushRoutesPerDumptypeLanding(t *testing.T) {
	srcMain := t.TempDir()
	write(t, filepath.Join(srcMain, "main.txt"), "main, stranded")
	srcBulk := t.TempDir()
	write(t, filepath.Join(srcBulk, "bulk.txt"), "bulk, stranded")
	scratchDir := t.TempDir()
	workdir := t.TempDir()
	stateDir := t.TempDir()
	sources := []config.DLE{
		{Host: "localhost", Path: srcMain},
		{Host: "localhost", Path: srcBulk, DumpType: "bulky"},
	}

	// Stage: dump both onto the scratch disk as a landing — a holding-disk run stages every DLE to the
	// disk regardless of its eventual landing, so the crash leaves both on scratch. `bulky` carries no
	// landing here (it resolves to the scratch landing).
	stageCfg := &config.Config{
		Landing:   "scratch",
		Media:     map[string]config.Media{"scratch": {Type: "disk", Params: map[string]string{"path": scratchDir}}},
		DumpTypes: map[string]config.DumpType{"bulky": {}},
		Sources:   sources,
		Workdir:   workdir,
		StateDir:  stateDir,
	}
	stageCfg.Compress.Scheme = "none"
	stageEng, err := New(stageCfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := stageEng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := stageEng.Run(context.Background(), time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("stage dump: %v", err)
	}

	// Flush: scratch is now a holding disk; `bulky` routes to the `bulk` landing, the default to `main`.
	flushCfg := &config.Config{
		Landing: "main",
		Media: map[string]config.Media{
			"main":    {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
			"bulk":    {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
			"scratch": {Type: "disk", Holding: true, Params: map[string]string{"path": scratchDir}},
		},
		DumpTypes: map[string]config.DumpType{"bulky": {Landing: "bulk"}},
		Sources:   sources,
		Workdir:   workdir,
		StateDir:  stateDir,
	}
	flushCfg.Compress.Scheme = "none"
	flushEng, err := New(flushCfg)
	if err != nil {
		t.Fatal(err)
	}
	n, err := flushEng.Flush(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC), logfDiscard)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 2 {
		t.Fatalf("flush moved %d archives, want 2", n)
	}

	got := map[string][]string{}
	for _, p := range flushEng.Catalog().Placements(s.ID) {
		for _, a := range p.Archives {
			got[p.Medium] = append(got[p.Medium], a.DLE)
		}
	}
	mainDLE := config.DLE{Host: "localhost", Path: srcMain}.Name()
	bulkDLE := config.DLE{Host: "localhost", Path: srcBulk}.Name()
	if len(got["main"]) != 1 || got["main"][0] != mainDLE {
		t.Errorf("medium main: got %v, want [%s]", got["main"], mainDLE)
	}
	if len(got["bulk"]) != 1 || got["bulk"][0] != bulkDLE {
		t.Errorf("medium bulk: got %v, want [%s]", got["bulk"], bulkDLE)
	}
	// The holding disk is empty after the flush drained both.
	scratchVol, _, _, err := flushEng.dep.mediumVolume("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := scratchVol.Files(); len(files) != 0 {
		t.Errorf("holding disk must be empty after flush, has %d file(s)", len(files))
	}
}

// TestPerDumptypeLandingValidation rejects a dumptype whose `landing` names an undefined medium.
func TestPerDumptypeLandingValidation(t *testing.T) {
	cfg := &config.Config{
		Landing: "main",
		Media: map[string]config.Media{
			"main": {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
		},
		DumpTypes: map[string]config.DumpType{
			"bulky": {Landing: "nope"},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for undefined dumptype landing")
	}
}
