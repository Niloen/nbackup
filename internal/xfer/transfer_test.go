package xfer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/programs"
)

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func reader(b []byte) Source { return Reader(io.NopCloser(bytes.NewReader(b))) }

// faultSource is a stat-less in-process source whose finish() reports a chosen error — used to
// stand in for a producer (tar) that died, or that got SIGPIPE when the sink stopped reading. It
// records whether it was reaped (finish called) and cleaned up so the failure-path bookkeeping in
// Transfer can be asserted.
type faultSource struct {
	data     []byte
	finErr   error
	finished bool
	cleaned  bool
}

func (s *faultSource) Open(context.Context) (io.ReadCloser, func() (SourceStats, error), error) {
	return io.NopCloser(bytes.NewReader(s.data)), func() (SourceStats, error) {
		s.finished = true
		return SourceStats{}, s.finErr
	}, nil
}
func (s *faultSource) Cleanup() { s.cleaned = true }

// errAfterWriter accepts up to failAfter bytes, then fails every write with failErr — a sink medium
// that dies mid-part (e.g. "medium is full"). Its Close is a no-op; the fault surfaces through Write.
type errAfterWriter struct {
	remaining int
	failErr   error
}

func (w *errAfterWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.failErr
	}
	if len(p) <= w.remaining {
		w.remaining -= len(p)
		return len(p), nil
	}
	n := w.remaining
	w.remaining = 0
	return n, w.failErr
}
func (w *errAfterWriter) Close() error { return nil }

// failSink hands out one unbounded part backed by w, then reports success at Commit — so the only
// fault it can raise is w's mid-stream write error.
type failSink struct{ w io.WriteCloser }

func (s *failSink) NextPart(context.Context) (io.WriteCloser, int64, error) { return s.w, -1, nil }
func (s *failSink) Commit(context.Context, SourceStats) error               { return nil }

// splitSink caps every part at cap bytes and records each committed part, so a test can assert the
// stream was split across the expected part boundaries (archive spanning) and reassembles intact.
type splitSink struct {
	cap   int64
	parts [][]byte
}

func (s *splitSink) NextPart(context.Context) (io.WriteCloser, int64, error) {
	return &partBuf{sink: s}, s.cap, nil
}
func (s *splitSink) Commit(context.Context, SourceStats) error { return nil }

type partBuf struct {
	bytes.Buffer
	sink *splitSink
}

func (p *partBuf) Close() error {
	p.sink.parts = append(p.sink.parts, append([]byte(nil), p.Bytes()...))
	return nil
}

// assertNoGoroutineLeak fails if the goroutine count has not settled back to at most before. It
// retries because a killed child's reaper goroutines exit slightly after the call returns.
func assertNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: %d before, %d after", before, runtime.NumGoroutine())
}

// TestHashSinkMatch: a plain reader → no filters → Hash matches (a mismatch is the
// Commit-role error TestHashSinkMismatch covers).
func TestHashSinkMatch(t *testing.T) {
	data := []byte(strings.Repeat("the quick brown fox\n", 500))
	if _, err := Transfer(context.Background(), reader(data), NewFilters(), Hash(sha(data))); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
}

// TestHashSinkMismatch: a wrong checksum is a Commit-role failure — the sink's own verdict,
// distinct from a mid-copy Sink-role fault (Hash's part writer can never itself fail to write).
func TestHashSinkMismatch(t *testing.T) {
	_, err := Transfer(context.Background(), reader([]byte("abc")), NewFilters(), Hash(sha([]byte("xyz"))))
	var te *Error
	if !errors.As(err, &te) || te.Role != RoleCommit {
		t.Fatalf("want Commit-role error, got %v", err)
	}
}

// TestFiltersRoundTrip: filters run on the local server — gzip -c | gzip -dc is identity.
func TestFiltersRoundTrip(t *testing.T) {
	if _, err := programs.Local().Command("gzip", "--version").Output(); err != nil {
		t.Skip("gzip unavailable")
	}
	data := []byte(strings.Repeat("payload-", 4096))
	f := NewFilters(programs.Cmd{Name: "gzip", Args: []string{"-c"}}).
		Add(programs.Cmd{Name: "gzip", Args: []string{"-dc"}})
	if _, err := Transfer(context.Background(), reader(data), f, Hash(sha(data))); err != nil {
		t.Fatalf("round-trip Transfer: %v", err)
	}
}

