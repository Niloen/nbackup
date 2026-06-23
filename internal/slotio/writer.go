// Package slotio authors and reads slots on a media.Volume. It owns how a slot
// maps onto a volume's files — one or more part files per archive plus a final seal
// record carrying the slot's metadata — so the engine supplies only backup streams
// and descriptive metadata, never positions or filenames. Compression is delegated
// to package filter (an external child process); slotio meters (checksums + counts)
// the compressed bytes on their way to the volume.
//
// An archive may be split into several parts across volumes (tape spanning). The
// writer streams the compressed payload through a pipe and drains it into parts
// sized to fit each volume's known remaining capacity, rolling to the next volume
// (via a VolumeSink) between parts. The split is PROACTIVE — each part is bounded
// before it is written — so a volume is never overfilled in the normal path; the
// media.ErrVolumeFull backstop only fires when an estimate came up short, and then
// the write fails rather than recovering.
package slotio

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/filter"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/slot"
	"github.com/Niloen/nbackup/internal/xfer"
)

// VolumeSink is the writer's view of a medium's changer: where the next part goes,
// how much fits, and how to roll between volumes. The librarian implements it (it
// owns the medium's shape and the roll). The writer never decides to roll — it asks
// for a part and writes it; the sink rolls onto a fresh volume when the loaded one is
// full, so the same call serves both "this volume has room for one more part" and
// "this volume is full, here is the next one".
type VolumeSink interface {
	// NextPart returns the volume to write the next part to and the maximum payload
	// bytes for it (its remaining capacity minus a file header, capped by part_size),
	// rolling onto a fresh volume first if the loaded one is full. A max < 0 means
	// unbounded — write the whole remaining stream as a single part. It errors when a
	// roll is needed but no further writable volume is available.
	NextPart() (vol media.Volume, max int64, volume string, epoch int, err error)
	// PlaceSeal returns the volume to write the slot's seal record (one whole file of
	// the given payload size) to, rolling first if it will not fit the loaded volume.
	PlaceSeal(size int64) (vol media.Volume, volume string, epoch int, err error)
	// Current is the loaded volume and its identity, without rolling — used to decide
	// which archives are still re-readable for the pre-seal verify.
	Current() (vol media.Volume, volume string, epoch int)
}

// Produced reports the raw-stream statistics of one archive, returned by the
// produce callback passed to Writer.WriteArchive.
type Produced struct {
	Uncompressed int64
	FileCount    int
	Members      []string
}

// ArchiveSpec is the descriptive metadata of an archive, known independently of
// the bytes the Writer measures while streaming it.
type ArchiveSpec struct {
	DLE      string
	Host     string
	Path     string
	Method   string
	Level    int
	BaseSlot string
}

// PartPosition is one part's volume and file position, for the catalog to index.
type PartPosition struct {
	Volume string
	Epoch  int
	Pos    int
}

// ArchivePositions is one archive's identity and the ordered positions of its parts.
type ArchivePositions struct {
	DLE   string
	Level int
	Parts []PartPosition
}

// Writer authors a single slot onto a medium via a VolumeSink. Callers create
// archives with WriteArchive and finalize with Seal. WriteArchive is safe for
// concurrent use only on an unbounded sink (disk); a bounded, spanning-capable sink
// rolls one shared volume and must be driven serially (the engine clamps dumpers).
type Writer struct {
	sink  VolumeSink
	codec string
	fopts filter.Options

	mu       sync.Mutex // guards the records below
	slot     *slot.Slot
	written  []archiveRecord // one per archive, in WriteArchive order
	sealPart PartPosition    // where the seal landed (set by Seal)
}

// archiveRecord remembers an archive's parts and checksum for the pre-seal verify.
type archiveRecord struct {
	dle    string
	level  int
	sha256 string
	parts  []PartPosition
}

// NewWriter begins authoring the given open slot onto sink, compressing archives
// with the named codec.
func NewWriter(sink VolumeSink, s *slot.Slot, codec string, fopts filter.Options) (*Writer, error) {
	if _, err := filter.Ext(codec); err != nil { // validate the codec name early
		return nil, err
	}
	return &Writer{sink: sink, codec: codec, fopts: fopts, slot: s}, nil
}

