package archiveio

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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
	partCap int64        // 0 = no extra cap
	last    CommitResult // the most recent archive reported via Record (stands in for the old Result())
}

// Record captures the committed archive so a test can read it back, standing in for the serial
// clerk's catalog write (this memSink is a WriteStore, not just a VolumeSink).
func (s *memSink) Record(r CommitResult) error {
	s.last = r
	return nil
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

// Bounded mirrors room()'s unbounded case: a part is capped by partCap or by a finite
// volume capacity, so only a no-cap volume with no partCap is unbounded.
func (s *memSink) Bounded() bool { return s.partCap > 0 || s.vols[s.idx].capacity > 0 }

func (s *memSink) PlaceFile(size int64) (media.Volume, string, int, error) {
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
	return func(p FilePos) (record.Header, io.ReadCloser, error) {
		v, ok := byName[p.Label]
		if !ok {
			return record.Header{}, nil, fmt.Errorf("no volume %q", p.Label)
		}
		return v.ReadFile(p.Pos)
	}
}

func writeOneArchive(t *testing.T, w *Writer, sink *memSink, dle string, body []byte) (record.Archive, ArchivePos) {
	t.Helper()
	spec := ArchiveSpec{DLE: dle, Host: "localhost", Path: "/p", Archiver: "m", Level: 0, Compress: "none"}
	aw := w.NewArchive(spec)
	if err := driveArchive(aw, body); err != nil {
		t.Fatalf("driveArchive: %v", err)
	}
	if err := aw.Commit(context.Background(), xfer.SourceStats{FileCount: 1, Uncompressed: int64(len(body)), Members: []record.Member{{Path: dle, Off: 0}}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return sink.last.Archive, sink.last.Pos
}

// driveArchive mimics xfer.Transfer's pull loop: copy body into parts (rolling between) until the
// stream is exhausted. It returns the first NextPart/copy/Close error so a test can assert on it.
func driveArchive(aw *ArchiveWriter, body []byte) error {
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

	spec := RunSpec{ID: "run-2026-06-21.001", CreatedAt: time.Unix(0, 0).UTC()}
	w := NewWriter(sink, sink, spec, nil, nil) // bounded by volume capacity (spanning) → sink.Bounded()==true

	body := []byte(strings.Repeat("abcdefgh", 25*1024/8*4)) // 100 KiB → spans v1+v2, last part on v3
	arch, apos := writeOneArchive(t, w, sink, "dle1", body)
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
	r := NewReader(openerOver(v1, v2, v3), nil, nil)
	rc, err := r.Open(Ref{Run: spec.ID, DLE: "dle1", Level: 0}, parts, nil)
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
	ok, err := r.Verify(Ref{Run: spec.ID, DLE: "dle1", Level: 0}, parts, arch.SHA256)
	if err != nil || !ok {
		t.Fatalf("Verify ok=%v err=%v", ok, err)
	}
}

// TestPartSizeSplitsWithinVolume forces several parts onto one unbounded volume via a
// part cap, then reads it back — exercising intra-volume splitting (the real-drive
// part_size path).
func TestPartSizeSplitsWithinVolume(t *testing.T) {
	v := newMemVolume("only", 0) // unbounded
	sink := &memSink{vols: []*memVolume{v}, partCap: 10 * 1024}

	spec := RunSpec{ID: "run-x", CreatedAt: time.Unix(0, 0).UTC()}
	w := NewWriter(sink, sink, spec, nil, nil)   // bounded by partCap (intra-volume split) → sink.Bounded()==true
	body := []byte(strings.Repeat("z", 55*1024)) // 55 KiB / 10 KiB ≈ 6 parts
	arch, apos := writeOneArchive(t, w, sink, "dle1", body)
	if arch.Parts < 5 {
		t.Fatalf("Parts = %d, want >= 5", arch.Parts)
	}
	r := NewReader(openerOver(v), nil, nil)
	rc, err := r.Open(Ref{Run: spec.ID, DLE: "dle1", Level: 0}, apos.Parts, nil)
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
	spec := RunSpec{ID: "run-y", CreatedAt: time.Unix(0, 0).UTC()}
	w := NewWriter(sink, sink, spec, nil, nil)

	body := []byte(strings.Repeat("q", 200*1024)) // far bigger than one volume
	err := driveArchive(w.NewArchive(ArchiveSpec{DLE: "dle1", Level: 0}), body)
	if err == nil {
		t.Fatal("expected an error when the sink cannot roll")
	}
	if !strings.Contains(err.Error(), "no further volume") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCommitRecordsUnreadable: a partial dump's omitted-file count (the producer's
// Unreadable paths) must land in the commit footer on the medium — durable, so a
// rebuild preserves the PARTIAL fact — and a copy (NewCopy) must carry it along.
func TestCommitRecordsUnreadable(t *testing.T) {
	v := newMemVolume("v1", 0)
	sink := &memSink{vols: []*memVolume{v}}
	spec := RunSpec{ID: "run-2026-06-21.001", CreatedAt: time.Unix(0, 0).UTC()}
	w := NewWriter(sink, sink, spec, nil, nil)

	body := []byte("payload of what was readable")
	aw := w.NewArchive(ArchiveSpec{DLE: "dle1", Host: "localhost", Path: "/p", Archiver: "m", Level: 0, Compress: "none"})
	if err := driveArchive(aw, body); err != nil {
		t.Fatalf("driveArchive: %v", err)
	}
	stats := xfer.SourceStats{
		FileCount:    2,
		Uncompressed: int64(len(body)),
		Members:      []record.Member{{Path: "readable.txt", Off: 0}},
		Unreadable:   []string{"/p/locked-a", "/p/locked-b"},
	}
	if err := aw.Commit(context.Background(), stats); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	arch := sink.last.Archive
	if arch.Unreadable != 2 {
		t.Fatalf("recorded Unreadable = %d, want 2", arch.Unreadable)
	}
	if !arch.Partial() {
		t.Fatal("Partial() = false for an archive with unreadable files")
	}

	// The commit footer on the medium itself must carry the count (rebuild reads it there).
	_, rc, err := v.ReadFile(sink.last.Pos.Commit.Pos)
	if err != nil {
		t.Fatalf("read commit footer: %v", err)
	}
	payload, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	footer, err := record.ParseCommit(payload)
	if err != nil {
		t.Fatalf("parse commit footer: %v", err)
	}
	if footer.Unreadable != 2 {
		t.Fatalf("footer Unreadable = %d, want 2 (the marker must be durable on the medium)", footer.Unreadable)
	}

	// A copy preserves the source's PARTIAL marker (like its stats and CreatedAt).
	v2 := newMemVolume("v2", 0)
	sink2 := &memSink{vols: []*memVolume{v2}}
	w2 := NewWriter(sink2, sink2, spec, nil, nil)
	cw := w2.NewCopy(arch)
	if err := driveArchive(cw, body); err != nil {
		t.Fatalf("copy driveArchive: %v", err)
	}
	if err := cw.Commit(context.Background(), xfer.SourceStats{}); err != nil {
		t.Fatalf("copy Commit: %v", err)
	}
	if got := sink2.last.Archive.Unreadable; got != 2 {
		t.Fatalf("copied archive Unreadable = %d, want 2", got)
	}
}

// TestPartSealsRecorded: each part of a split archive carries its own seal (size +
// SHA256), durable in the commit footer, and VerifyPart checks exactly one part —
// catching a corrupt part at that index while the others still pass. A copy re-splits
// to its own layout and re-seals (fresh seals, preserved whole-archive checksum).
func TestPartSealsRecorded(t *testing.T) {
	v := newMemVolume("only", 0)
	sink := &memSink{vols: []*memVolume{v}, partCap: 10 * 1024}
	spec := RunSpec{ID: "run-x", CreatedAt: time.Unix(0, 0).UTC()}
	w := NewWriter(sink, sink, spec, nil, nil)
	body := []byte(strings.Repeat("y", 35*1024)) // 35 KiB / 10 KiB → 4 parts
	arch, apos := writeOneArchive(t, w, sink, "dle1", body)

	if len(arch.PartSeals) != arch.Parts || arch.Parts < 3 {
		t.Fatalf("PartSeals = %d for %d parts, want aligned and split", len(arch.PartSeals), arch.Parts)
	}
	var total int64
	for i, s := range arch.PartSeals {
		if s.Size != int64(len(v.data[apos.Parts[i].Pos])) {
			t.Fatalf("seal %d Size = %d, want the part's stored size %d", i, s.Size, len(v.data[apos.Parts[i].Pos]))
		}
		total += s.Size
	}
	if total != arch.Compressed {
		t.Fatalf("seal sizes sum to %d, want Compressed %d", total, arch.Compressed)
	}

	// The footer on the medium carries the seals — a rebuild restores them from there.
	_, rc, err := v.ReadFile(apos.Commit.Pos)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(rc)
	rc.Close()
	footer, err := record.ParseCommit(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(footer.PartSeals) != arch.Parts {
		t.Fatalf("footer PartSeals = %d, want %d (seals must be durable on the medium)", len(footer.PartSeals), arch.Parts)
	}

	// VerifyPart passes per part; corrupting ONE part fails that index only.
	ref := Ref{Run: spec.ID, DLE: "dle1", Level: 0}
	r := NewReader(openerOver(v), nil, nil)
	for i, s := range arch.PartSeals {
		if ok, err := r.VerifyPart(ref, apos.Parts, i, s); err != nil || !ok {
			t.Fatalf("VerifyPart(%d) ok=%v err=%v, want pass", i, ok, err)
		}
	}
	v.data[apos.Parts[1].Pos][0] ^= 0xff // bit-rot in part 1
	if ok, _ := r.VerifyPart(ref, apos.Parts, 1, arch.PartSeals[1]); ok {
		t.Fatal("VerifyPart(1) passed on a corrupted part")
	}
	if ok, err := r.VerifyPart(ref, apos.Parts, 0, arch.PartSeals[0]); err != nil || !ok {
		t.Fatalf("VerifyPart(0) ok=%v err=%v, want pass (only part 1 is corrupt)", ok, err)
	}
	v.data[apos.Parts[1].Pos][0] ^= 0xff // restore for the copy below

	// A copy re-splits to its own layout and re-seals; the whole-archive checksum survives.
	v2 := newMemVolume("v2", 0)
	sink2 := &memSink{vols: []*memVolume{v2}, partCap: 20 * 1024} // coarser split: fewer parts
	w2 := NewWriter(sink2, sink2, spec, nil, nil)
	cw := w2.NewCopy(arch)
	if err := driveArchive(cw, body); err != nil {
		t.Fatal(err)
	}
	if err := cw.Commit(context.Background(), xfer.SourceStats{}); err != nil {
		t.Fatal(err)
	}
	carch := sink2.last.Archive
	if carch.SHA256 != arch.SHA256 {
		t.Fatal("copy changed the whole-archive checksum")
	}
	if carch.Parts >= arch.Parts || len(carch.PartSeals) != carch.Parts {
		t.Fatalf("copy Parts=%d seals=%d, want a fresh, aligned seal set for the coarser layout (source had %d)",
			carch.Parts, len(carch.PartSeals), arch.Parts)
	}
	cref := Ref{Run: spec.ID, DLE: "dle1", Level: 0}
	r2 := NewReader(openerOver(v2), nil, nil)
	for i, s := range carch.PartSeals {
		if ok, err := r2.VerifyPart(cref, sink2.last.Pos.Parts, i, s); err != nil || !ok {
			t.Fatalf("copy VerifyPart(%d) ok=%v err=%v", i, ok, err)
		}
	}
}

// TestOpenSealedCatchesCorruptPart: Open armed with seals fails the stream at the
// damaged part with ErrSealMismatch (naming the part), instead of delivering corrupt
// bytes to the consumer; unsealed Open (nil) still reads straight through — the
// pre-seal behavior for sealless archives.
func TestOpenSealedCatchesCorruptPart(t *testing.T) {
	v := newMemVolume("only", 0)
	sink := &memSink{vols: []*memVolume{v}, partCap: 8 * 1024}
	spec := RunSpec{ID: "run-x", CreatedAt: time.Unix(0, 0).UTC()}
	w := NewWriter(sink, sink, spec, nil, nil)
	body := []byte(strings.Repeat("s", 20*1024)) // 3 parts
	arch, apos := writeOneArchive(t, w, sink, "dle1", body)
	ref := Ref{Run: spec.ID, DLE: "dle1", Level: 0}
	r := NewReader(openerOver(v), nil, nil)

	// Flip one byte in the middle part — the silent kind of damage tar cannot see.
	v.data[apos.Parts[1].Pos][100] ^= 0x01

	rc, err := r.Open(ref, apos.Parts, arch.PartSeals)
	if err != nil {
		t.Fatalf("Open (prime) should succeed — the fault is mid-stream: %v", err)
	}
	_, err = io.ReadAll(rc)
	rc.Close()
	if !errors.Is(err, ErrSealMismatch) {
		t.Fatalf("sealed read err = %v, want ErrSealMismatch", err)
	}
	if !strings.Contains(err.Error(), "part 2 of 3") {
		t.Fatalf("seal error %q does not name the part", err)
	}

	// Unsealed (nil) reads through unchecked, as before seals existed.
	rc, err = r.Open(ref, apos.Parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || len(got) != len(body) {
		t.Fatalf("unsealed read n=%d err=%v, want the full (corrupt) payload", len(got), err)
	}

	// Misaligned seals are dropped, not misapplied: a short seal list reads unchecked.
	rc, err = r.Open(ref, apos.Parts, arch.PartSeals[:1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(rc); err != nil {
		t.Fatalf("misaligned seals must disarm the check, got %v", err)
	}
	rc.Close()
}
