package archiveio

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// memVolume is a minimal in-memory media.Volume for testing the spanning writer and
// reader without a real medium. It tracks a byte budget so the sink can size parts.
type memVolume struct {
	name     string
	capacity int64 // 0 = unbounded
	used     int64
	hdrs     map[int]record.Header
	data     map[int][]byte
	next     int
}

func newMemVolume(name string, capacity int64) *memVolume {
	return &memVolume{name: name, capacity: capacity, hdrs: map[int]record.Header{}, data: map[int][]byte{}}
}

func (v *memVolume) AppendFile(ctx context.Context, h record.Header) (media.FileWriter, error) {
	return &memFileWriter{v: v, ctx: ctx, h: h}, nil
}

type memFileWriter struct {
	v   *memVolume
	ctx context.Context
	h   record.Header
	buf bytes.Buffer
	pos int
}

func (w *memFileWriter) Pos() int                    { return w.pos }
func (w *memFileWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *memFileWriter) Close() error {
	if w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	if w.v.capacity > 0 && w.v.used+record.HeaderBlock+int64(w.buf.Len()) > w.v.capacity {
		return media.ErrVolumeFull // backstop: proactive sizing should avoid this
	}
	pos := w.v.next
	w.v.next++
	w.v.hdrs[pos] = w.h
	w.v.data[pos] = append([]byte(nil), w.buf.Bytes()...)
	w.v.used += record.HeaderBlock + int64(w.buf.Len())
	w.pos = pos
	return nil
}

func (v *memVolume) ReadFile(pos int) (record.Header, io.ReadCloser, error) {
	d, ok := v.data[pos]
	if !ok {
		return record.Header{}, nil, fmt.Errorf("no file at %d", pos)
	}
	return v.hdrs[pos], io.NopCloser(bytes.NewReader(d)), nil
}

func (v *memVolume) Files() ([]record.FileInfo, error) {
	out := make([]record.FileInfo, 0, len(v.hdrs))
	for pos := 0; pos < v.next; pos++ {
		if h, ok := v.hdrs[pos]; ok {
			out = append(out, record.FileInfo{Pos: pos, Header: h})
		}
	}
	return out, nil
}

func (v *memVolume) RemoveFile(pos int) error {
	delete(v.hdrs, pos)
	delete(v.data, pos)
	return nil
}

// memSink rolls a writer across a list of memVolumes, sizing each part to the loaded
// volume's remaining capacity (optionally capped by partCap to force intra-volume
// splits).
type memSink struct {
	vols    []*memVolume
	idx     int
	partCap int64 // 0 = no extra cap
}

// room is the max payload bytes for the next part on the loaded volume: >=0 when
// bounded (clamped, never negative), -1 when unbounded.
func (s *memSink) room() int64 {
	v := s.vols[s.idx]
	if v.capacity <= 0 {
		if s.partCap > 0 {
			return s.partCap
		}
		return -1
	}
	room := v.capacity - v.used - record.HeaderBlock
	if room < 0 {
		room = 0
	}
	if s.partCap > 0 && s.partCap < room {
		return s.partCap
	}
	return room
}

func (s *memSink) advance() error {
	if s.idx+1 >= len(s.vols) {
		return fmt.Errorf("memSink: no further volume")
	}
	s.idx++
	return nil
}

func (s *memSink) NextPart() (media.Volume, int64, string, int, error) {
	// Roll only when the loaded volume cannot hold a header plus a byte.
	for r := s.room(); r >= 0 && r < 1; r = s.room() {
		if err := s.advance(); err != nil {
			return nil, 0, "", 0, err
		}
	}
	v := s.vols[s.idx]
	return v, s.room(), v.name, 1, nil
}

func (s *memSink) PlaceRecord(size int64) (media.Volume, string, int, error) {
	if r := s.room(); r >= 0 && size > r {
		if err := s.advance(); err != nil {
			return nil, "", 0, err
		}
	}
	v := s.vols[s.idx]
	return v, v.name, 1, nil
}

// partOpener over a fixed set of named volumes — the read side mounts the volume each
// part names and reads its position.
func openerOver(vols ...*memVolume) PartOpener {
	byName := map[string]*memVolume{}
	for _, v := range vols {
		byName[v.name] = v
	}
	return func(p record.FilePos) (record.Header, io.ReadCloser, error) {
		v, ok := byName[p.Label]
		if !ok {
			return record.Header{}, nil, fmt.Errorf("no volume %q", p.Label)
		}
		return v.ReadFile(p.Pos)
	}
}