// WriteArchive appends one archive to the slot, split into as many part files as the
// loaded volumes' capacities require. It pipes the produced raw stream through the
// codec's compressor child, metering (checksum + size) the whole compressed stream,
// and drains that stream into parts: each part is bounded by the current volume's
// Room, and when a part fills the sink rolls to the next volume. It records the
// archive (with its part count) in the slot and returns the recorded metadata.
//
// progress, if non-nil, is called as the stream flows with the running
// (uncompressed, compressed) byte counts — the live signal for `nb status`. It runs
// on the producing goroutine, so it must be cheap.
func (w *Writer) WriteArchive(spec ArchiveSpec, progress func(uncompressed, compressed int64), produce func(out io.Writer) (Produced, error)) (slot.Archive, error) {
	// Producer: tar|compressor -> meter -> pipe. The meter sits on the whole stream
	// so the checksum/size cover the concatenation of every part. The consumer below
	// drains the pipe concurrently into volume parts.
	pr, pw := io.Pipe()
	meter := xfer.NewMeter(pw)
	var (
		res        Produced
		produceErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cw, e := filter.Compress(w.codec, meter, w.fopts)
		if e != nil {
			pw.CloseWithError(e)
			produceErr = e
			return
		}
		var src io.Writer = cw
		if progress != nil {
			src = xfer.NewCounter(cw, func(total int64) { progress(total, meter.Bytes()) })
		}
		r, e := produce(src)
		if e != nil {
			cw.Close()
			pw.CloseWithError(e)
			produceErr = e
			return
		}
		if e := cw.Close(); e != nil { // waits the compressor child; flushes the meter
			pw.CloseWithError(e)
			produceErr = e
			return
		}
		res = r
		pw.Close() // EOF to the consumer
	}()

	base := media.Header{
		Slot:      w.slot.ID,
		Kind:      media.KindArchive,
		DLE:       spec.DLE,
		Host:      spec.Host,
		Path:      spec.Path,
		Method:    spec.Method,
		Codec:     w.codec,
		Level:     spec.Level,
		BaseSlot:  spec.BaseSlot,
		CreatedAt: w.slot.CreatedAt,
	}
	parts, err := w.drainParts(base, pr)
	if err != nil {
		pr.CloseWithError(err) // unblock the producer goroutine
	}
	<-done // producer finished: meter is complete, res/produceErr are visible
	if err == nil {
		err = produceErr
	}
	if err != nil {
		return slot.Archive{}, err
	}

	arch := slot.Archive{
		DLE:          spec.DLE,
		Host:         spec.Host,
		Path:         spec.Path,
		Method:       spec.Method,
		Codec:        w.codec,
		Level:        spec.Level,
		Compressed:   meter.Bytes(),
		Uncompressed: res.Uncompressed,
		FileCount:    res.FileCount,
		SHA256:       meter.SHA256(),
		Parts:        len(parts),
		BaseSlot:     spec.BaseSlot,
		Members:      res.Members,
	}

	w.mu.Lock()
	w.slot.AddArchive(arch)
	w.written = append(w.written, archiveRecord{dle: spec.DLE, level: spec.Level, sha256: arch.SHA256, parts: parts})
	w.mu.Unlock()
	return arch, nil
}

// drainParts reads the payload from src and writes it as one or more part files
// (headers cloned from base, with an ascending Part index), rolling the sink to the
// next volume whenever a part fills. It returns the ordered part positions. On error
// it returns it for the caller to handle (closing the producer); the partial files
// left behind are unsealed and ignored by a scan.
func (w *Writer) drainParts(base media.Header, src io.Reader) ([]PartPosition, error) {
	var (
		parts []PartPosition
		part  int
	)
	for {
		vol, max, volName, epoch, err := w.sink.NextPart()
		if err != nil {
			return nil, err
		}

		h := base
		h.Part = part

		var wrote int64
		pos, err := vol.AppendFile(h, func(out io.Writer) error {
			r := src
			if max >= 0 {
				r = io.LimitReader(src, max)
			}
			n, e := io.Copy(out, r)
			wrote = n
			return e
		})
		if err != nil {
			return nil, err
		}
		parts = append(parts, PartPosition{Volume: volName, Epoch: epoch, Pos: pos})
		part++

		// Producer exhausted within this part (or the sink is unbounded): done.
		if max < 0 || wrote < max {
			return parts, nil
		}
		// Filled the part exactly at the cap — peek one byte to tell "exactly done"
		// from "more to come". On EOF the archive is complete; otherwise continue with
		// the peeked byte re-prepended (the next NextPart rolls if the volume is full,
		// or stays put when the cap was a part_size split within a volume).
		var b [1]byte
		n, perr := io.ReadFull(src, b[:])
		if n == 0 {
			if perr == io.EOF || perr == io.ErrUnexpectedEOF {
				return parts, nil
			}
			return nil, perr
		}
		src = io.MultiReader(bytes.NewReader(b[:1]), src)
	}
}

