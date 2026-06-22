// Package slotio authors and reads slots on a media.Volume. It owns how a slot
// maps onto a volume's files — one file per archive plus a final seal record
// carrying the slot's metadata — so the engine supplies only backup streams and
// descriptive metadata, never positions or filenames. Compression is delegated to
// package filter (an external child process); slotio meters (checksums + counts)
// the compressed bytes on their way to the volume.
package slotio

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/filter"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/slot"
	"github.com/Niloen/nbackup/internal/xfer"
)

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

// Writer authors a single slot onto a media.Volume. Callers create archives with
// WriteArchive (safe to call concurrently) and finalize with Seal.
type Writer struct {
	vol   media.Volume
	codec string
	fopts filter.Options

	mu        sync.Mutex // guards the records below (WriteArchive may run concurrently)
	slot      *slot.Slot
	positions []archivePos // remembered for the pre-seal verify
}

type archivePos struct {
	pos    int
	sha256 string
}

// NewWriter begins authoring the given open slot onto vol, compressing archives
// with the named codec.
func NewWriter(vol media.Volume, s *slot.Slot, codec string, fopts filter.Options) (*Writer, error) {
	if _, err := filter.Ext(codec); err != nil { // validate the codec name early
		return nil, err
	}
	return &Writer{vol: vol, codec: codec, fopts: fopts, slot: s}, nil
}

// WriteArchive appends one archive file to the volume: it writes an identity
// header, then pipes the produced raw stream through the codec's compressor child
// while metering (checksum + size) the compressed bytes. It records the archive in
// the slot and returns the recorded metadata. Safe for concurrent use.
func (w *Writer) WriteArchive(spec ArchiveSpec, produce func(out io.Writer) (Produced, error)) (slot.Archive, error) {
	h := media.Header{
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

	var (
		res  Produced
		sha  string
		size int64
	)
	pos, err := w.vol.AppendFile(h, func(out io.Writer) error {
		meter := xfer.NewMeter(out)
		cw, e := filter.Compress(w.codec, meter, w.fopts)
		if e != nil {
			return e
		}
		r, e := produce(cw)
		if e != nil {
			cw.Close()
			return e
		}
		if e := cw.Close(); e != nil {
			return e
		}
		res, sha, size = r, meter.SHA256(), meter.Bytes()
		return nil
	})
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
		Compressed:   size,
		Uncompressed: res.Uncompressed,
		FileCount:    res.FileCount,
		SHA256:       sha,
		BaseSlot:     spec.BaseSlot,
		Members:      res.Members,
	}

	w.mu.Lock()
	w.slot.AddArchive(arch)
	w.positions = append(w.positions, archivePos{pos: pos, sha256: sha})
	w.mu.Unlock()
	return arch, nil
}

// ArchiveCount reports how many archives have been written so far.
func (w *Writer) ArchiveCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.slot.Archives)
}

// ArchivePosition is one archive's identity and volume position.
type ArchivePosition struct {
	DLE   string
	Level int
	Pos   int
}

// Positions returns the volume position of every archive written, for the
// catalog to index. Call after all WriteArchive calls have completed.
func (w *Writer) Positions() []ArchivePosition {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]ArchivePosition, len(w.positions))
	for i, p := range w.positions {
		a := w.slot.Archives[i]
		out[i] = ArchivePosition{DLE: a.DLE, Level: a.Level, Pos: p.pos}
	}
	return out
}

// Seal verifies every written archive against its recorded checksum, seals the
// slot, and appends the seal record (the slot's metadata) as the final file — the
// marker that makes the slot complete. The sealed slot is returned.
func (w *Writer) Seal(now time.Time) (*slot.Slot, error) {
	for _, p := range w.positions {
		got, err := w.hashFile(p.pos)
		if err != nil {
			return nil, fmt.Errorf("verify position %d: %w", p.pos, err)
		}
		if got != p.sha256 {
			return nil, fmt.Errorf("checksum mismatch at position %d before sealing", p.pos)
		}
	}
	if err := w.slot.Seal(now); err != nil {
		return nil, err
	}
	data, err := w.slot.Marshal()
	if err != nil {
		return nil, err
	}
	seal := media.Header{Slot: w.slot.ID, Kind: media.KindSeal, CreatedAt: now}
	if _, err := w.vol.AppendFile(seal, func(out io.Writer) error {
		_, e := out.Write(data)
		return e
	}); err != nil {
		return nil, err
	}
	return w.slot, nil
}

func (w *Writer) hashFile(pos int) (string, error) {
	_, rc, err := w.vol.ReadFile(pos)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	return xfer.HashReader(rc)
}
