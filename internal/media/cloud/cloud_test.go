package cloud

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"gocloud.dev/blob"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/media/fslike"
	"github.com/Niloen/nbackup/internal/record"
)

// openVol opens a fresh in-memory cloud volume. The mem:// driver needs no
// network or credentials — and returns an independent, empty bucket per open — so
// the full Volume contract is exercised offline with no cross-test state.
func openVol(t *testing.T) media.Volume {
	t.Helper()
	v, err := media.OpenVolume("cloud", media.Options{"url": "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestRequiresURL: the factory rejects a missing url rather than opening a
// useless volume.
func TestRequiresURL(t *testing.T) {
	if _, err := media.OpenVolume("cloud", media.Options{}); err == nil {
		t.Fatal("expected cloud to require a url")
	}
}

// TestRejectsPartSize mirrors disk: an object store is unbounded, never splits an
// archive, and silently ignoring part_size would mislead.
func TestRejectsPartSize(t *testing.T) {
	_, err := media.OpenVolume("cloud", media.Options{"url": "mem://", "part_size": "1MB"})
	if err == nil {
		t.Fatal("expected cloud to reject part_size")
	}
}

func appendArchive(t *testing.T, v media.Volume, slot, dle string, level int, payload string) int {
	t.Helper()
	pos, err := v.AppendFile(
		record.Header{Slot: slot, Kind: record.KindArchive, DLE: dle, Level: level, Compress: "none"},
		func(w io.Writer) error { _, e := w.Write([]byte(payload)); return e },
	)
	if err != nil {
		t.Fatal(err)
	}
	return pos
}

func TestVolumeRoundTrip(t *testing.T) {
	v := openVol(t)

	pos := appendArchive(t, v, "slot-2026-06-22", "h-data", 0, "hello world")

	h, rc, err := v.ReadFile(pos)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if h.DLE != "h-data" || h.Level != 0 || h.Kind != record.KindArchive {
		t.Errorf("header round trip wrong: %+v", h)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Errorf("payload = %q, want %q", data, "hello world")
	}
}

// TestCleanPayloadObject confirms the payload object is a clean archive (no header
// to skip) under the expected slots/<slot>/ key, with a separate .hdr sidecar —
// so a plain GET yields a stock-tool-usable file, exactly as the disk medium does.
func TestCleanPayloadObject(t *testing.T) {
	// Drive the blob store directly so the on-bucket object keys can be inspected.
	ctx := context.Background()
	bucket, err := blob.OpenBucket(ctx, "mem://")
	if err != nil {
		t.Fatal(err)
	}
	defer bucket.Close()
	v, err := fslike.Open(blobStore{ctx: ctx, bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	appendArchive(t, v, "slot-2026-06-22", "h-data", 0, "hello world")

	var payloadKey, hdrKey string
	iter := bucket.List(nil)
	for {
		obj, err := iter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasSuffix(obj.Key, ".hdr"):
			hdrKey = obj.Key
		case strings.HasSuffix(obj.Key, ".tar"):
			payloadKey = obj.Key
		}
	}
	if !strings.HasPrefix(payloadKey, "slots/slot-2026-06-22/") {
		t.Errorf("payload key = %q, want slots/slot-2026-06-22/…​.tar", payloadKey)
	}
	if hdrKey == "" {
		t.Errorf("no .hdr sidecar object written")
	}
	raw, err := bucket.ReadAll(ctx, payloadKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "hello world" {
		t.Errorf("payload object is not a clean archive: %q", raw)
	}
}

// TestReopenRescans confirms reopening the same bucket rebuilds the position index
// from object keys alone (the catalog-rebuild path). It uses the file:// driver
// because it must persist across two opens (each mem:// open is independent).
func TestReopenRescans(t *testing.T) {
	url := "file://" + t.TempDir()
	v, err := media.OpenVolume("cloud", media.Options{"url": url, "prefix": "reopen-test/"})
	if err != nil {
		t.Fatal(err)
	}
	pos := appendArchive(t, v, "slot-a", "h-data", 0, "payload")

	v2, err := media.OpenVolume("cloud", media.Options{"url": url, "prefix": "reopen-test/"})
	if err != nil {
		t.Fatal(err)
	}
	_, rc, err := v2.ReadFile(pos)
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "payload" {
		t.Errorf("payload after reopen = %q, want %q", data, "payload")
	}
}

// TestConcurrentAppend exercises parallel workers: many appends must get unique
// positions and all land. Run under -race.
func TestConcurrentAppend(t *testing.T) {
	v := openVol(t)

	const n = 24
	var wg sync.WaitGroup
	positions := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			positions[i] = appendArchive(t, v, "slot-x", fmt.Sprintf("dle-%d", i), 0, fmt.Sprintf("payload-%d", i))
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for _, p := range positions {
		if seen[p] {
			t.Fatalf("duplicate position %d", p)
		}
		seen[p] = true
	}
	files, err := v.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != n {
		t.Fatalf("Files() = %d, want %d", len(files), n)
	}
	for i := 1; i < len(files); i++ {
		if files[i].Pos <= files[i-1].Pos {
			t.Errorf("Files() not sorted by position at %d", i)
		}
	}
}

func TestRemoveSlot(t *testing.T) {
	v := openVol(t)
	appendArchive(t, v, "slot-a", "h-data", 0, "a")
	appendArchive(t, v, "slot-b", "h-data", 0, "b")

	if err := v.RemoveSlot("slot-a"); err != nil {
		t.Fatal(err)
	}
	files, err := v.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Header.Slot != "slot-b" {
		t.Fatalf("after RemoveSlot(slot-a), files = %+v", files)
	}
}

// TestAbortedWriteLeavesNoObject confirms a failed payload write does not commit a
// partial object — the same atomicity the disk and tape media rely on.
func TestAbortedWriteLeavesNoObject(t *testing.T) {
	cv := openVol(t)
	wantErr := fmt.Errorf("boom")
	_, err := cv.AppendFile(
		record.Header{Slot: "slot-x", Kind: record.KindArchive, DLE: "h-data", Compress: "none"},
		func(w io.Writer) error { return wantErr },
	)
	if err == nil {
		t.Fatal("expected the write error to propagate")
	}
	files, err := cv.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("aborted write left files behind: %+v", files)
	}
}
