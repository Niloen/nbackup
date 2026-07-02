package record_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// updateGolden regenerates the committed golden JSON instead of asserting
// against it: `go test ./internal/record -run Golden -update`. The goldens are the
// on-medium wire shape of a Header and a Label for a fixed input; refresh them
// only when the format is changed on purpose, and review the diff.
var updateGolden = flag.Bool("update", false, "rewrite golden files")

// goldenTime is a fixed timestamp so the golden bytes are deterministic.
var goldenTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// goldenHeader is one archive header carrying every identity field, so a renamed,
// dropped, or retyped field on record.Header changes the committed golden and the
// test fails — a tripwire on the self-describing file header's wire record.
func goldenHeader() record.Header {
	return record.Header{
		Run:       "run-2026-01-02.001",
		Kind:      record.KindArchive,
		DLE:       "h-data",
		Host:      "h",
		Path:      "/data",
		Archiver:  "gnutar",
		Compress:  "none",
		Encrypt:   "none",
		Level:     1,
		BaseRun:   "run-2026-01-01.001",
		CreatedAt: goldenTime,
	}
}

// goldenArchive is one commit footer carrying every field — the per-archive marker a
// rebuild reads off the medium, the richest frozen on-medium record. Members is
// omitempty (it rides in the separate index, cleared before MarshalCommit), so the
// footer omits it.
func goldenArchive() record.Archive {
	return record.Archive{
		Run:          "run-2026-01-02.001",
		DLE:          "h-data",
		Host:         "h",
		Path:         "/data",
		Archiver:     "gnutar",
		Compress:     "none",
		Encrypt:      "none",
		Level:        1,
		Compressed:   4096,
		Uncompressed: 8192,
		FileCount:    3,
		Unreadable:   1,
		SHA256:       "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Parts:        2,
		BaseRun:      "run-2026-01-01.001",
		CreatedAt:    goldenTime,
	}
}

// goldenLabel is one volume label carrying every field, the tape's file-0 identity.
func goldenLabel() record.Label {
	return record.Label{
		Magic:     record.LabelMagic,
		Name:      "lto-0007",
		Pool:      "lto",
		Epoch:     3,
		WrittenAt: goldenTime,
	}
}

// TestHeaderGolden pins the JSON wire encoding of record.Header. The encoding is
// what every Volume implementation frames a file with (disk sidecar, tape inline
// block), so a silent change to it would make older volumes unreadable.
func TestHeaderGolden(t *testing.T) {
	assertGolden(t, "header.golden.json", goldenHeader())
}

// TestLabelGolden pins the JSON wire encoding of record.Label.
func TestLabelGolden(t *testing.T) {
	assertGolden(t, "label.golden.json", goldenLabel())
}

// TestArchiveGolden pins the JSON wire encoding of the commit footer, marshaled the
// way the writer commits it (record.MarshalCommit, indented). A renamed, dropped,
// retyped, or re-tagged field on record.Archive changes the committed golden and fails
// here — the footer's tripwire, the peer of TestHeaderGolden for the on-medium marker.
func TestArchiveGolden(t *testing.T) {
	got, err := record.MarshalCommit(goldenArchive())
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenBytes(t, "archive.golden.json", bytes.TrimRight(got, "\n"))
}

// assertGolden marshals v the way the format package writes it (compact JSON, as
// EncodeHeader and WriteLabel do) and compares to the committed golden, or rewrites
// it under -update.
func assertGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenBytes(t, name, got)
}

// assertGoldenBytes compares already-marshaled wire bytes to the committed golden, or
// rewrites it under -update. It is the shared core for records whose on-medium
// encoding is not plain compact JSON (the commit footer is indented).
func assertGoldenBytes(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	want = bytes.TrimRight(want, "\n")
	if !bytes.Equal(got, want) {
		t.Errorf("wire JSON drifted from %s\n got: %s\nwant: %s\n(if intended, regenerate with -update)", name, got, want)
	}
}

// TestHeaderBlockFraming proves the 32 KB framing round-trips without committing a
// single padded block to the repo: the golden holds only the meaningful JSON line;
// the test reconstructs the fixed-size block in code and asserts both directions.
// This is how the header format is locked without storing 32 KB of zeros.
func TestHeaderBlockFraming(t *testing.T) {
	h := goldenHeader()

	// EncodeHeader emits exactly one HeaderBlock, JSON then a newline then zero pad.
	var buf bytes.Buffer
	if err := record.EncodeHeader(&buf, h); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != record.HeaderBlock {
		t.Fatalf("encoded header = %d bytes, want a fixed %d-byte block", buf.Len(), record.HeaderBlock)
	}
	block := buf.Bytes()
	nl := bytes.IndexByte(block, '\n')
	if nl < 0 {
		t.Fatal("header block has no newline terminator")
	}
	for _, b := range block[nl+1:] {
		if b != 0 {
			t.Fatal("header block is not zero-padded after the newline")
		}
	}

	// The committed golden line must be exactly what lands on the medium.
	want, err := os.ReadFile(filepath.Join("testdata", "header.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(block[:nl], bytes.TrimRight(want, "\n")) {
		t.Errorf("framed header line != golden\n got: %s\nwant: %s", block[:nl], bytes.TrimRight(want, "\n"))
	}

	// Reconstruct a block from the golden line alone — padding in code, never on
	// disk — and decode it back: the read path older volumes depend on.
	rebuilt := make([]byte, record.HeaderBlock)
	copy(rebuilt, append(bytes.TrimRight(want, "\n"), '\n'))
	got, err := record.DecodeHeader(bytes.NewReader(rebuilt))
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Errorf("decoded header = %+v, want %+v", got, h)
	}
}

// TestHeaderLevelAlwaysEmitted: the header promises the level (README's self-describing
// story), so a level-0 full must carry an explicit "level":0 — the regression for the
// omitempty tag that made L0 headers omit the key entirely.
func TestHeaderLevelAlwaysEmitted(t *testing.T) {
	h := goldenHeader()
	h.Level = 0
	h.BaseRun = "" // a full has no base
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"level":0`)) {
		t.Errorf(`L0 header omits "level":0: %s`, b)
	}
}
