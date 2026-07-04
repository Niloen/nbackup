package archiveio

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/record"
)

// errCloser wraps a reader with a controllable Close error, so a test can force
// the partsReader's EOF-close-error propagation branch.
type errCloser struct {
	io.Reader
	closeErr error
}

func (e errCloser) Close() error { return e.closeErr }

// scriptOpener serves a pre-scripted response per part-open call (by call order),
// so a test can inject a wrong-volume header, a mid-stream open failure, or a
// close error without a real medium.
type scriptOpener struct {
	resp []struct {
		h   record.Header
		rc  io.ReadCloser
		err error
	}
	calls int
}

func (s *scriptOpener) open(FilePos) (record.Header, io.ReadCloser, error) {
	i := s.calls
	s.calls++
	if i >= len(s.resp) {
		return record.Header{}, nil, errors.New("scriptOpener: no scripted response")
	}
	r := s.resp[i]
	return r.h, r.rc, r.err
}

func archiveHeaderFor(want Ref, part int) record.Header {
	return record.Header{Kind: record.KindArchive, Run: want.Run, DLE: want.DLE, Level: want.Level, Part: part}
}

// TestAssertPartWrongVolume is the advertised catch-all: a mounted part whose
// header names a different archive (a swapped tape / stale catalog) is rejected
// at prime time, before any bytes are trusted, with an actionable message.
func TestAssertPartWrongVolume(t *testing.T) {
	want := Ref{Run: "run-2026-06-01.001", DLE: "app01-data", Level: 0}
	// The mounted part actually holds a DIFFERENT run — the classic swapped-volume fault.
	wrong := record.Header{Kind: record.KindArchive, Run: "run-2025-01-01.001", DLE: "app01-data", Level: 0, Part: 0}
	closed := false
	so := &scriptOpener{resp: []struct {
		h   record.Header
		rc  io.ReadCloser
		err error
	}{
		{h: wrong, rc: errCloser{Reader: strings.NewReader("bytes"), closeErr: nil}},
	}}
	// Wrap so we can observe the reader is closed on the rejection path.
	so.resp[0].rc = closeSpy{ReadCloser: so.resp[0].rc, closed: &closed}

	_, err := NewReader(so.open, nil).Open(want, []FilePos{{Pos: 0}}, nil)
	if err == nil {
		t.Fatal("want a wrong-volume rejection at prime, got nil")
	}
	if !strings.Contains(err.Error(), "wrong volume or stale catalog") {
		t.Fatalf("error should flag a wrong/stale volume, got: %v", err)
	}
	if !closed {
		t.Fatal("the rejected part's stream must be closed (no leak on the fault path)")
	}
}

type closeSpy struct {
	io.ReadCloser
	closed *bool
}

func (c closeSpy) Close() error { *c.closed = true; return c.ReadCloser.Close() }

// TestAssertPartWrongKind rejects a position that holds a non-archive record
// (e.g. a commit footer) where an archive part was expected.
func TestAssertPartWrongKind(t *testing.T) {
	want := Ref{Run: "run-2026-06-01.001", DLE: "d", Level: 0}
	so := &scriptOpener{resp: []struct {
		h   record.Header
		rc  io.ReadCloser
		err error
	}{
		{h: record.Header{Kind: record.KindCommit, Run: want.Run, DLE: want.DLE, Level: want.Level}, rc: io.NopCloser(strings.NewReader(""))},
	}}
	_, err := NewReader(so.open, nil).Open(want, []FilePos{{Pos: 0}}, nil)
	if err == nil || !strings.Contains(err.Error(), "not an archive") {
		t.Fatalf("want a not-an-archive rejection, got: %v", err)
	}
}

