// Package slotio authors and reads slots on a media.Volume. It owns how a slot maps
// onto a volume's files — one or more part files per archive plus a final seal record
// carrying the slot's metadata — so the engine supplies only an already-transformed
// payload stream and descriptive metadata, never positions or filenames. slotio knows
// nothing of compression or encryption: it meters (checksum + size) the bytes that land
// and splits them into parts. The transform pipeline (compress/encrypt) is the engine's
// to compose and run; slotio drains its output.
//
// An archive may be split into several parts across volumes (tape spanning). The writer
// drains the payload into parts sized to fit each volume's known remaining capacity,
// rolling to the next volume (via a VolumeSink) between parts. The split is PROACTIVE —
// each part is bounded before it is written — so a volume is never overfilled in the
// normal path; the media.ErrVolumeFull backstop only fires when an estimate came up short,
// and then the write fails rather than recovering.
package slotio

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
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
}

// SlotSpec is the descriptive identity of a slot to author: what is known before
// any archive is written, independent of the archives and bytes the Writer
// assembles while streaming. NewWriter starts the slot from it and Seal returns
// the finished record.Slot — so the caller describes the slot and slotio produces the
// artifact, never the reverse. The fields mirror record.NewSlot's parameters.
type SlotSpec struct {
	ID        string    // the slot's identity (see record.IDFromParts)
	Date      string    // run date, YYYY-MM-DD
	Sequence  int       // 1 for the day's first run, 2+ for later runs
	Generator string    // tool authoring the slot (e.g. "nbdump")
	CreatedAt time.Time // when authoring began; a copy preserves the source slot's
}

// Writer authors a single slot onto a medium via a VolumeSink. Callers write archive
// payloads with WriteArchive (recording each with Record) and finalize with Seal.
// WriteArchive is safe for concurrent use only on an unbounded sink (disk); a bounded,
// spanning-capable sink rolls one shared volume and must be driven serially (the engine
// clamps archivers).
type Writer struct {
	sink VolumeSink
	lim  *xfer.Limiter // optional bandwidth cap on the bytes landing on the medium (nil = uncapped)

	mu       sync.Mutex // guards the records below
	slot     *record.Slot
	written  []archiveRecord // one per recorded archive, in Record order
	sealPart record.FilePos  // where the seal landed (set by Seal)
}

// archiveRecord remembers an archive's parts so the catalog can index where each
// archive's bytes landed (see Positions).
type archiveRecord struct {
	dle   string
	level int
	parts []record.FilePos
}

// NewWriter begins authoring a new slot, described by spec, onto sink. The Writer builds
// and owns the record.Slot from spec (the caller never hands one in); Seal returns it
// sealed. lim, when non-nil, caps the rate of bytes written to the medium (network
// politeness); a nil lim is uncapped. The same lim is shared across concurrent
// WriteArchive calls on an unbounded sink, so several workers to one medium share its
// budget (Amanda's netusage).
func NewWriter(sink VolumeSink, spec SlotSpec, lim *xfer.Limiter) *Writer {
	slot := record.NewSlot(spec.ID, spec.Date, spec.Sequence, spec.Generator, spec.CreatedAt)
	return &Writer{sink: sink, lim: lim, slot: slot}
}

// WriteArchive drains the archive payload — already compressed and encrypted by the
// caller's transform pipeline — into the slot, split into as many part files as the
// loaded volumes' capacities require. It meters the bytes that land (sha256 + size) and
// returns the archive with its measured fields filled (Compressed, SHA256, Parts) plus
// the ordered part positions. It does NOT record the archive in the slot: the producer's
// raw-stream stats (Uncompressed/FileCount/Members) arrive only after the pipeline
// drains, so the caller merges those and calls Record. The descriptive fields of meta
// (DLE/Host/Path/Archiver/Compress/Encrypt/Level/BaseSlot) are taken as-is.
//
// progress, if non-nil, is called with the running compressed byte count as the payload
// drains — the live signal for `nb status`. It runs on the draining goroutine, so it
// must be cheap.
func (w *Writer) WriteArchive(meta record.Archive, payload io.Reader, progress func(compressed int64)) (record.Archive, []record.FilePos, error) {
	mr := &meteredReader{r: payload, h: sha256.New(), progress: progress}
	parts, err := w.drainParts(w.archiveHeader(meta), mr)
	if err != nil {
		return record.Archive{}, nil, err
	}
	arch := meta
	arch.Compressed = mr.n
	arch.SHA256 = hex.EncodeToString(mr.h.Sum(nil))
	arch.Parts = len(parts)
	return arch, parts, nil
}

