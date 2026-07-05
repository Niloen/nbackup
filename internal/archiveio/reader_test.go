package archiveio

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/media"
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

func (s *scriptOpener) open(_ FilePos, _ media.Range) (record.Header, io.ReadCloser, error) {
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

	_, err := NewReader(so.open, nil).Open(want, BareParts([]FilePos{{Pos: 0}}), media.Range{})
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
	_, err := NewReader(so.open, nil).Open(want, BareParts([]FilePos{{Pos: 0}}), media.Range{})
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
	rc, err := NewReader(so.open, nil).Open(want, BareParts([]FilePos{{Pos: 0}, {Pos: 1}}), media.Range{})
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
	rc, err := NewReader(so.open, nil).Open(want, BareParts([]FilePos{{Pos: 0}, {Pos: 1}}), media.Range{})
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
	rc, err := NewReader(so.open, nil).Open(want, BareParts([]FilePos{{Pos: 0}}), media.Range{})
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
	_, err := NewReader(nil, nil).Open(Ref{Run: "r", DLE: "d", Level: 0}, nil, media.Range{})
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
	ok, err := NewReader(so.open, nil).Verify(want, BareParts([]FilePos{{Pos: 0}}), "deadbeef")
	if ok {
		t.Fatal("VerifyParts must not report ok for a wrong-volume mount")
	}
	if err == nil || !strings.Contains(err.Error(), "wrong volume or stale catalog") {
		t.Fatalf("want the assertion fault surfaced as an error, got: %v", err)
	}
}

// TestOpenRangeAcrossParts pins the sub-range → (part, in-part slice) mapping of the
// unified Open: ranges crossing part boundaries read exactly their bytes; a ranged
// segment that covers a WHOLE part keeps that part's seal check armed (a partial
// slice reads unchecked — it cannot be checked against a whole-part seal); and a
// sub-range over sealless parts refuses, since only the seals know the part sizes.
func TestOpenRangeAcrossParts(t *testing.T) {
	want := Ref{Run: "run-r", DLE: "d", Level: 0}
	v := newMemVolume("v", 0)
	bodies := [][]byte{
		[]byte("aaaaaaaaaa"), // part 0: bytes [0,10)
		[]byte("bbbbbbbbbb"), // part 1: bytes [10,20)
		[]byte("cccc"),       // part 2: bytes [20,24)
	}
	var parts []Part
	var whole []byte
	for i, body := range bodies {
		v.hdrs[i] = archiveHeaderFor(want, i)
		v.data[i] = append([]byte(nil), body...)
		sum := sha256.Sum256(body)
		parts = append(parts, Part{
			Pos:  FilePos{Label: "v", Pos: i},
			Seal: record.PartSeal{Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])},
		})
		whole = append(whole, body...)
	}
	r := NewReader(openerOver(v), nil)

	for _, tc := range []struct {
		name string
		rng  media.Range
		want string
	}{
		{"crosses part 0 into 1", media.Range{Off: 5, Len: 10}, string(whole[5:15])},
		{"exactly one whole part", media.Range{Off: 10, Len: 10}, string(whole[10:20])},
		{"open-ended tail", media.Range{Off: 21}, string(whole[21:])},
		{"spans all three parts", media.Range{Off: 8, Len: 14}, string(whole[8:22])},
	} {
		rc, err := r.Open(want, parts, tc.rng)
		if err != nil {
			t.Fatalf("%s: Open: %v", tc.name, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || string(got) != tc.want {
			t.Fatalf("%s: read %q (err=%v), want %q", tc.name, got, err, tc.want)
		}
	}

	// A sub-range over sealless parts refuses: without seals the part sizes are
	// unknown, so the range cannot locate itself.
	if _, err := r.Open(want, Unsealed(parts), media.Range{Off: 1, Len: 2}); err == nil || !strings.Contains(err.Error(), "seals") {
		t.Fatalf("sealless sub-range should refuse, got: %v", err)
	}

	// Corrupt part 1: a ranged read whose segment covers that whole part still
	// catches it (the seal check armed on the whole-part segment)...
	v.data[1][0] ^= 0x01
	rc, err := r.Open(want, parts, media.Range{Off: 10, Len: 10})
	if err != nil {
		t.Fatalf("Open (prime) should succeed — the fault is on the stream: %v", err)
	}
	_, err = io.ReadAll(rc)
	rc.Close()
	if !errors.Is(err, ErrSealMismatch) {
		t.Fatalf("whole-part ranged read of a corrupt part: err = %v, want ErrSealMismatch", err)
	}
	// ...while a partial slice of the same part reads unchecked, as documented.
	rc, err = r.Open(want, parts, media.Range{Off: 12, Len: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("a partial segment must read unchecked, got: %v", err)
	}
	rc.Close()
}
