package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// TestPartialDumpKeepsRunAndMarksArchive is the regression for two partial-dump gaps:
// a dump that committed a PARTIAL archive (an unreadable source file omitted, run exits
// non-zero) used to (1) return no run — so the CLI's failure record carried an empty run
// id and `nb report --dump --run <id>` found nothing — and (2) record nothing about the
// omission in the commit footer, so the PARTIAL fact vanished from `nb run` and from a
// rebuilt catalog. Now the failed run returns its committed run, the archive carries the
// omitted-file count, and a rebuild from the medium preserves it.
func TestPartialDumpKeepsRunAndMarksArchive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 files are still readable as root; the partial path cannot trigger")
	}
	src := t.TempDir()
	write(t, filepath.Join(src, "readable.txt"), "ok")
	locked := filepath.Join(src, "locked.txt")
	write(t, locked, "secret")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Landing:  "disk",
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
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	day := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	s, runErr := eng.Run(context.Background(), day, nil)
	if runErr == nil {
		t.Fatal("a partial dump must return an error (the gap must be loud)")
	}
	// (1) The failure still hands back the committed run — id and archives — so the
	// caller's run record is not blank.
	if s == nil || s.ID == "" {
		t.Fatalf("failed run returned no committed run (run = %+v)", s)
	}
	if len(s.Archives) != 1 {
		t.Fatalf("committed archives = %d, want 1", len(s.Archives))
	}
	// (2) The archive carries the PARTIAL marker.
	ar := s.Archives[0]
	if ar.Unreadable == 0 || !ar.Partial() {
		t.Fatalf("archive Unreadable = %d, want > 0 (PARTIAL must be recorded)", ar.Unreadable)
	}
	if !s.Partial() {
		t.Fatal("run.Partial() = false, want true")
	}

	// A rebuild from the medium's own commit footers preserves the marker — the fact
	// survives a wiped catalog.
	if _, err := eng.RebuildCatalog(nil); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt, err := eng.Catalog().ReadRun(s.ID)
	if err != nil {
		t.Fatalf("read rebuilt run: %v", err)
	}
	if len(rebuilt.Archives) != 1 || rebuilt.Archives[0].Unreadable != ar.Unreadable {
		t.Fatalf("rebuilt archive Unreadable = %+v, want %d preserved", rebuilt.Archives, ar.Unreadable)
	}
}
