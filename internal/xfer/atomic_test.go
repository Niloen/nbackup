package xfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/programs"
)

// incompressibleBody is pseudo-random content (a fixed LCG, no clock/randomness), so
// each inner frame's encoded size stays near frameSize and the atom bound actually
// cuts — compressible input would pack the whole stream into one atom.
func incompressibleBody(n int) []byte {
	b := make([]byte, n)
	x := uint32(12345)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// atomCountingSink is collectSink plus a per-part cut record, so a test can assert
// one part per atom and reconstruct each atom's bytes.
type atomCountingSink struct {
	parts     [][]byte
	cur       *bytes.Buffer
	committed bool
	stats     SourceStats
}

func (s *atomCountingSink) NextPart(context.Context) (io.WriteCloser, int64, error) {
	s.cur = &bytes.Buffer{}
	return nopWriteCloser{s.cur}, -1, nil
}
func (s *atomCountingSink) Commit(_ context.Context, st SourceStats) error {
	s.committed = true
	s.stats = st
	return nil
}

// closePart is called via the writer's Close — emulate by capturing on Close.
type partCapture struct {
	s *atomCountingSink
	b *bytes.Buffer
}

func (p partCapture) Write(b []byte) (int, error) { return p.b.Write(b) }
func (p partCapture) Close() error                { p.s.parts = append(p.s.parts, p.b.Bytes()); return nil }

// TestTransferAtomsGzipSealRoundTrip drives the full atomic write path with gzip
// standing in for both the inner Full stage and the PerFrame seal (every atom is one
// complete gzip message, exactly as it would be one complete gpg message): the input
// must land as several whole atoms — one sink part each — every atom independently
// decodable, the per-atom Frames table correct, and the double decode byte-identical
// to the input.
func TestTransferAtomsGzipSealRoundTrip(t *testing.T) {
	gzipOrSkip(t)
	body := incompressibleBody(200 * 1024)
	const frameSize, atomBound = 8 * 1024, 40 * 1024

	inner := NewFilters(programs.Cmd{Name: "gzip", Args: []string{"-c"}})
	seal := programs.Cmd{Name: "gzip", Args: []string{"-c"}}
	src := AtomicSource(&bytesSource{body: body}, inner, frameSize, seal, atomBound)

	sink := &atomCountingSink{}
	// Wrap NextPart writers so each part's bytes are captured on Close.
	stats, err := TransferAtoms(context.Background(), src, sinkWithCapture{sink})
	if err != nil {
		t.Fatalf("TransferAtoms: %v", err)
	}
	if !sink.committed {
		t.Fatal("sink never committed")
	}
	if len(sink.parts) < 2 {
		t.Fatalf("got %d part(s), want several atoms (bound %d over %d encoded input)", len(sink.parts), atomBound, len(body))
	}
	if len(stats.Frames) != len(sink.parts) {
		t.Fatalf("stats.Frames = %d entries, want one per atom (%d)", len(stats.Frames), len(sink.parts))
	}
	// Each atom must be a complete message on its own — one seal-decode per atom —
	// and the concatenated plaintexts (whole inner frames) one valid stream.
	var plaintext bytes.Buffer
	var encOff int64
	for i, atom := range sink.parts {
		if stats.Frames[i].Enc != encOff {
			t.Fatalf("atom %d: Frames.Enc = %d, want %d", i, stats.Frames[i].Enc, encOff)
		}
		encOff += int64(len(atom))
		plaintext.Write(gunzipAll(t, atom))
	}
	decoded := gunzipAll(t, plaintext.Bytes())
	if !bytes.Equal(decoded, body) {
		t.Fatalf("atomic round trip: %d bytes decoded, want %d identical", len(decoded), len(body))
	}
	// The raw offsets are the member→atom map: increasing from 0, ending inside the stream.
	if stats.Frames[0].Raw != 0 {
		t.Fatalf("first atom Raw = %d, want 0", stats.Frames[0].Raw)
	}
	for i := 1; i < len(stats.Frames); i++ {
		if stats.Frames[i].Raw <= stats.Frames[i-1].Raw {
			t.Fatalf("atom raw offsets must ascend: %+v", stats.Frames)
		}
	}
}

// sinkWithCapture wraps atomCountingSink so each part's bytes are recorded on Close.
type sinkWithCapture struct{ s *atomCountingSink }

func (w sinkWithCapture) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	b := &bytes.Buffer{}
	return partCapture{s: w.s, b: b}, -1, nil
}
func (w sinkWithCapture) Commit(ctx context.Context, st SourceStats) error {
	return w.s.Commit(ctx, st)
}

// TestTransferAtomsSealDeathFailsAsSource: a seal child dying mid-atom must fail the
// transfer in the source zone (the atom named) with no commit.
func TestTransferAtomsSealDeathFailsAsSource(t *testing.T) {
	gzipOrSkip(t)
	body := testBody(64 * 1024)
	dying := programs.Cmd{Name: "sh", Args: []string{"-c", "head -c 50 >/dev/null; exit 3"}}
	src := AtomicSource(&bytesSource{body: body}, NewFilters(programs.Cmd{Name: "gzip", Args: []string{"-c"}}), 8*1024, dying, 40*1024)
	sink := &atomCountingSink{}
	_, err := TransferAtoms(context.Background(), src, sinkWithCapture{sink})
	if err == nil {
		t.Fatal("a dying seal child must fail the transfer")
	}
	var xe *Error
	if !errors.As(err, &xe) || xe.Role != RoleSource {
		t.Fatalf("seal faults must surface as RoleSource, got: %v", err)
	}
	if sink.committed {
		t.Fatal("the sink must not commit after a seal fault")
	}
	if !strings.Contains(err.Error(), "atom") {
		t.Fatalf("the failing atom should be named, got: %v", err)
	}
}

// TestTransferAtomsInnerDeathFailsAsSource: an inner (compress) child dying must
// likewise fail in the source zone with no commit.
func TestTransferAtomsInnerDeathFailsAsSource(t *testing.T) {
	gzipOrSkip(t)
	body := testBody(64 * 1024)
	dying := programs.Cmd{Name: "sh", Args: []string{"-c", "head -c 50 >/dev/null; exit 3"}}
	src := AtomicSource(&bytesSource{body: body}, NewFilters(dying), 8*1024, programs.Cmd{Name: "gzip", Args: []string{"-c"}}, 40*1024)
	sink := &atomCountingSink{}
	_, err := TransferAtoms(context.Background(), src, sinkWithCapture{sink})
	if err == nil {
		t.Fatal("a dying inner child must fail the transfer")
	}
	var xe *Error
	if !errors.As(err, &xe) || xe.Role != RoleSource {
		t.Fatalf("inner-stage faults must surface as RoleSource, got: %v", err)
	}
	if sink.committed {
		t.Fatal("the sink must not commit after an inner-stage fault")
	}
}