// TestReadMidStreamOpenFailure: the first part opens and drains clean, but the
// SECOND part fails to open (a volume that won't mount mid-chain). The fault
// surfaces from Read, not swallowed as EOF.
func TestReadMidStreamOpenFailure(t *testing.T) {
	want := Ref{Run: "run-2026-06-01.001", DLE: "d", Level: 0}
	mountErr := errors.New("mount failed: drive empty")
	so := &scriptOpener{resp: []struct {
		h   record.Header
		rc  io.ReadCloser
		err error
	}{
		{h: archiveHeaderFor(want, 0), rc: io.NopCloser(strings.NewReader("part0"))},
		{err: mountErr}, // second part won't open
	}}
	rc, err := NewReader(so.open, nil).Open(want, []FilePos{{Pos: 0}, {Pos: 1}}, nil)
	if err != nil {
		t.Fatalf("prime (part 0) should succeed: %v", err)
	}
	_, err = io.ReadAll(rc)
	rc.Close()
	if !errors.Is(err, mountErr) {
		t.Fatalf("mid-stream open failure must surface from Read, got: %v", err)
	}
}

// TestReadWrongPartIndex: part 0 is fine, but the volume mounted for part 1
// holds the wrong part index (a stale catalog / reordered span) — rejected
// mid-stream before its bytes are concatenated.
func TestReadWrongPartIndex(t *testing.T) {
	want := Ref{Run: "run-2026-06-01.001", DLE: "d", Level: 0}
	so := &scriptOpener{resp: []struct {
		h   record.Header
		rc  io.ReadCloser
		err error
	}{
		{h: archiveHeaderFor(want, 0), rc: io.NopCloser(strings.NewReader("part0"))},
		{h: archiveHeaderFor(want, 7), rc: io.NopCloser(strings.NewReader("part1"))}, // wrong index (want 1)
	}}
	rc, err := NewReader(so.open, nil).Open(want, []FilePos{{Pos: 0}, {Pos: 1}}, nil)
	if err != nil {
		t.Fatalf("prime should succeed: %v", err)
	}
	_, err = io.ReadAll(rc)
	rc.Close()
	if err == nil || !strings.Contains(err.Error(), "expected part 1") {
		t.Fatalf("want a wrong-part-index rejection, got: %v", err)
	}
}

// TestReadPartCloseErrorPropagates: a part that reads clean but errors on Close
// (an EOF with no trailing bytes) surfaces the close error rather than a silent
// clean EOF — a torn read must not read as a complete archive.
func TestReadPartCloseErrorPropagates(t *testing.T) {
	want := Ref{Run: "run-2026-06-01.001", DLE: "d", Level: 0}
	closeErr := errors.New("short read on close")
	so := &scriptOpener{resp: []struct {
		h   record.Header
		rc  io.ReadCloser
		err error
	}{
		{h: archiveHeaderFor(want, 0), rc: errCloser{Reader: strings.NewReader("payload"), closeErr: closeErr}},
	}}
	rc, err := NewReader(so.open, nil).Open(want, []FilePos{{Pos: 0}}, nil)
	if err != nil {
		t.Fatalf("prime should succeed: %v", err)
	}
	_, err = io.ReadAll(rc)
	rc.Close()
	if !errors.Is(err, closeErr) {
		t.Fatalf("a part's Close error must surface from Read, got: %v", err)
	}
}

// TestOpenNoParts rejects an archive record that lists no parts (a corrupt
// catalog entry) rather than returning an empty, silently-valid stream.
func TestOpenNoParts(t *testing.T) {
	_, err := NewReader(nil, nil).Open(Ref{Run: "r", DLE: "d", Level: 0}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no parts") {
		t.Fatalf("want a no-parts error, got: %v", err)
	}
}

// TestVerifyPartsWrongVolume: VerifyParts over a swapped volume surfaces the
// assertion fault as a read error (not a false checksum verdict).
func TestVerifyPartsWrongVolume(t *testing.T) {
	want := Ref{Run: "run-2026-06-01.001", DLE: "d", Level: 0}
	so := &scriptOpener{resp: []struct {
		h   record.Header
		rc  io.ReadCloser
		err error
	}{
		{h: record.Header{Kind: record.KindArchive, Run: "other", DLE: "d", Level: 0}, rc: io.NopCloser(strings.NewReader("x"))},
	}}
	ok, err := NewReader(so.open, nil).Verify(want, []FilePos{{Pos: 0}}, "deadbeef")
	if ok {
		t.Fatal("VerifyParts must not report ok for a wrong-volume mount")
	}
	if err == nil || !strings.Contains(err.Error(), "wrong volume or stale catalog") {
		t.Fatalf("want the assertion fault surfaced as an error, got: %v", err)
	}
}
