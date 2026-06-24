package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// TestCloudLandingRoundTrip drives the full engine against a cloud medium (the
// file:// gocloud driver, so no network or credentials): land a slot, verify it
// (reads every archive back and re-hashes), and restore it. This exercises the
// cloud Volume's AppendFile/ReadFile/Files through dump → verify → restore exactly
// as a real S3/GCS bucket would, proving the medium plugs into the engine like any
// other address-identified store.
func TestCloudLandingRoundTrip(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "land me in the cloud")

	cfg := &config.Config{
		Landing: "cloud",
		Media: map[string]config.Media{
			"cloud": {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
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

	s, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump to cloud: %v", err)
	}

	if failures, err := eng.Verify([]string{s.ID}, nil); err != nil || failures != 0 {
		t.Fatalf("verify from cloud: failures=%d err=%v", failures, err)
	}

	dest := t.TempDir()
	name := config.DLE{Host: "h", Path: src}.Name()
	if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore from cloud: %v", err)
	}
	assertContent(t, filepath.Join(dest, "f.txt"), "land me in the cloud")
}

// TestSyncDiskToCloud lands on disk and mirrors offsite to a cloud medium, the
// canonical "land fast, replicate offsite" flow. It asserts the copy is recorded
// as a second placement and that a re-sync is idempotent.
func TestSyncDiskToCloud(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "offsite me")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":  {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"cloud": {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
		},
		Sync:    []config.SyncRule{{To: "cloud"}},
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

	s, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}

	report, err := eng.SyncTo("", "cloud", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("sync apply: %v", err)
	}
	if report.Copied() != 1 {
		t.Fatalf("copied = %d, want 1", report.Copied())
	}
	if !eng.placedOn(s.ID, "cloud") {
		t.Fatalf("slot %s not on cloud after sync", s.ID)
	}
	if got := len(eng.cat.Placements(s.ID)); got != 2 {
		t.Fatalf("slot %s placements = %d, want 2 (disk + cloud)", s.ID, got)
	}

	// Re-sync is a no-op: the slot already exists on the cloud target.
	report, err = eng.SyncTo("", "cloud", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if report.Copied() != 0 {
		t.Fatalf("re-sync copied = %d, want 0 (idempotent)", report.Copied())
	}
}