// CopyArchive writes an already-compressed archive payload (src is the source copy's
// parts concatenated) to the slot, re-split into parts sized to the target's volumes.
// It does NOT compress or re-checksum the stream — the same bytes are written, so the
// recorded checksum is unchanged — and only the part layout (and Parts count) is new.
// The header carries the archive's original codec, so restore reverses the right one.
func (w *Writer) CopyArchive(meta slot.Archive, src io.Reader) (slot.Archive, error) {
	base := media.Header{
		Slot:      w.slot.ID,
		Kind:      media.KindArchive,
		DLE:       meta.DLE,
		Host:      meta.Host,
		Path:      meta.Path,
		Method:    meta.Method,
		Codec:     meta.Codec,
		Level:     meta.Level,
		BaseSlot:  meta.BaseSlot,
		CreatedAt: w.slot.CreatedAt,
	}
	h := sha256.New()
	parts, err := w.drainParts(base, io.TeeReader(src, h))
	if err != nil {
		return slot.Archive{}, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != meta.SHA256 {
		return slot.Archive{}, fmt.Errorf("copy of %s L%d checksum mismatch (source corrupt?)", meta.DLE, meta.Level)
	}
	arch := meta
	arch.Parts = len(parts)
	w.mu.Lock()
	w.slot.AddArchive(arch)
	w.written = append(w.written, archiveRecord{dle: meta.DLE, level: meta.Level, sha256: arch.SHA256, parts: parts})
	w.mu.Unlock()
	return arch, nil
}

// ArchiveCount reports how many archives have been written so far.
func (w *Writer) ArchiveCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.slot.Archives)
}

// Positions returns the part positions of every archive written, for the catalog to
// index. Call after all WriteArchive calls have completed.
func (w *Writer) Positions() []ArchivePositions {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]ArchivePositions, len(w.written))
	for i, a := range w.written {
		out[i] = ArchivePositions{DLE: a.dle, Level: a.level, Parts: append([]PartPosition(nil), a.parts...)}
	}
	return out
}

// SealPosition returns where the seal record landed (its volume and position).
func (w *Writer) SealPosition() PartPosition {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sealPart
}

// Seal re-verifies the archives still readable on the loaded volume, seals the slot,
// and appends the seal record (the slot's metadata) as the final file — the marker
// that makes the slot complete. The sealed slot is returned.
//
// The pre-seal re-read is the belt-and-suspenders catch for a volume that silently
// mangled a write. It is kept for archives whose every part sits on the volume
// currently loaded; an archive that spanned onto an earlier, now-swapped-out volume
// cannot be re-read on a single drive, so it is trusted to its streaming-meter
// checksum (the bytes were hashed on the way out).
func (w *Writer) Seal(now time.Time) (*slot.Slot, error) {
	curVol, curName, _ := w.sink.Current()
	for _, a := range w.written {
		if !partsAllOn(a.parts, curName) {
			continue // spanned onto a swapped-out volume; trust the meter checksum
		}
		got, err := hashParts(curVol, a.parts)
		if err != nil {
			return nil, fmt.Errorf("verify %s L%d before sealing: %w", a.dle, a.level, err)
		}
		if got != a.sha256 {
			return nil, fmt.Errorf("checksum mismatch for %s L%d before sealing", a.dle, a.level)
		}
	}
	if err := w.slot.Seal(now); err != nil {
		return nil, err
	}
	data, err := w.slot.Marshal()
	if err != nil {
		return nil, err
	}
	vol, sealVol, sealEpoch, err := w.sink.PlaceSeal(int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("place the seal record: %w", err)
	}
	seal := media.Header{Slot: w.slot.ID, Kind: media.KindSeal, CreatedAt: now}
	pos, err := vol.AppendFile(seal, func(out io.Writer) error {
		_, e := out.Write(data)
		return e
	})
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.sealPart = PartPosition{Volume: sealVol, Epoch: sealEpoch, Pos: pos}
	w.mu.Unlock()
	return w.slot, nil
}

// partsAllOn reports whether every part of an archive lives on the named volume.
func partsAllOn(parts []PartPosition, volume string) bool {
	for _, p := range parts {
		if p.Volume != volume {
			return false
		}
	}
	return true
}

// hashParts reads an archive's parts in order from one volume and returns the
// sha256 of their concatenated payloads (the whole compressed stream).
func hashParts(vol media.Volume, parts []PartPosition) (string, error) {
	readers := make([]io.Reader, 0, len(parts))
	var closers []io.Closer
	for _, p := range parts {
		_, rc, err := vol.ReadFile(p.Pos)
		if err != nil {
			for _, c := range closers {
				c.Close()
			}
			return "", err
		}
		readers = append(readers, rc)
		closers = append(closers, rc)
	}
	sha, err := xfer.HashReader(io.MultiReader(readers...))
	for _, c := range closers {
		c.Close()
	}
	return sha, err
}
