package bucket

import (
	"context"
	"path/filepath"
	"testing"
)

// TestOpenPlainPath: a location with no scheme opens a metadata-free fileblob
// directory, created on demand and rooted at the path's absolute form.
func TestOpenPlainPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "created", "on", "demand")
	b, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("plain path: %v", err)
	}
	defer b.Close()

	// A round-trip proves the directory is usable and writes no .attrs sidecar.
	ctx := context.Background()
	if err := b.WriteAll(ctx, "obj", []byte("hi"), nil); err != nil {
		t.Fatal(err)
	}
	got, err := b.ReadAll(ctx, "obj")
	if err != nil || string(got) != "hi" {
		t.Fatalf("read back = %q err=%v, want hi", got, err)
	}
	if ok, _ := b.Exists(ctx, "obj.attrs"); ok {
		t.Fatal("fileblob should write no .attrs metadata sidecar")
	}
}

// TestOpenPlainPathIgnoresTempDir: fileblob commits a write by renaming a staged temp
// file, and a rename only works within one filesystem — so the staging must happen
// inside the bucket directory, never os.TempDir() (a library on /opt with a tmpfs /tmp
// would fail every write with EXDEV). An unusable TMPDIR simulates that: the write must
// succeed anyway, proving the temp file was never staged there.
func TestOpenPlainPathIgnoresTempDir(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))
	b, err := Open(context.Background(), filepath.Join(t.TempDir(), "lib"))
	if err != nil {
		t.Fatalf("plain path: %v", err)
	}
	defer b.Close()
	if err := b.WriteAll(context.Background(), "obj", []byte("hi"), nil); err != nil {
		t.Fatalf("write must stage inside the bucket dir, not TMPDIR: %v", err)
	}
}

// TestOpenRelativePath exercises the filepath.Abs branch: a relative location is
// resolved to an absolute directory (Abs succeeds for any syntactically valid path).
func TestOpenRelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := Open(context.Background(), "rel-sub")
	if err != nil {
		t.Fatalf("relative path: %v", err)
	}
	defer b.Close()
	if err := b.WriteAll(context.Background(), "k", []byte("v"), nil); err != nil {
		t.Fatal(err)
	}
}

// TestOpenMemScheme: a URL location dispatches to its scheme's driver — mem:// opens
// the in-memory bucket.
func TestOpenMemScheme(t *testing.T) {
	b, err := Open(context.Background(), "mem://")
	if err != nil {
		t.Fatalf("mem scheme: %v", err)
	}
	defer b.Close()
	if err := b.WriteAll(context.Background(), "k", []byte("v"), nil); err != nil {
		t.Fatal(err)
	}
}

// TestOpenFileScheme: a file:// URL uses the same fileblob driver a cloud bucket would
// swap in for, distinct from the plain-path branch.
func TestOpenFileScheme(t *testing.T) {
	url := "file://" + t.TempDir() + "?metadata=skip"
	b, err := Open(context.Background(), url)
	if err != nil {
		t.Fatalf("file scheme: %v", err)
	}
	defer b.Close()
}

// TestOpenUnknownScheme: a URL whose scheme has no registered driver is an error, not
// a mis-parse into a filesystem path.
func TestOpenUnknownScheme(t *testing.T) {
	if b, err := Open(context.Background(), "bogus://host/path"); err == nil {
		b.Close()
		t.Fatal("an unregistered URL scheme should error")
	}
}
