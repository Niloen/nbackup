package cli

import (
	"context"
	"os"
	"path/filepath"
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
		Landing: config.MediumList{"disk"},
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	websrc, err := newEngineSource(cfg, "", nil)
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

// TestWebSourceSeesConfigEditedAfterStartup pins the other half of the freshness
// contract: an operator editing the config file — here a medium's capacity — must
// see it in `nb web` without a restart. The source watches the config file and
// reloads the whole config, rebuilding its engine from the fresh values.
func TestWebSourceSeesConfigEditedAfterStartup(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nbackup.yaml")
	write := func(capacity string) {
		body := "" +
			"workdir: " + filepath.Join(dir, "work") + "\n" +
			"state_dir: " + filepath.Join(dir, "state") + "\n" +
			"compress:\n  scheme: none\n" +
			"landing: disk\n" +
			"media:\n" +
			"  disk:\n" +
			"    type: disk\n" +
			"    capacity: " + capacity + "\n" +
			"    path: " + filepath.Join(dir, "vault") + "\n" +
			"sources:\n" +
			"  default:\n" +
			"    localhost: [" + t.TempDir() + "]\n"
		if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("1G")

	load := func() (*config.Config, error) { return loadConfigOrDefaultCatalog(cfgPath, "") }
	cfg, err := load()
	if err != nil {
		t.Fatal(err)
	}
	websrc, err := newEngineSource(cfg, cfgPath, load)
	if err != nil {
		t.Fatal(err)
	}
	capacityOf := func() int64 {
		for _, m := range websrc.Media() {
			if m.Name == "disk" {
				return m.Capacity
			}
		}
		t.Fatal("disk medium missing")
		return 0
	}
	if got := capacityOf(); got != 1_000_000_000 {
		t.Fatalf("initial capacity = %d, want 1000000000", got)
	}

	// The operator bumps the capacity. A stat-stamp change needs a distinct mtime;
	// tests can't sleep on the wall clock here, so backdate the first write instead
	// and let the rewrite land at "now".
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(cfgPath, old, old); err != nil {
		t.Fatal(err)
	}
	websrc.cfgStamp = statFile(cfgPath) // re-sync to the backdated stamp
	write("2G")

	if got := capacityOf(); got != 2_000_000_000 {
		t.Fatalf("capacity after config edit = %d, want 2000000000 (config reload not picked up)", got)
	}
}
