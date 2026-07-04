package record

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestIndexRoundTrip pins the per-archive member index encode/decode cycle: a member
// list written by EncodeIndex reads back identically through DecodeIndex — offsets
// included, and an unreported offset (-1) survives as itself.
func TestIndexRoundTrip(t *testing.T) {
	members := []Member{
		{Path: "./", Off: 0}, {Path: "./etc/", Off: 512}, {Path: "./etc/hosts", Off: 1024},
		{Path: "./var/log/a.log", Off: -1},
	}
	frames := []Frame{{Raw: 0, Enc: 0}, {Raw: 268435456, Enc: 80530636}}
	var buf bytes.Buffer
	if err := EncodeIndex(&buf, Index{Members: members, Frames: frames}); err != nil {
		t.Fatalf("EncodeIndex: %v", err)
	}
	got, err := DecodeIndex(&buf)
	if err != nil {
		t.Fatalf("DecodeIndex: %v", err)
	}
	if !reflect.DeepEqual(got.Members, members) || !reflect.DeepEqual(got.Frames, frames) {
		t.Errorf("round-trip = %+v, want members %v frames %v", got, members, frames)
	}
}

// TestFrameJSONShape pins the frame table's documented on-medium form: a compact
// two-element array per frame, [[raw, enc], …].
func TestFrameJSONShape(t *testing.T) {
	data, err := json.Marshal([]Frame{{Raw: 0, Enc: 0}, {Raw: 100, Enc: 42}})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[[0,0],[100,42]]" {
		t.Fatalf("frames marshal as %s, want [[0,0],[100,42]]", data)
	}
	var back []Frame
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back[1] != (Frame{Raw: 100, Enc: 42}) {
		t.Fatalf("unmarshal = %+v", back)
	}
}

// TestDecodeIndexMalformed confirms DecodeIndex rejects non-gzip input rather than
// returning garbage.
func TestDecodeIndexMalformed(t *testing.T) {
	if _, err := DecodeIndex(strings.NewReader("not gzip data")); err == nil {
		t.Error("DecodeIndex of non-gzip input should error")
	}
}