// TestFiltersFaultRole: decompressing non-gzip input fails in the Filters zone.
func TestFiltersFaultRole(t *testing.T) {
	if _, err := programs.Local().Command("gzip", "--version").Output(); err != nil {
		t.Skip("gzip unavailable")
	}
	f := NewFilters(programs.Cmd{Name: "gzip", Args: []string{"-dc"}})
	_, err := Transfer(context.Background(), reader([]byte("not gzip data")), f, Writer(io.Discard))
	var te *Error
	if !errors.As(err, &te) || te.Role != RoleFilters {
		t.Fatalf("want Filters-role error, got %v", err)
	}
}

// TestProgramSink: a program sink (cat) consumes the stream as stdin.
func TestProgramSink(t *testing.T) {
	data := []byte("hello program sink")
	sink := NewProgramSink(programs.Local()).Add(programs.Cmd{Name: "cat"})
	if _, err := Transfer(context.Background(), reader(data), NewFilters(), sink); err != nil {
		t.Fatalf("program sink Transfer: %v", err)
	}
}

// TestIsBrokenPipe: EPIPE (however wrapped) and the "signal: broken pipe" text are the pipe
// symptom; a genuine media/decode fault is not. This is the classifier the sink-first
// suppression relies on, so it is pinned directly.
func TestIsBrokenPipe(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare EPIPE", syscall.EPIPE, true},
		{"wrapped EPIPE", fmt.Errorf("tar: write pipe: %w", syscall.EPIPE), true},
		{"signal text", errors.New("signal: broken pipe"), true},
		{"write-pipe text", errors.New("write |1: broken pipe"), true},
		{"genuine media fault", errors.New("device read error: bad block 42"), false},
		{"other syscall", syscall.ENOSPC, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isBrokenPipe(c.err); got != c.want {
				t.Fatalf("isBrokenPipe(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestSinkFailsFirstSuppressesSourceBrokenPipe: the sink dies mid-part while the source, having its
// pipe closed under it, reports EPIPE. Transfer must surface the sink's REAL error, not the
// producer's broken-pipe symptom.
func TestSinkFailsFirstSuppressesSourceBrokenPipe(t *testing.T) {
	before := runtime.NumGoroutine()
	mediumFull := errors.New("medium is full; load a fresh volume")
	src := &faultSource{
		data:   bytes.Repeat([]byte("x"), 1<<20),
		finErr: fmt.Errorf("tar exited: %w", syscall.EPIPE), // producer got SIGPIPE
	}
	sink := &failSink{w: &errAfterWriter{remaining: 16, failErr: mediumFull}}

	_, err := Transfer(context.Background(), src, NewFilters(), sink)

	var te *Error
	if !errors.As(err, &te) || te.Role != RoleSink {
		t.Fatalf("want Sink-role error, got %v", err)
	}
	if !errors.Is(err, mediumFull) {
		t.Fatalf("want the sink's real error, got %v", err)
	}
	if strings.Contains(err.Error(), "broken pipe") || isBrokenPipe(err) {
		t.Fatalf("broken-pipe symptom leaked into result: %v", err)
	}
	if !src.finished || !src.cleaned {
		t.Fatalf("source not reaped/cleaned: finished=%v cleaned=%v", src.finished, src.cleaned)
	}
	assertNoGoroutineLeak(t, before)
}

// TestSinkFailsFirstSuppressesFilterBrokenPipe: the end-to-end shape — source → real filter (cat) →
// sink, where the sink fails first. The filter, still holding unwritten bytes when its output pipe
// closes, gets a broken pipe; Transfer must drop that and surface the sink's real error.
func TestSinkFailsFirstSuppressesFilterBrokenPipe(t *testing.T) {
	before := runtime.NumGoroutine()
	mediumFull := errors.New("medium is full; load a fresh volume")
	// A stream far larger than any pipe buffer, so cat is still writing when we close its output.
	data := bytes.Repeat([]byte("payload-"), 1<<20)
	filters := NewFilters(programs.Cmd{Name: "cat"})
	sink := &failSink{w: &errAfterWriter{remaining: 16, failErr: mediumFull}}

	_, err := Transfer(context.Background(), reader(data), filters, sink)

	var te *Error
	if !errors.As(err, &te) || te.Role != RoleSink {
		t.Fatalf("want Sink-role error, got %v", err)
	}
	if !errors.Is(err, mediumFull) {
		t.Fatalf("want the sink's real error, got %v", err)
	}
	if strings.Contains(err.Error(), "broken pipe") || isBrokenPipe(err) {
		t.Fatalf("filter's broken-pipe symptom leaked into result: %v", err)
	}
	assertNoGoroutineLeak(t, before)
}

// TestGenuineSourceFaultBeatsSinkSymptom: a real media-read fault at the source is NOT a broken
// pipe, so even when the sink also fails, the upstream cause wins (Source role).
func TestGenuineSourceFaultBeatsSinkSymptom(t *testing.T) {
	badBlock := errors.New("device read error: bad block 42")
	src := &faultSource{
		data:   bytes.Repeat([]byte("x"), 1<<20),
		finErr: badBlock,
	}
	sink := &failSink{w: &errAfterWriter{remaining: 16, failErr: errors.New("sink symptom")}}

	_, err := Transfer(context.Background(), src, NewFilters(), sink)

	var te *Error
	if !errors.As(err, &te) || te.Role != RoleSource {
		t.Fatalf("want Source-role error, got %v", err)
	}
	if !errors.Is(err, badBlock) {
		t.Fatalf("want the genuine media fault, got %v", err)
	}
}

// TestFilterOpenFailure: a filter whose program cannot start fails in the Filters zone, and the
// already-started source is still reaped and cleaned up (no leaked producer, no hang).
func TestFilterOpenFailure(t *testing.T) {
	before := runtime.NumGoroutine()
	src := &faultSource{data: []byte("payload")}
	filters := NewFilters(programs.Cmd{Name: "nbackup-no-such-filter-xyzzy"})

	_, err := Transfer(context.Background(), src, filters, Writer(io.Discard))

	var te *Error
	if !errors.As(err, &te) || te.Role != RoleFilters {
		t.Fatalf("want Filters-role error, got %v", err)
	}
	if !src.finished || !src.cleaned {
		t.Fatalf("source not reaped/cleaned after filter-open failure: finished=%v cleaned=%v", src.finished, src.cleaned)
	}
	assertNoGoroutineLeak(t, before)
}

// TestCopyPartBoundaries pins the bounded-part peek/roll directly: the four ways a part can end.
func TestCopyPartBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		max     int64
		wantOut string
		wantEOF bool
	}{
		{"unbounded copies all", "hello world", -1, "hello world", true},
		{"stream ends within part", "hi", 5, "hi", true},
		{"exact fill, nothing follows", "hello", 5, "hello", true},
		{"exact fill, more follows", "hello world", 5, "hello", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := bufio.NewReader(strings.NewReader(c.data))
			eof, err := copyPart(&buf, r, c.max)
			if err != nil {
				t.Fatalf("copyPart: %v", err)
			}
			if eof != c.wantEOF {
				t.Fatalf("eof = %v, want %v", eof, c.wantEOF)
			}
			if buf.String() != c.wantOut {
				t.Fatalf("wrote %q, want %q", buf.String(), c.wantOut)
			}
		})
	}
}

// TestSpanningAcrossParts: a capped sink splits the stream into bounded parts (archive spanning).
// The parts reassemble to the original, and an exact multiple of the cap must NOT leave an empty
// trailing part — that is the whole point of the one-byte peek.
func TestSpanningAcrossParts(t *testing.T) {
	const cap = 100
	for _, n := range []int{50, 100, 250, 300} {
		t.Run(fmt.Sprintf("len=%d", n), func(t *testing.T) {
			data := bytes.Repeat([]byte("z"), n)
			sink := &splitSink{cap: cap}
			if _, err := Transfer(context.Background(), reader(data), NewFilters(), sink); err != nil {
				t.Fatalf("Transfer: %v", err)
			}
			wantParts := (n + cap - 1) / cap
			if len(sink.parts) != wantParts {
				t.Fatalf("got %d parts, want %d", len(sink.parts), wantParts)
			}
			var got []byte
			for i, p := range sink.parts {
				if len(p) == 0 {
					t.Fatalf("part %d is empty", i)
				}
				if int64(len(p)) > cap {
					t.Fatalf("part %d exceeds cap: %d > %d", i, len(p), cap)
				}
				got = append(got, p...)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("reassembled %d bytes, want %d", len(got), len(data))
			}
		})
	}
}
