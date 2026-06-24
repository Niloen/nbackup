package media_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/media"
)

// updateGolden regenerates the committed golden JSON instead of asserting
// against it: `go test ./internal/media -run Golden -update`. The goldens are the
// on-medium wire shape of a Header and a Label for a fixed input; refresh them
// only when the format is changed on purpose, and review the diff.
var updateGolden = flag.Bool("update", false, "rewrite golden files")

// goldenTime is a fixed timestamp so the golden bytes are deterministic.
var goldenTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// goldenHeader is one archive header carrying every identity field, so a renamed,
// dropped, or retyped field on media.Header changes the committed golden and the
// test fails — a tripwire on the self-describing file header's wire format.
func goldenHeader() media.Header {
	return media.Header{
		Slot:      "slot-2026-01-02",
		Kind:      media.KindArchive,
		DLE:       "h-data",
		Host:      "h",
		Path:      "/data",
		Archiver:  "gnutar",
		Codec:     "none",
		Level:     1,
		BaseSlot:  "slot-2026-01-01",
		CreatedAt: goldenTime,
	}
}

// goldenLabel is one volume label carrying every field, the tape's file-0 identity.
func goldenLabel() media.Label {
	return media.Label{
		Magic:     media.LabelMagic,
		Name:      "lto-0007",
		Pool:      "lto",
		Sequence:  7,
		Epoch:     3,
		WrittenAt: goldenTime,
	}
}

// TestHeaderGolden pins the JSON wire encoding of media.Header. The encoding is
// what every Volume implementation frames a file with (disk sidecar, tape inline
// block), so a silent change to it would make older volumes unreadable.
func TestHeaderGolden(t *testing.T) {
	assertGolden(t, "header.golden.json", goldenHeader())
}

// TestLabelGolden pins the JSON wire encoding of media.Label.
func TestLabelGolden(t *testing.T) {
	assertGolden(t, "label.golden.json", goldenLabel())
}

// assertGolden marshals v the way the media package writes it (compact JSON, as
// EncodeHeader and WriteLabel do) and compares to the committed golden, or rewrites
// it under -update.
func assertGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := media.EncodeHeader(&buf, h); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != media.HeaderBlock {
		t.Fatalf("encoded header = %d bytes, want a fixed %d-byte block", buf.Len(), media.HeaderBlock)
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
	rebuilt := make([]byte, media.HeaderBlock)
	copy(rebuilt, append(bytes.TrimRight(want, "\n"), '\n'))
	got, err := media.DecodeHeader(bytes.NewReader(rebuilt))
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Errorf("decoded header = %+v, want %+v", got, h)
	}
}
