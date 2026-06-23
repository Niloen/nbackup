package slotio

import (
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/filter"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/xfer"
)

// Reader reads slot contents back from a media.Volume.
type Reader struct {
	vol   media.Volume
	fopts filter.Options
}

// NewReader returns a Reader over vol. fopts carries codec settings (e.g. a
// binary override) used when decompressing archives.
func NewReader(vol media.Volume, fopts filter.Options) *Reader {
	return &Reader{vol: vol, fopts: fopts}
}

// Expect is the identity a caller believes lives at a position, asserted against
// the file's actual header before its bytes are trusted. It is the cheap catch-all
// against a swapped volume or a stale catalog (the header is decoded anyway).
type Expect struct {
	Slot  string
	DLE   string
	Level int
}

// OpenArchive opens the decompressed stream of the archive file at pos, reversing
// the codec it was written with. It first asserts the file's header matches want.
// The caller closes the returned reader, which closes the decompressor child and
// the underlying volume file.
func (r *Reader) OpenArchive(pos int, codec string, want Expect) (io.ReadCloser, error) {
	h, payload, err := r.vol.ReadFile(pos)
	if err != nil {
		return nil, err
	}
	if err := assertArchive(h, want); err != nil {
		payload.Close()
		return nil, err
	}
	src, err := filter.Decompress(codec, payload, r.fopts)
	if err != nil {
		payload.Close()
		return nil, err
	}
	return multiCloser{Reader: src, closers: []io.Closer{src, payload}}, nil
}

// VerifyFile asserts the header at pos matches want, then re-hashes the raw
// payload and compares it to sha.
func (r *Reader) VerifyFile(pos int, want Expect, sha string) (bool, error) {
	h, payload, err := r.vol.ReadFile(pos)
	if err != nil {
		return false, err
	}
	defer payload.Close()
	if err := assertArchive(h, want); err != nil {
		return false, err
	}
	got, err := xfer.HashReader(payload)
	if err != nil {
		return false, err
	}
	return got == sha, nil
}

// assertArchive confirms a file's header is the archive the catalog expected. A
// mismatch means the wrong volume is mounted or the catalog is stale.
func assertArchive(h media.Header, want Expect) error {
	if h.Kind != media.KindArchive {
		return fmt.Errorf("position holds a %q record, not an archive", h.Kind)
	}
	if h.Slot != want.Slot || h.DLE != want.DLE || h.Level != want.Level {
		return fmt.Errorf("position holds %s %s L%d, expected %s %s L%d (wrong volume or stale catalog — run `nb rebuild`)",
			h.Slot, h.DLE, h.Level, want.Slot, want.DLE, want.Level)
	}
	return nil
}

// multiCloser adapts a reader plus the closers backing it into one ReadCloser.
type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m multiCloser) Close() error {
	var err error
	for _, c := range m.closers {
		if e := c.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
