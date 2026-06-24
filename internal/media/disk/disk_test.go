package disk

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Niloen/nbackup/internal/format"
	"github.com/Niloen/nbackup/internal/media"
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
	pos, err := v.AppendFile(
		format.Header{Slot: slot, Kind: format.KindArchive, DLE: dle, Level: level, Codec: "none"},
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

	pos := appendArchive(t, v, "slot-2026-06-22", "h-data", 0, "hello world")

	h, rc, err := v.ReadFile(pos)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if h.DLE != "h-data" || h.Level != 0 || h.Kind != format.KindArchive {
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
	slotDir := filepath.Join(dir, "slots", "slot-2026-06-22")
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

func TestRemoveSlot(t *testing.T) {
	dir := t.TempDir()
	v := openVol(t, dir)
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
