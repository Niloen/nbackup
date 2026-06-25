package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// TestRecoverSelectedFiles dumps a full then an incremental, then recovers a
// single changed file and an unchanged file as of the latest date — exercising
// OpenRecover (the merged tree) + ExtractSelection (per-archive extraction).
func TestRecoverSelectedFiles(t *testing.T) {
	src := t.TempDir()
	catalogDir := t.TempDir()

	write(t, filepath.Join(src, "etc", "hosts"), "hosts-v1")
	write(t, filepath.Join(src, "etc", "passwd"), "passwd-v1")
	write(t, filepath.Join(src, "var", "log", "a.log"), "log-v1")

	cfg := &config.Config{
		Landing: "disk",
		Media:   map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": catalogDir}}},
		Sources: []config.DLE{{Host: "localhost", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skip("GNU tar not available")
	}

	day1 := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(day1, nil); err != nil {
		t.Fatalf("day1: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)
	write(t, filepath.Join(src, "etc", "hosts"), "hosts-v2")  // changed
	write(t, filepath.Join(src, "etc", "new.conf"), "new-v1") // added

	day2 := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(day2, nil); err != nil {
		t.Fatalf("day2: %v", err)
	}

	name := config.DLE{Host: "localhost", Path: src}.Name()

	// As of day 2: recover the changed file (from the incremental), the unchanged
	// file (from the full), and the added file — into one destination.
	tree, err := eng.OpenRecover(name, "2026-06-22")
	if err != nil {
		t.Fatalf("OpenRecover: %v", err)
	}
	steps, err := tree.Collect([]string{"/etc/hosts", "/etc/passwd", "/etc/new.conf"})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	dest := t.TempDir()
	if _, err := eng.ExtractSelection(steps, dest, logfDiscard); err != nil {
		t.Fatalf("ExtractSelection: %v", err)
	}
	assertContent(t, filepath.Join(dest, "etc", "hosts"), "hosts-v2")
	assertContent(t, filepath.Join(dest, "etc", "passwd"), "passwd-v1")
	assertContent(t, filepath.Join(dest, "etc", "new.conf"), "new-v1")
	// Selected-file recovery extracts only what was asked: a.log was not selected.
	if _, err := os.Stat(filepath.Join(dest, "var", "log", "a.log")); !os.IsNotExist(err) {
		t.Errorf("a.log should not have been recovered, stat err = %v", err)
	}

	// As of day 1: hosts is the original and new.conf does not exist.
	tree1, err := eng.OpenRecover(name, "2026-06-21")
	if err != nil {
		t.Fatalf("OpenRecover day1: %v", err)
	}
	if _, ok := tree1.Lookup("etc/new.conf"); ok {
		t.Error("new.conf should not exist as of day 1")
	}
	steps1, err := tree1.Collect([]string{"/etc/hosts"})
	if err != nil {
		t.Fatal(err)
	}
	dest1 := t.TempDir()
	if _, err := eng.ExtractSelection(steps1, dest1, logfDiscard); err != nil {
		t.Fatalf("ExtractSelection day1: %v", err)
	}
	assertContent(t, filepath.Join(dest1, "etc", "hosts"), "hosts-v1")
}

// TestRecoverWholeDirectory recovers a directory subtree spanning the full and
// the incremental in one selection.
func TestRecoverWholeDirectory(t *testing.T) {
	src := t.TempDir()
	catalogDir := t.TempDir()
	write(t, filepath.Join(src, "etc", "hosts"), "h1")
	write(t, filepath.Join(src, "etc", "passwd"), "p1")

	cfg := &config.Config{
		Landing: "disk",
		Media:   map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": catalogDir}}},
		Sources: []config.DLE{{Host: "localhost", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skip("GNU tar not available")
	}
	if _, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	write(t, filepath.Join(src, "etc", "hosts"), "h2")
	if _, err := eng.Run(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatal(err)
	}

	name := config.DLE{Host: "localhost", Path: src}.Name()
	tree, err := eng.OpenRecover(name, "2026-06-22")
	if err != nil {
		t.Fatal(err)
	}
	steps, err := tree.Collect([]string{"/etc"})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := eng.ExtractSelection(steps, dest, logfDiscard); err != nil {
		t.Fatal(err)
	}
	assertContent(t, filepath.Join(dest, "etc", "hosts"), "h2")  // from incremental
	assertContent(t, filepath.Join(dest, "etc", "passwd"), "p1") // from full
}
