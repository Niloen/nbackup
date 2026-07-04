package xfer

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"

	"github.com/Niloen/nbackup/internal/programs"
)

// bytesSource is a stat-less producing source over a fixed body.
type bytesSource struct{ body []byte }

func (s *bytesSource) Open(context.Context) (io.ReadCloser, func() (SourceStats, error), error) {
	return io.NopCloser(bytes.NewReader(s.body)), func() (SourceStats, error) {
		return SourceStats{Uncompressed: int64(len(s.body))}, nil
	}, nil
}
func (s *bytesSource) Cleanup() {}

// collectSink gathers the whole stream into memory and records whether Commit ran.
type collectSink struct {
	buf       bytes.Buffer
	committed bool
	stats     SourceStats
}

func (s *collectSink) NextPart(context.Context) (io.WriteCloser, int64, error) {
	return nopWriteCloser{&s.buf}, -1, nil // nopWriteCloser: sink.go
}
func (s *collectSink) Commit(_ context.Context, st SourceStats) error {
	s.committed = true
	s.stats = st
	return nil
}

func gzipOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skipf("gzip not available: %v", err)
	}
}

// testBody is compressible, non-repeating content so frame boundaries land at
// distinct encoded offsets.
func testBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i/7+i/301)%20)
	}
	return b
}

// TestChunkSourceFramedDecodesAsOneStream is the byte-identity acceptance: a framed
// gzip encode (child respawn per chunk) must decode back to the exact input with ONE
// stock decoder invocation — the concatenation is a valid single stream — and the
// recorded frame table must be true: decoding the encoded stream from any frame's
// encoded offset yields exactly the raw stream from that frame's raw offset.
func TestChunkSourceFramedDecodesAsOneStream(t *testing.T) {
	gzipOrSkip(t)
	body := testBody(64 * 1024)
	const frameSize = 10_000

	src := ChunkSource(&bytesSource{body: body}, NewFilters(programs.Cmd{Name: "gzip", Args: []string{"-c"}}), frameSize)
	sink := &collectSink{}
	stats, err := Transfer(context.Background(), src, NewFilters(), sink)
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if !sink.committed {
		t.Fatal("sink never committed")
	}
	encoded := sink.buf.Bytes()

	// One stock decode over the whole stream (stdlib gzip reads concatenated members
	// as one stream, exactly like `gzip -d`).
	decoded := gunzipAll(t, encoded)
	if !bytes.Equal(decoded, body) {
		t.Fatalf("framed encode did not decode back to the input (%d vs %d bytes)", len(decoded), len(body))
	}

	wantFrames := (len(body) + frameSize - 1) / frameSize
	if len(stats.Frames) != wantFrames {
		t.Fatalf("recorded %d frames, want %d", len(stats.Frames), wantFrames)
	}
	if stats.Frames[0].Raw != 0 || stats.Frames[0].Enc != 0 {
		t.Fatalf("first frame must be {0,0}, got %+v", stats.Frames[0])
	}
	// The restart property: decode from every frame boundary independently.
	for i, f := range stats.Frames {
		got := gunzipAll(t, encoded[f.Enc:])
		if !bytes.Equal(got, body[f.Raw:]) {
			t.Fatalf("frame %d: decode from Enc=%d != raw stream from Raw=%d", i, f.Enc, f.Raw)
		}
	}
}

func gunzipAll(t *testing.T, encoded []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	gz.Multistream(true)
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

// TestChunkSourceIdentityPassThrough: with no real filter stages there is nothing to
// respawn — the inner source is returned unchanged and no frames are recorded.
func TestChunkSourceIdentityPassThrough(t *testing.T) {
	inner := &bytesSource{body: []byte("plain")}
	if src := ChunkSource(inner, NewFilters(), 1024); src != Source(inner) {
		t.Fatal("empty filter chain must return the inner source unchanged")
	}
}

// TestChunkSourceChildDeathFailsAsSource is the fault-injection acceptance: an encode
// child dying mid-chunk must fail the transfer in the SOURCE zone (the frame and the
// failing program named) and the sink must never commit — no committed archive.
func TestChunkSourceChildDeathFailsAsSource(t *testing.T) {
	body := testBody(64 * 1024)
	// The child consumes a little input and dies; sh is always present.
	dying := programs.Cmd{Name: "sh", Args: []string{"-c", "head -c 100 >/dev/null; exit 3"}}
	src := ChunkSource(&bytesSource{body: body}, NewFilters(dying), 10_000)
	sink := &collectSink{}
	_, err := Transfer(context.Background(), src, NewFilters(), sink)
	if err == nil {
		t.Fatal("a dying encode child must fail the transfer")
	}
	var xe *Error
	if !errors.As(err, &xe) || xe.Role != RoleSource {
		t.Fatalf("encode faults must surface as RoleSource (filters are absorbed); got: %v", err)
	}
	if sink.committed {
		t.Fatal("the sink must not commit after an encode fault")
	}
	if got := err.Error(); !bytes.Contains([]byte(got), []byte("frame 0")) {
		t.Fatalf("the failing frame should be named, got: %v", got)
	}
}