// Record adds a completed archive (all fields final) to the slot under its part
// positions, keeping the running total and the catalog index in sync. Call it once the
// caller has merged the producer's stats into the archive WriteArchive returned.
func (w *Writer) Record(arch record.Archive, parts []record.FilePos) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.slot.AddArchive(arch)
	w.written = append(w.written, archiveRecord{dle: arch.DLE, level: arch.Level, parts: parts})
}

// meteredReader counts and hashes the bytes read through it — the streaming sha256 and
// size of the payload that lands on the medium — and reports the running total. It is
// robust to drainParts re-prepending a peeked byte: the re-prepended byte comes from a
// separate reader, so it is never hashed twice.
type meteredReader struct {
	r        io.Reader
	h        hash.Hash
	n        int64
	progress func(int64)
}

func (m *meteredReader) Read(p []byte) (int, error) {
	k, err := m.r.Read(p)
	if k > 0 {
		m.h.Write(p[:k])
		m.n += int64(k)
		if m.progress != nil {
			m.progress(m.n)
		}
	}
	return k, err
}

// archiveHeader builds the base record.Header an archive's parts share (drainParts
// clones it per part with an ascending Part index). Every framing field comes straight
// from the archive's descriptive metadata.
func (w *Writer) archiveHeader(a record.Archive) record.Header {
	return record.Header{
		Slot:      w.slot.ID,
		Kind:      record.KindArchive,
		DLE:       a.DLE,
		Host:      a.Host,
		Path:      a.Path,
		Archiver:  a.Archiver,
		Compress:  a.Compress,
		Encrypt:   a.Encrypt,
		Level:     a.Level,
		BaseSlot:  a.BaseSlot,
		CreatedAt: w.slot.CreatedAt,
	}
}

// drainParts reads the payload from src and writes it as one or more part files
// (headers cloned from base, with an ascending Part index), rolling the sink to the
// next volume whenever a part fills. It returns the ordered part positions. On error
// it returns it for the caller to handle (closing the producer); the partial files
// left behind are unsealed and ignored by a scan.
func (w *Writer) drainParts(base record.Header, src io.Reader) ([]record.FilePos, error) {
	var (
		parts []record.FilePos
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
			// Pace the write to the medium's cap. Throttling here back-pressures the
			// producer (tar → compress → encrypt) through the pipe, so the one-pass
			// pipeline slows without buffering. A nil lim leaves out untouched.
			n, e := io.Copy(w.lim.Writer(out), r)
			wrote = n
			return e
		})
		if err != nil {
			return nil, err
		}
		parts = append(parts, record.FilePos{Label: volName, Epoch: epoch, Pos: pos})
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
func (w *Writer) CopyArchive(meta record.Archive, src io.Reader) (record.Archive, error) {
	h := sha256.New()
	parts, err := w.drainParts(w.archiveHeader(meta), io.TeeReader(src, h))
	if err != nil {
		return record.Archive{}, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != meta.SHA256 {
		return record.Archive{}, fmt.Errorf("copy of %s L%d checksum mismatch (source corrupt?)", meta.DLE, meta.Level)
	}
	arch := meta
	arch.Parts = len(parts)
	w.Record(arch, parts)
	return arch, nil
}

// ArchiveCount reports how many archives have been recorded so far.
func (w *Writer) ArchiveCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.slot.Archives)
}

// Positions returns the part positions of every archive recorded, for the catalog to
// index. Call after all WriteArchive/Record calls have completed.
func (w *Writer) Positions() []record.ArchivePos {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]record.ArchivePos, len(w.written))
	for i, a := range w.written {
		out[i] = record.ArchivePos{DLE: a.dle, Level: a.level, Parts: append([]record.FilePos(nil), a.parts...)}
	}
	return out
}

// SealPosition returns where the seal record landed (its volume and position).
func (w *Writer) SealPosition() record.FilePos {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sealPart
}

// Seal seals the slot and appends the seal record (the slot's metadata) as the final
// file — the marker that makes the slot complete. The sealed slot is returned.
//
// Like Amanda's taper, sealing does not read the medium back: each archive was hashed
// inline as it streamed out (the streaming-meter sha256 recorded in the catalog), so
// the write path's integrity rests on that checksum, not a re-read. Verifying the bytes
// actually landed on the medium is the job of the explicit, operator-invoked `nb verify`
// (the amcheckdump analogue), kept out of the dump path so a single drive never has to
// re-read — or reload swapped-out volumes — just to close a slot.
func (w *Writer) Seal(now time.Time) (*record.Slot, error) {
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
	seal := record.Header{Slot: w.slot.ID, Kind: record.KindSeal, CreatedAt: now}
	pos, err := vol.AppendFile(seal, func(out io.Writer) error {
		_, e := out.Write(data)
		return e
	})
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.sealPart = record.FilePos{Label: sealVol, Epoch: sealEpoch, Pos: pos}
	w.mu.Unlock()
	return w.slot, nil
}
