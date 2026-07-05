package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// asOfFixture dumps one run onto disk and returns the engine and DLE name for the
// as-of restore tests.
func asOfFixture(t *testing.T) (*Engine, string) {
	t.Helper()
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "as of me")

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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skip("GNU tar not available")
	}
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("dump: %v", err)
	}
	return eng, config.DLE{Host: "localhost", Path: src}.Name()
}

// TestRestoreAsOfPinnedMedium exercises RestoreAsOf with a valid --from pin: the whole
// DLE is reconstructed as of a date, reading from the named medium's copy.
func TestRestoreAsOfPinnedMedium(t *testing.T) {
	eng, dle := asOfFixture(t)

	dest := t.TempDir()
	if err := eng.RestoreAsOf(dle, "2026-06-21", dest, "disk", false, nil); err != nil {
		t.Fatalf("restore as-of with --from disk: %v", err)
	}
	assertContent(t, filepath.Join(dest, "f.txt"), "as of me")
}

// TestRestoreAsOfUnknownFrom exercises checkFromMedium via RestoreAsOf: an unknown
// --from medium is rejected before any restore work.
func TestRestoreAsOfUnknownFrom(t *testing.T) {
	eng, dle := asOfFixture(t)

	err := eng.RestoreAsOf(dle, "2026-06-21", t.TempDir(), "nope", false, nil)
	if err == nil {
		t.Fatal("an unknown --from medium must be rejected")
	}
	if !strings.Contains(err.Error(), "unknown medium") || !strings.Contains(err.Error(), "--from") {
		t.Errorf("error = %v, want an unknown --from medium error", err)
	}
}

// TestRestoreAsOfToUnknownFrom exercises checkFromMedium via the remote (RestoreAsOfTo)
// path: the --from validation fires before any SSH connection is attempted, so an
// unknown medium is a clean rejection even without a reachable client.
func TestRestoreAsOfToUnknownFrom(t *testing.T) {
	eng, dle := asOfFixture(t)

	err := eng.RestoreAsOfTo(dle, "2026-06-21", "app01", "/restore", "nope", nil)
	if err == nil {
		t.Fatal("an unknown --from medium must be rejected before the SSH restore")
	}
	if !strings.Contains(err.Error(), "unknown medium") {
		t.Errorf("error = %v, want an unknown-medium error", err)
	}
}
