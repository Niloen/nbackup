package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/record"
)

// TestCloudLandingRoundTrip drives the full engine against a cloud medium (the
// file:// gocloud driver, so no network or credentials): land a run, verify it
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
		t.Fatalf("dump to cloud: %v", err)
	}

	if rep, err := eng.Verify([]string{s.ID}, VerifyOptions{}, nil); err != nil || rep.Failures != 0 {
		t.Fatalf("verify from cloud: failures=%d err=%v", rep.Failures, err)
	}

	dest := t.TempDir()
	name := config.DLE{Host: "localhost", Path: src}.Name()
	if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore from cloud: %v", err)
	}
	assertContent(t, filepath.Join(dest, "f.txt"), "land me in the cloud")
}

// TestCloudPartSizeSplitsAndRestores sets a small part_size on a cloud medium so a
// large archive is chopped into several part-objects (the fix for the 10000-part
// multipart-upload ceiling). It proves the parts all land on the one logical volume,
// then verify (multi-part reassembly + re-hash) and restore both reconstruct the
// archive from the concatenated parts.
func TestCloudPartSizeSplitsAndRestores(t *testing.T) {
	src := t.TempDir()
	body := strings.Repeat("cloud-part-size-", 12*1024) // ~192 KiB, several 64 KiB parts
	write(t, filepath.Join(src, "big.txt"), body)

	cfg := &config.Config{
		Landing: "cloud",
		Media: map[string]config.Media{
			// part_size 64 KiB (the 2-header minimum) forces an intra-volume split.
			"cloud": {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir(), "part_size": "65536"}},
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
		t.Fatalf("dump to cloud: %v", err)
	}
	if s.Archives[0].Parts < 2 {
		t.Fatalf("archive Parts = %d, want >= 2 (part_size must split it)", s.Archives[0].Parts)
	}
	if rep, err := eng.Verify([]string{s.ID}, VerifyOptions{}, nil); err != nil || rep.Failures != 0 {
		t.Fatalf("verify from cloud: failures=%d err=%v", rep.Failures, err)
	}
	dest := t.TempDir()
	name := config.DLE{Host: "localhost", Path: src}.Name()
	if err := eng.Restore(s.ID, name, dest, false, nil); err != nil {
		t.Fatalf("restore from cloud: %v", err)
	}
	assertContent(t, filepath.Join(dest, "big.txt"), body)
}

// TestCloudPartSizeStaysConcurrent proves enabling part_size on a cloud medium does NOT
// clamp it to a single serial drive: a concurrent-write object store is not Serial, so the
// conductor keeps every worker and never logs the single-drive clamp — splitting an archive
// into parts is irrelevant to its parallelism. It runs several DLEs through multiple workers
// under -race to exercise concurrent part-split landing writes.
func TestCloudPartSizeStaysConcurrent(t *testing.T) {
	var dles []config.DLE
	for i := 0; i < 4; i++ {
		src := t.TempDir()
		write(t, filepath.Join(src, "f.txt"), strings.Repeat(fmt.Sprintf("dle-%d-", i), 20*1024)) // ~140 KiB each
		dles = append(dles, config.DLE{Host: "localhost", Path: src})
	}

	cfg := &config.Config{
		Landing: "cloud",
		Media: map[string]config.Media{
			"cloud": {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir(), "part_size": "65536"}},
		},
		Sources:  dles,
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	cfg.Parallelism.Workers = 4

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	// White-box: the prepared writer is not serial, so the conductor's clamp (keyed on
	// Serial alone) never fires even though part_size splits each archive into parts.
	spec := archiveio.RunSpec{ID: record.IDFromParts(record.DateString(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)), 1), CreatedAt: time.Now()}
	pw, err := eng.openWriter("cloud", spec, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pw.Serial {
		t.Errorf("cloud is a concurrent-write medium; Serial must be false even with part_size")
	}

	var mu sync.Mutex
	var clamped bool
	logf := func(format string, args ...any) {
		if strings.Contains(fmt.Sprintf(format, args...), "running 1 worker") {
			mu.Lock()
			clamped = true
			mu.Unlock()
		}
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), logf)
	if err != nil {
		t.Fatalf("concurrent part-split dump: %v", err)
	}
	if clamped {
		t.Fatal("cloud with part_size was clamped to 1 worker; it must stay concurrent")
	}
	if len(s.Archives) != len(dles) {
		t.Fatalf("archives = %d, want %d", len(s.Archives), len(dles))
	}
	if rep, err := eng.Verify([]string{s.ID}, VerifyOptions{}, nil); err != nil || rep.Failures != 0 {
		t.Fatalf("verify: failures=%d err=%v", rep.Failures, err)
	}
}

// TestCloudPartSizeDefaultAndBound covers the policy the engine applies: an unset
// part_size defaults to 10 GB, and an explicit value above the 40 GB ceiling is
// rejected (so the knob can never silently reproduce the 10000-part upload failure).
func TestCloudPartSizeDefaultAndBound(t *testing.T) {
	base := func(p map[string]string) *config.Config {
		return &config.Config{
			Landing:  "cloud",
			Media:    map[string]config.Media{"cloud": {Type: "cloud", Params: p}},
			Workdir:  t.TempDir(),
			StateDir: t.TempDir(),
		}
	}

	eng, err := New(base(map[string]string{"url": "mem://"}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := eng.partSizeFor("cloud")
	if err != nil {
		t.Fatalf("partSizeFor (unset): %v", err)
	}
	if got != 10<<30 { // 10 GiB (binary units, per the cloud medium's part_size policy)
		t.Errorf("default part_size = %d, want 10 GiB", got)
	}

	eng2, err := New(base(map[string]string{"url": "mem://", "part_size": "50GB"})) // > 40 GiB cap
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng2.partSizeFor("cloud"); err == nil {
		t.Fatal("part_size above the 40 GiB cap should be rejected")
	} else if !strings.Contains(err.Error(), "exceeds the maximum") {
		t.Errorf("error = %q, want it to explain the maximum", err)
	}
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
		Sync:     []config.SyncRule{{To: "cloud"}},
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

	report, err := eng.SyncTo("", "cloud", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("sync apply: %v", err)
	}
	if report.Copied() != 1 {
		t.Fatalf("copied = %d, want 1", report.Copied())
	}
	if !eng.placedOn(s.ID, "cloud") {
		t.Fatalf("run %s not on cloud after sync", s.ID)
	}
	if got := len(eng.cat.Placements(s.ID)); got != 2 {
		t.Fatalf("run %s placements = %d, want 2 (disk + cloud)", s.ID, got)
	}

	// Re-sync is a no-op: the run already exists on the cloud target.
	report, err = eng.SyncTo("", "cloud", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if report.Copied() != 0 {
		t.Fatalf("re-sync copied = %d, want 0 (idempotent)", report.Copied())
	}
}
