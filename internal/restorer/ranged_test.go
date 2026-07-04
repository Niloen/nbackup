package restorer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

// tarEntry is one ordered member of a built test archive.
type tarEntry struct {
	name, content string
}

// buildTarOrdered builds a real tar stream with deterministic member order and
// returns it with each member's byte offset — the fixture a ranged read plans over.
func buildTarOrdered(t *testing.T, entries []tarEntry) ([]byte, []record.Member) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var members []record.Member
	for _, e := range entries {
		members = append(members, record.Member{Path: e.name, Off: int64(buf.Len())})
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
		if err := tw.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), members
}

// frameGzip encodes raw as concatenated gzip members every frameSize bytes — exactly
// the FRAMED-INVISIBLE layout ChunkSource writes — returning the encoded stream and
// its frame table.
func frameGzip(t *testing.T, raw []byte, frameSize int) ([]byte, []record.Frame) {
	t.Helper()
	var enc bytes.Buffer
	var frames []record.Frame
	for off := 0; off < len(raw); off += frameSize {
		end := min(off+frameSize, len(raw))
		frames = append(frames, record.Frame{Raw: int64(off), Enc: int64(enc.Len())})
		gz := gzip.NewWriter(&enc)
		if _, err := gz.Write(raw[off:end]); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return enc.Bytes(), frames
}

// framedFixture builds a multi-frame gzip archive with a big head member and small
// tail members, loaded into a range-capable fakeStore.
func framedFixture(t *testing.T, dle string) (*fakeStore, recovery.ExtractStep, []record.Member, int) {
	entries := []tarEntry{
		{"big.bin", strings.Repeat("B", 200*1024)},
		{"etc-hosts", "127.0.0.1 localhost\n"},
		{"tail.txt", "the needle\n"},
	}
	raw, members := buildTarOrdered(t, entries)
	enc, frames := frameGzip(t, raw, 8*1024)
	r := ref("run-2026-07-04.001", dle, 0)
	store := &fakeStore{
		payloads: map[archiveio.Ref][]byte{r: enc},
		members:  map[archiveio.Ref][]record.Member{r: members},
		frames:   map[archiveio.Ref][]record.Frame{r: frames},
		ranged:   true,
	}
	step := recovery.ExtractStep{
		Step: recovery.Step{RunID: r.Run, DLE: dle, Level: 0, Archiver: "gnutar", Compress: "gzip"},
	}
	return store, step, members, len(enc)
}

// TestExtractSelectionRangedEgress is the selective-restore acceptance: extracting one
// small file from a multi-frame archive must fetch only a small fraction of the
// encoded object (counted by the fake's ranged opener), never open the whole stream,
// and land the right bytes via the real gzip → tar pipeline.
func TestExtractSelectionRangedEgress(t *testing.T) {
	arch := gnutarOrSkip(t)
	dle := "app01-data"
	store, step, _, encTotal := framedFixture(t, dle)
	step.Members = []string{"tail.txt"}

	deps := testDeps(store, nil)
	deps.ArchiverFor = func(typeName, host string) (archiver.Archiver, error) { return arch, nil }
	r := New(deps)
	dest := filepath.Join(t.TempDir(), "out")
	files, archives, err := r.ExtractSelection([]recovery.ExtractStep{step}, dest, nil)
	if err != nil {
		t.Fatalf("ExtractSelection (ranged): %v", err)
	}
	if files != 1 || archives != 1 {
		t.Fatalf("files=%d archives=%d, want 1/1", files, archives)
	}
	got, err := os.ReadFile(filepath.Join(dest, "tail.txt"))
	if err != nil || string(got) != "the needle\n" {
		t.Fatalf("restored tail.txt = %q, %v", got, err)
	}
	if len(store.opened) != 0 {
		t.Fatalf("the whole stream was opened (%v) — the ranged path should have handled it", store.opened)
	}
	if store.rangedBytes == 0 || store.rangedBytes > int64(encTotal)/4 {
		t.Fatalf("ranged egress = %d of %d encoded bytes; want a small fraction", store.rangedBytes, encTotal)
	}
}

// TestExtractSelectionRangedMidMember extracts a bounded mid-stream member (its extent
// ends at the next member's offset), proving the discard-to-offset + extent-limit path.
func TestExtractSelectionRangedMidMember(t *testing.T) {
	arch := gnutarOrSkip(t)
	dle := "app01-data"
	store, step, _, _ := framedFixture(t, dle)
	step.Members = []string{"etc-hosts"}

	deps := testDeps(store, nil)
	deps.ArchiverFor = func(typeName, host string) (archiver.Archiver, error) { return arch, nil }
	r := New(deps)
	dest := filepath.Join(t.TempDir(), "out")
	if _, _, err := r.ExtractSelection([]recovery.ExtractStep{step}, dest, nil); err != nil {
		t.Fatalf("ExtractSelection: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "etc-hosts"))
	if err != nil || string(got) != "127.0.0.1 localhost\n" {
		t.Fatalf("restored etc-hosts = %q, %v", got, err)
	}
}

// TestExtractSelectionRangedFallback: on a range-incapable copy the selection falls
// back to the whole-stream path — the unchanged existing code — and still restores.
func TestExtractSelectionRangedFallback(t *testing.T) {
	arch := gnutarOrSkip(t)
	dle := "app01-data"
	store, step, _, _ := framedFixture(t, dle)
	store.ranged = false
	step.Members = []string{"tail.txt"}

	deps := testDeps(store, nil)
	deps.ArchiverFor = func(typeName, host string) (archiver.Archiver, error) { return arch, nil }
	r := New(deps)
	dest := filepath.Join(t.TempDir(), "out")
	if _, _, err := r.ExtractSelection([]recovery.ExtractStep{step}, dest, nil); err != nil {
		t.Fatalf("ExtractSelection (fallback): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "tail.txt"))
	if err != nil || string(got) != "the needle\n" {
		t.Fatalf("restored tail.txt = %q, %v", got, err)
	}
	if len(store.opened) != 1 {
		t.Fatalf("fallback should open the whole stream once, opened %v", store.opened)
	}
}

// TestPlanExtentsAndGroups pins the pure planning: extent resolution + coalescing, and
// frame covering + fetch merging.
func TestPlanExtentsAndGroups(t *testing.T) {
	members := []record.Member{
		{Path: "a", Off: 0}, {Path: "b", Off: 1024}, {Path: "c", Off: 4096}, {Path: "d", Off: 9216},
	}
	// Adjacent selections coalesce.
	ext, ok := planExtents(members, []string{"a", "b"})
	if !ok || len(ext) != 1 || ext[0] != (rawExtent{start: 0, end: 4096}) {
		t.Fatalf("adjacent extents should coalesce: %+v ok=%v", ext, ok)
	}
	// The last member's extent runs to the stream's end.
	ext, ok = planExtents(members, []string{"d"})
	if !ok || len(ext) != 1 || ext[0] != (rawExtent{start: 9216, end: -1}) {
		t.Fatalf("tail extent: %+v ok=%v", ext, ok)
	}
	// A missing member or an offset-less index refuses (fallback).
	if _, ok := planExtents(members, []string{"nope"}); ok {
		t.Fatal("unknown member must refuse")
	}
	if _, ok := planExtents([]record.Member{{Path: "a", Off: -1}}, []string{"a"}); ok {
		t.Fatal("offset-less index must refuse")
	}

	frames := []record.Frame{{Raw: 0, Enc: 0}, {Raw: 4096, Enc: 1000}, {Raw: 8192, Enc: 2000}}
	// One extent inside frame 0: fetch exactly frame 0's encoded range.
	g := planGroups(frames, []rawExtent{{start: 512, end: 1024}})
	if len(g) != 1 || g[0].encOff != 0 || g[0].encLen != 1000 || g[0].rawStart != 0 {
		t.Fatalf("frame-0 group: %+v", g)
	}
	// Two extents in the same frame window merge into one fetch.
	g = planGroups(frames, []rawExtent{{start: 512, end: 1024}, {start: 2048, end: 3072}})
	if len(g) != 1 || len(g[0].extents) != 2 {
		t.Fatalf("same-window extents should merge: %+v", g)
	}
	// A tail extent fetches to the stream's end.
	g = planGroups(frames, []rawExtent{{start: 9000, end: -1}})
	if len(g) != 1 || g[0].encOff != 2000 || g[0].encLen != -1 || g[0].rawStart != 8192 {
		t.Fatalf("tail group: %+v", g)
	}
	// The identity pipeline (no frames) maps extents 1:1.
	g = planGroups(nil, []rawExtent{{start: 512, end: 1024}})
	if len(g) != 1 || g[0].encOff != 512 || g[0].encLen != 512 || g[0].rawStart != 512 {
		t.Fatalf("identity group: %+v", g)
	}
}

// TestSampleFrame drills one frame group structurally over the fake store: the listed
// members+offsets must match the index slice, rotation picks different frames, and a
// tampered index is caught as a mismatch (Ran && !OK), not an error.
func TestSampleFrame(t *testing.T) {
	arch := gnutarOrSkip(t)
	dle := "app01-data"
	store, step, members, _ := framedFixture(t, dle)
	deps := testDeps(store, nil)
	r := New(deps)
	arch2 := record.Archive{Run: step.RunID, DLE: dle, Level: 0, Compress: "gzip"}
	aref := ref(step.RunID, dle, 0)

	res, err := r.Sample("", arch2, crypt.Options{}, arch, 0)
	if err != nil {
		t.Fatalf("SampleFrame: %v", err)
	}
	if !res.Ran || !res.OK {
		t.Fatalf("SampleFrame ran=%v ok=%v detail=%q", res.Ran, res.OK, res.Detail)
	}

	// Tamper: shift a recorded member offset — the structural sample must catch it.
	bad := append([]record.Member(nil), members...)
	for i := range bad {
		if bad[i].Path == "tail.txt" {
			bad[i].Off += 512
		}
	}
	store.members[aref] = bad
	caught := false
	for rot := 0; rot < len(store.frames[aref]); rot++ {
		res, err = r.Sample("", arch2, crypt.Options{}, arch, rot)
		if err != nil {
			t.Fatalf("SampleFrame rot=%d: %v", rot, err)
		}
		if res.Ran && !res.OK {
			caught = true
			break
		}
	}
	if !caught {
		t.Fatal("a tampered member offset must fail some frame's structural sample")
	}

	// No ranged copy: an ingredient gap, not a verdict.
	store.ranged = false
	res, err = r.Sample("", arch2, crypt.Options{}, arch, 0)
	if err != nil || res.Ran {
		t.Fatalf("rangeless copy should skip (ran=%v err=%v)", res.Ran, err)
	}
}
