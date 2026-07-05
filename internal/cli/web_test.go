package cli

import (
	"context"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
)

// TestWebSourceSeesCatalogWrittenAfterStartup pins the freshness contract of `nb
// web`: it is a long-running reader beside cron-driven writer processes, so a run
// dumped *after* the server started must appear without a restart. The source must
// notice catalog.json changing on disk and rebuild its engine.
func TestWebSourceSeesCatalogWrittenAfterStartup(t *testing.T) {
	src := t.TempDir()
	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	websrc, err := newEngineSource(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(websrc.Runs()); n != 0 {
		t.Fatalf("empty catalog shows %d runs", n)
	}

	// A separate engine plays the cron `nb dump` process, rewriting catalog.json
	// behind the web server's back.
	writer, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	run, err := writer.Run(context.Background(), time.Date(2026, 7, 5, 2, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}

	runs := websrc.Runs()
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("web source did not pick up the new catalog: got %v, want [%s]", runs, run.ID)
	}
}