func writeOneArchive(t *testing.T, w *Writer, dle string, body []byte) (record.Archive, record.ArchivePos) {
	t.Helper()
	spec := ArchiveSpec{DLE: dle, Host: "localhost", Path: "/p", Archiver: "m", Level: 0, Compress: "none"}
	aw := w.NewArchive(spec, nil)
	if err := driveArchive(aw, body); err != nil {
		t.Fatalf("driveArchive: %v", err)
	}
	if err := aw.Commit(context.Background(), xfer.Produced{FileCount: 1, Uncompressed: int64(len(body)), Members: []string{dle}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	arch, pos := aw.Result()
	return arch, pos
}

// driveArchive mimics xfer.Transfer's pull loop: copy body into parts (rolling between) until the
// stream is exhausted. It returns the first NextPart/copy/Close error so a test can assert on it.
func driveArchive(aw ArchiveWriter, body []byte) error {
	r := bufio.NewReader(bytes.NewReader(body))
	for {
		pw, max, err := aw.NextPart(context.Background())
		if err != nil {
			return err
		}
		eof := false
		var copyErr error
		if max < 0 {
			_, copyErr = io.Copy(pw, r)
			eof = true
		} else if _, copyErr = io.CopyN(pw, r, max); copyErr == io.EOF {
			eof, copyErr = true, nil
		} else if copyErr == nil {
			if _, pe := r.Peek(1); pe == io.EOF {
				eof = true
			} else if pe != nil {
				copyErr = pe
			}
		}
		if copyErr != nil {
			pw.Close()
			return copyErr
		}
		if err := pw.Close(); err != nil {
			return err
		}
		if eof {
			return nil
		}
	}
}

// TestSpanAcrossVolumes writes an archive larger than one volume and confirms it
// lands as multiple parts across volumes, then reads it back byte-for-byte.
func TestSpanAcrossVolumes(t *testing.T) {
	// Three 96 KiB volumes (64 KiB usable payload each after the 32 KiB header).
	cap := int64(96 * 1024)
	v1, v2, v3 := newMemVolume("v1", cap), newMemVolume("v2", cap), newMemVolume("v3", cap)
	sink := &memSink{vols: []*memVolume{v1, v2, v3}}

	spec := SlotSpec{ID: "slot-2026-06-21", Date: "2026-06-21", Sequence: 1, Generator: "test", CreatedAt: time.Unix(0, 0).UTC()}
	w := NewWriter(sink, spec, nil, nil)

	body := []byte(strings.Repeat("abcdefgh", 25*1024/8*4)) // 100 KiB → spans v1+v2, last part on v3
	arch, apos := writeOneArchive(t, w, "dle1", body)
	if arch.Parts < 2 {
		t.Fatalf("Parts = %d, want >= 2 (the archive must span)", arch.Parts)
	}

	parts := apos.Parts
	if len(parts) != arch.Parts {
		t.Fatalf("recorded %d parts, archive says %d", len(parts), arch.Parts)
	}
	vols := map[string]bool{}
	for _, p := range parts {
		vols[p.Label] = true
	}
	if len(vols) < 2 {
		t.Fatalf("parts landed on a single volume %v; did not span", vols)
	}

	// Read the archive back by concatenating its parts; it must equal the input.
	r := NewReader()
	rc, err := r.Open(parts, Expect{Slot: spec.ID, DLE: "dle1", Level: 0}, openerOver(v1, v2, v3))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes, want %d", len(got), len(body))
	}

	// VerifyParts must confirm the recorded checksum over the concatenation.
	ok, err := r.VerifyParts(parts, Expect{Slot: spec.ID, DLE: "dle1", Level: 0}, arch.SHA256, openerOver(v1, v2, v3))
	if err != nil || !ok {
		t.Fatalf("VerifyParts ok=%v err=%v", ok, err)
	}
}

// TestPartSizeSplitsWithinVolume forces several parts onto one unbounded volume via a
// part cap, then reads it back — exercising intra-volume splitting (the real-drive
// part_size path).
func TestPartSizeSplitsWithinVolume(t *testing.T) {
	v := newMemVolume("only", 0) // unbounded
	sink := &memSink{vols: []*memVolume{v}, partCap: 10 * 1024}

	spec := SlotSpec{ID: "slot-x", Date: "2026-06-21", Sequence: 1, Generator: "test", CreatedAt: time.Unix(0, 0).UTC()}
	w := NewWriter(sink, spec, nil, nil)
	body := []byte(strings.Repeat("z", 55*1024)) // 55 KiB / 10 KiB ≈ 6 parts
	arch, apos := writeOneArchive(t, w, "dle1", body)
	if arch.Parts < 5 {
		t.Fatalf("Parts = %d, want >= 5", arch.Parts)
	}
	r := NewReader()
	rc, err := r.Open(apos.Parts, Expect{Slot: spec.ID, DLE: "dle1", Level: 0}, openerOver(v))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes, want %d", len(got), len(body))
	}
}

// TestRollFailureNoDeadlock confirms that when the sink cannot Advance mid-archive,
// WriteArchive surfaces the error (rather than hanging) — the writer no longer runs a
// producer goroutine, so it must simply return the sink's roll failure.
func TestRollFailureNoDeadlock(t *testing.T) {
	v := newMemVolume("v1", 96*1024) // one small volume, no room to roll
	sink := &memSink{vols: []*memVolume{v}}
	spec := SlotSpec{ID: "slot-y", Date: "2026-06-21", Sequence: 1, Generator: "test", CreatedAt: time.Unix(0, 0).UTC()}
	w := NewWriter(sink, spec, nil, nil)

	body := []byte(strings.Repeat("q", 200*1024)) // far bigger than one volume
	err := driveArchive(w.NewArchive(ArchiveSpec{DLE: "dle1", Level: 0}, nil), body)
	if err == nil {
		t.Fatal("expected an error when the sink cannot roll")
	}
	if !strings.Contains(err.Error(), "no further volume") {
		t.Fatalf("unexpected error: %v", err)
	}
}
