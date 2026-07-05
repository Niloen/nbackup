package gdrive

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/media/fslike"
	"github.com/Niloen/nbackup/internal/record"
)

// openVol builds a fresh gdrive volume over an in-memory fake, returning both so a test can
// inspect the on-Drive tree. The full Volume contract runs offline, with no network or
// credentials — the same posture the cloud medium gets from mem://.
func openVol(t *testing.T) (media.Volume, *fakeDrive) {
	t.Helper()
	fake := newFakeDrive()
	v, err := fslike.Open(newStore(fake, rootID))
	if err != nil {
		t.Fatal(err)
	}
	return v, fake
}

func appendArchive(t *testing.T, v media.Volume, run, dle string, level int, payload string) int {
	t.Helper()
	pos, err := writeFileT(v,
		record.Header{Run: run, Kind: record.KindArchive, DLE: dle, Level: level, Compress: "none"},
		func(w io.Writer) error { _, e := w.Write([]byte(payload)); return e },
	)
	if err != nil {
		t.Fatal(err)
	}
	return pos
}

func TestVolumeRoundTrip(t *testing.T) {
	v, _ := openVol(t)
	pos := appendArchive(t, v, "run-2026-06-22.001", "h-data", 0, "hello world")

	h, rc, err := v.ReadFile(pos, media.Range{})
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

// TestRangedRead confirms a sub-range read returns just those bytes — the Drive Range-header
// download that keeps a selective restore from paying for a whole archive.
func TestRangedRead(t *testing.T) {
	v, _ := openVol(t)
	pos := appendArchive(t, v, "run-a", "h-data", 0, "0123456789")

	_, rc, err := v.ReadFile(pos, media.Range{Off: 3, Len: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "3456" {
		t.Errorf("ranged read = %q, want %q", data, "3456")
	}
}

// TestCleanLayout confirms the on-Drive tree is runs/<run>/<clean-payload> + a .hdr sidecar,
// so the layout matches disk and cloud (a run streams between them unchanged).
func TestCleanLayout(t *testing.T) {
	v, fake := openVol(t)
	appendArchive(t, v, "run-2026-06-22.001", "h-data", 0, "hello world")

	runsID := fake.folderID(rootID, "runs")
	if runsID == "" {
		t.Fatal("no runs/ folder created")
	}
	runID := fake.folderID(runsID, "run-2026-06-22.001")
	if runID == "" {
		t.Fatal("no run subfolder created")
	}
	items, _ := fake.list(runID)
	var payload, hdr string
	for _, it := range items {
		switch {
		case len(it.name) >= 4 && it.name[len(it.name)-4:] == ".hdr":
			hdr = it.name
		case len(it.name) >= 4 && it.name[len(it.name)-4:] == ".tar":
			payload = it.name
		}
	}
	if payload == "" || hdr == "" {
		t.Fatalf("run folder = %+v, want a .tar payload and a .hdr sidecar", items)
	}
}

// TestReopenRescans confirms a fresh store over the same Drive rebuilds the position index
// from the folder tree alone (the catalog-rebuild path) and reads back a file.
func TestReopenRescans(t *testing.T) {
	fake := newFakeDrive()
	v, err := fslike.Open(newStore(fake, rootID))
	if err != nil {
		t.Fatal(err)
	}
	pos := appendArchive(t, v, "run-a", "h-data", 0, "payload")

	v2, err := fslike.Open(newStore(fake, rootID))
	if err != nil {
		t.Fatal(err)
	}
	_, rc, err := v2.ReadFile(pos, media.Range{})
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "payload" {
		t.Errorf("payload after reopen = %q, want %q", data, "payload")
	}
}

// TestConcurrentAppendOneRunFolder exercises parallel workers writing the same run: every
// append lands with a unique position, and — the Drive-specific hazard — the store mints
// exactly ONE run folder despite the race (Drive would happily create duplicates). Run -race.
func TestConcurrentAppendOneRunFolder(t *testing.T) {
	v, fake := openVol(t)

	const n = 24
	var wg sync.WaitGroup
	positions := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			positions[i] = appendArchive(t, v, "run-x", fmt.Sprintf("dle-%d", i), 0, fmt.Sprintf("payload-%d", i))
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
	runsID := fake.folderID(rootID, "runs")
	if got := fake.countFolders(runsID, "run-x"); got != 1 {
		t.Fatalf("run folder count = %d, want 1 (concurrent appends must not duplicate the folder)", got)
	}
}

func TestRemoveFile(t *testing.T) {
	v, _ := openVol(t)
	posA := appendArchive(t, v, "run-a", "h-data", 0, "a")
	appendArchive(t, v, "run-b", "h-data", 0, "b")

	if err := v.RemoveFile(posA); err != nil {
		t.Fatal(err)
	}
	files, err := v.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Header.Run != "run-b" {
		t.Fatalf("after RemoveFile(run-a), files = %+v", files)
	}
	if err := v.RemoveFile(posA); err != nil {
		t.Fatalf("second RemoveFile should be a no-op: %v", err)
	}
}

// TestRemoveTreeReclaimsFolder confirms removing a run's last file reclaims the run folder,
// leaving no empty directory behind (the fslike layer calls RemoveTree for the last file).
func TestRemoveTreeReclaimsFolder(t *testing.T) {
	v, fake := openVol(t)
	pos := appendArchive(t, v, "run-a", "h-data", 0, "a")
	runsID := fake.folderID(rootID, "runs")
	if fake.folderID(runsID, "run-a") == "" {
		t.Fatal("run folder not created")
	}
	if err := v.RemoveFile(pos); err != nil {
		t.Fatal(err)
	}
	if id := fake.folderID(runsID, "run-a"); id != "" {
		t.Fatalf("run folder %s survived removal of its last file", id)
	}
}

// TestAbortedWriteLeavesNoObject confirms an aborted write — ctx canceled before Close —
// commits no file: no header sidecar lands, so the scan skips the orphan payload, the same
// atomicity disk/cloud/tape rely on.
func TestAbortedWriteLeavesNoObject(t *testing.T) {
	v, _ := openVol(t)
	ctx, cancel := context.WithCancel(context.Background())
	fw, err := v.AppendFile(ctx, record.Header{Run: "run-x", Kind: record.KindArchive, DLE: "h-data", Compress: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("partial payload")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := fw.Close(); err == nil {
		t.Fatal("expected Close to report the aborted write")
	}
	files, err := v.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("aborted write left files behind: %+v", files)
	}
}

// TestFactoryRequiresFolder confirms the factory rejects a missing folder before it ever
// reaches for credentials (so the check is testable without a real Drive).
func TestFactoryRequiresFolder(t *testing.T) {
	if _, err := media.OpenVolume("gdrive", media.Options{}); err == nil {
		t.Fatal("expected gdrive to require a folder")
	}
}

func TestPartSizePolicy(t *testing.T) {
	p := media.PartSizeFor("gdrive")
	if p.Default != 10<<30 {
		t.Errorf("gdrive part_size default = %d, want 10 GiB", p.Default)
	}
	if p.Max != 100<<30 {
		t.Errorf("gdrive part_size max = %d, want 100 GiB", p.Max)
	}
}

// TestPrefixReroots confirms a prefix nests the run tree under a subfolder, so several
// catalogs can share one Drive folder.
func TestPrefixReroots(t *testing.T) {
	fake := newFakeDrive()
	seed := newStore(fake, rootID)
	seed.mu.Lock()
	sub, _, err := seed.dirIDLocked("team-a/nb", true)
	seed.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	v, err := fslike.Open(newStore(fake, sub))
	if err != nil {
		t.Fatal(err)
	}
	appendArchive(t, v, "run-a", "h-data", 0, "x")
	if fake.folderID(sub, "runs") == "" {
		t.Fatal("runs/ not created under the prefix subfolder")
	}
	if fake.folderID(rootID, "runs") != "" {
		t.Fatal("runs/ leaked to the root instead of the prefix")
	}
}

func TestExtractCode(t *testing.T) {
	cases := map[string]string{
		"4/0AbCdEf": "4/0AbCdEf",
		"http://localhost/?code=4/0AbCdEf&scope=drive": "4/0AbCdEf",
		"http://localhost/?scope=drive&code=xyz":       "xyz",
	}
	for in, want := range cases {
		if got := extractCode(in); got != want {
			t.Errorf("extractCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// writeFileT bridges tests to the writer-based AppendFile.
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
