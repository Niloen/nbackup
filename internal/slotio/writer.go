// Package slotio authors and reads slots on a media.Store. It owns the slot's
// on-media layout — archive object paths, the manifest and checksum files, and
// the seal-last protocol — so the engine supplies only backup streams and
// descriptive metadata and never has to know how a slot is laid out on storage.
// Compression is delegated to package filter (an external child process); slotio
// only meters (checksums + counts) the compressed bytes on their way to storage.
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

// Writer authors a single slot onto a media.Store. Callers create archives with
// WriteArchive (safe to call concurrently) and finalize with Seal. The on-media
// layout, compression codec, and integrity protocol are the Writer's concern.
type Writer struct {
	store media.Store
	codec string
	ext   string
	fopts filter.Options

	mu        sync.Mutex // guards the records below (WriteArchive may run concurrently)
	slot      *slot.Slot
	manifest  *slot.Manifest
	checksums map[string]string
}

// NewWriter begins authoring the given open slot onto store, compressing archives
// with the named codec.
func NewWriter(store media.Store, s *slot.Slot, codec string, fopts filter.Options) (*Writer, error) {
	ext, err := filter.Ext(codec)
	if err != nil {
		return nil, err
	}
	return &Writer{
		store:     store,
		codec:     codec,
		ext:       ext,
		fopts:     fopts,
		slot:      s,
		manifest:  &slot.Manifest{SlotID: s.ID},
		checksums: map[string]string{},
	}, nil
}

// WriteArchive streams one archive into the slot: it opens the destination
// object, meters (checksum + size) the compressed bytes, and pipes the produced
// raw stream through the codec's compressor child on the way to storage. It then
// records the archive in the slot, manifest, and checksum set, and returns the
// recorded metadata. Safe for concurrent use across DLEs.
func (w *Writer) WriteArchive(spec ArchiveSpec, produce func(out io.Writer) (Produced, error)) (slot.Archive, error) {
	rel := fmt.Sprintf("%s/%s-L%d.tar", slot.DirArchives, spec.DLE, spec.Level)
	if w.ext != "" {
		rel += "." + w.ext
	}

	obj, err := w.store.Create(w.slot.ID, rel)
	if err != nil {
		return slot.Archive{}, err
	}
	meter := xfer.NewMeter(obj)
	cw, err := filter.Compress(w.codec, meter, w.fopts)
	if err != nil {
		obj.Close()
		return slot.Archive{}, err
	}
	res, perr := produce(cw)
	closeErr := cw.Close() // finishes the compressor child; meter now holds the totals
	objCloseErr := obj.Close()
	if perr != nil {
		return slot.Archive{}, perr
	}
	if closeErr != nil {
		return slot.Archive{}, closeErr
	}
	if objCloseErr != nil {
		return slot.Archive{}, objCloseErr
	}

	arch := slot.Archive{
		DLE:          spec.DLE,
		Host:         spec.Host,
		Path:         spec.Path,
		Method:       spec.Method,
		Codec:        w.codec,
		Level:        spec.Level,
		File:         rel,
		Compressed:   meter.Bytes(),
		Uncompressed: res.Uncompressed,
		FileCount:    res.FileCount,
		SHA256:       meter.SHA256(),
		BaseSlot:     spec.BaseSlot,
	}

	w.mu.Lock()
	w.slot.AddArchive(arch)
	w.manifest.Archives = append(w.manifest.Archives, slot.ArchiveFiles{
		DLE: spec.DLE, Level: spec.Level, Files: res.Members,
	})
	w.checksums[rel] = arch.SHA256
	w.mu.Unlock()
	return arch, nil
}

// ArchiveCount reports how many archives have been written so far.
func (w *Writer) ArchiveCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.slot.Archives)
}

// Seal writes the manifest and checksum file, verifies every archive's checksum
// against what actually landed on the store, then seals the slot and writes
// SLOT.json last (the marker that makes the slot complete). The sealed slot is
// returned. Call after all WriteArchive calls have completed.
func (w *Writer) Seal(now time.Time) (*slot.Slot, error) {
	if err := w.putMarshal(slot.FileManifest, w.manifest.Marshal); err != nil {
		return nil, err
	}
	if err := w.putBytes(slot.FileChecksums, slot.FormatChecksums(w.checksums)); err != nil {
		return nil, err
	}
	if err := w.verify(); err != nil {
		return nil, err
	}
	if err := w.slot.Seal(now); err != nil {
		return nil, err
	}
	if err := w.putMarshal(slot.FileSlot, w.slot.Marshal); err != nil {
		return nil, err
	}
	return w.slot, nil
}

// verify re-reads each written archive and confirms its checksum before sealing.
func (w *Writer) verify() error {
	for rel, want := range w.checksums {
		got, err := hashObject(w.store, w.slot.ID, rel)
		if err != nil {
			return fmt.Errorf("verify %s: %w", rel, err)
		}
		if got != want {
			return fmt.Errorf("checksum mismatch for %s before sealing", rel)
		}
	}
	return nil
}

func (w *Writer) putBytes(name string, data []byte) error {
	return putBytes(w.store, w.slot.ID, name, data)
}

func (w *Writer) putMarshal(name string, marshal func() ([]byte, error)) error {
	data, err := marshal()
	if err != nil {
		return err
	}
	return w.putBytes(name, data)
}
