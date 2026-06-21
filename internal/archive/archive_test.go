package archive

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRoundTripFullAndIncremental verifies that a full archive plus an
// incremental archive, extracted in order, reproduce the live data including a
// modified and a newly added file.
func TestRoundTripFullAndIncremental(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "alpha")
	mustWrite(t, filepath.Join(src, "sub", "b.txt"), "beta")

	out := t.TempDir()
	full := filepath.Join(out, "full.tar.zst")
	res, err := Create(CreateOptions{SourcePath: src, OutFile: full})
	if err != nil {
		t.Fatalf("full create: %v", err)
	}
	if res.FileCount != 2 {
		t.Fatalf("full file count = %d, want 2", res.FileCount)
	}

	// Modify one file, add one file.
	mustWrite(t, filepath.Join(src, "a.txt"), "alpha-changed")
	mustWrite(t, filepath.Join(src, "c.txt"), "gamma")

	incr := filepath.Join(out, "incr.tar.zst")
	res2, err := Create(CreateOptions{SourcePath: src, OutFile: incr, Base: res.Snapshot})
	if err != nil {
		t.Fatalf("incr create: %v", err)
	}
	if res2.FileCount != 2 {
		t.Fatalf("incremental file count = %d, want 2 (changed + new)", res2.FileCount)
	}

	dest := t.TempDir()
	if err := Extract(full, dest); err != nil {
		t.Fatalf("extract full: %v", err)
	}
	if err := Extract(incr, dest); err != nil {
		t.Fatalf("extract incr: %v", err)
	}

	assertContent(t, filepath.Join(dest, "a.txt"), "alpha-changed")
	assertContent(t, filepath.Join(dest, "sub", "b.txt"), "beta")
	assertContent(t, filepath.Join(dest, "c.txt"), "gamma")
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}
