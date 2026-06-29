package disk

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

func openVol(t *testing.T, path string) media.Volume {
	t.Helper()
	v, err := media.OpenVolume("disk", media.Options{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestRejectsPartSize confirms disk refuses part_size: it is unbounded, so it never
// splits an archive, and silently ignoring the knob would mislead.
func TestRejectsPartSize(t *testing.T) {
	_, err := media.OpenVolume("disk", media.Options{"path": t.TempDir(), "part_size": "1MB"})
	if err == nil {
		t.Fatal("expected disk to reject part_size")
	}
}

func appendArchive(t *testing.T, v media.Volume, slot, dle string, level int, payload string) int {
	t.Helper()
	pos, err := writeFileT(v,
		record.Header{Slot: slot, Kind: record.KindArchive, DLE: dle, Level: level, Compress: "none"},
		func(w io.Writer) error { _, e := w.Write([]byte(payload)); return e },
	)
	if err != nil {
		t.Fatal(err)
	}
	return pos
}

func TestVolumeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	v := openVol(t, dir)

	pos := appendArchive(t, v, "slot-2026-06-22.001", "h-data", 0, "hello world")

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

	// The on-disk payload file is a CLEAN archive (no header to skip) and the
	// header lives in a separate .hdr sidecar — usable directly with stock tools.
	slotDir := filepath.Join(dir, "slots", "slot-2026-06-22.001")
	entries, err := os.ReadDir(slotDir)
	if err != nil {
		t.Fatal(err)
	}
	var payloadName, hdrName string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".hdr" {
			hdrName = e.Name()
		} else {
			payloadName = e.Name()
		}
	}
	if hdrName == "" || payloadName == "" {
		t.Fatalf("expected a payload + .hdr sidecar, got %v", entries)
	}
	raw, err := os.ReadFile(filepath.Join(slotDir, payloadName))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "hello world" {
		t.Errorf("payload file is not a clean archive: %q", raw)
	}

	// Reopen (rescan from disk) must reconstruct the index and read back.
	v2 := openVol(t, dir)
	_, rc2, err := v2.ReadFile(pos)
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	rc2.Close()
}

// TestConcurrentAppend exercises parallel workers: many appends must get unique
// positions and all land. Run under -race.
func TestConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	v := openVol(t, dir)

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

// TestOrphanPayloadIgnored simulates an interrupted dump: the payload is written
// but the process dies before the .hdr sidecar. A later reopen must ignore the
// orphan rather than index a header-less position — otherwise Files() reads an
// empty header key and fails with "is a directory" on the slots root.
func TestOrphanPayloadIgnored(t *testing.T) {
	dir := t.TempDir()
	v := openVol(t, dir)
	pos := appendArchive(t, v, "slot-2026-06-22.001", "h-data", 0, "good")

	// Drop a bare payload (no sidecar) into a fresh slot, as an aborted Write would.
	orphanSlot := filepath.Join(dir, "slots", "slot-2026-06-25.001")
	if err := os.MkdirAll(orphanSlot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanSlot, "000000-h_data-L0.tar"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	v2 := openVol(t, dir)
	files, err := v2.Files()
	if err != nil {
		t.Fatalf("Files() after reopen with orphan: %v", err)
	}
	if len(files) != 1 || files[0].Pos != pos {
		t.Fatalf("orphan not ignored: files = %+v", files)
	}
}

// TestTornHeaderSkipped covers the present-but-corrupt case: the .hdr sidecar
// exists (so the position is not "incomplete") but its bytes are not valid JSON, as
// a power-loss or reordered write could leave. Files() must skip it like an absent
// file, never abort the whole catalog rebuild.
func TestTornHeaderSkipped(t *testing.T) {
	dir := t.TempDir()
	v := openVol(t, dir)
	pos := appendArchive(t, v, "slot-2026-06-22.001", "h-data", 0, "good")

	// A complete-looking pair (payload + .hdr) whose .hdr is garbage JSON.
	slot := filepath.Join(dir, "slots", "slot-2026-06-25.001")
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slot, "000001-h_data-L0.tar"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slot, "000001-h_data-L0.hdr"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	v2 := openVol(t, dir)
	files, err := v2.Files()
	if err != nil {
		t.Fatalf("Files() with a torn header: %v", err)
	}
	if len(files) != 1 || files[0].Pos != pos {
		t.Fatalf("torn header not skipped: files = %+v", files)
	}
}

func TestRemoveFile(t *testing.T) {
	dir := t.TempDir()
	v := openVol(t, dir)
	posA := appendArchive(t, v, "slot-a", "h-data", 0, "a")
	appendArchive(t, v, "slot-b", "h-data", 0, "b")

	if err := v.RemoveFile(posA); err != nil {
		t.Fatal(err)
	}
	files, err := v.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Header.Slot != "slot-b" {
		t.Fatalf("after RemoveFile(slot-a), files = %+v", files)
	}
	// Removing the slot's last file reclaims its now-empty directory.
	if _, err := os.Stat(filepath.Join(dir, "slots", "slot-a")); !os.IsNotExist(err) {
		t.Fatalf("slot-a directory still present after its last file was removed: %v", err)
	}
	// Removing a position again is a no-op (idempotent).
	if err := v.RemoveFile(posA); err != nil {
		t.Fatalf("second RemoveFile should be a no-op: %v", err)
	}
}

// TestRemoveFileRaceWithAppend guards the concurrency the multi-holding-disk drain relies on:
// a dump appends a new archive to a slot while the drain reclaims another archive's last file
// from the SAME slot. RemoveFile reclaims a slot's directory when it empties — so the in-flight
// append must keep its directory from being swept out from under its just-written payload.
// Each iteration starts a fresh slot with one file, then removes it concurrently with appending
// a second to the same slot; the second file must survive and be readable.
func TestRemoveFileRaceWithAppend(t *testing.T) {
	for i := 0; i < 300; i++ {
		dir := t.TempDir()
		v := openVol(t, dir)
		slot := "slot-race"
		posA := appendArchive(t, v, slot, "a", 0, "a-payload")

		var wg sync.WaitGroup
		wg.Add(2)
		var posB int
		go func() { defer wg.Done(); _ = v.RemoveFile(posA) }()
		go func() { defer wg.Done(); posB = appendArchive(t, v, slot, "b", 0, "b-payload") }()
		wg.Wait()

		_, rc, err := v.ReadFile(posB)
		if err != nil {
			t.Fatalf("iter %d: concurrently-appended file vanished: %v", i, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != "b-payload" {
			t.Fatalf("iter %d: read %q, want b-payload", i, got)
		}
	}
}

// writeFileT bridges tests to the writer-based AppendFile (callback shape kept for brevity).
func writeFileT(v media.Volume, h record.Header, write func(io.Writer) error) (int, error) {
	fw, err := v.AppendFile(context.Background(), h)
	if err != nil {
		return 0, err
	}
	if err := write(fw); err != nil {
		fw.Close()
		return 0, err
	}
	if err := fw.Close(); err != nil {
		return 0, err
	}
	return fw.Pos(), nil
}
