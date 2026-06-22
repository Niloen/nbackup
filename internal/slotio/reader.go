package slotio

import (
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

// OpenArchive opens the decompressed stream of the archive file at pos, reversing
// the codec it was written with. The caller closes the returned reader, which
// closes the decompressor child and the underlying volume file.
func (r *Reader) OpenArchive(pos int, codec string) (io.ReadCloser, error) {
	_, payload, err := r.vol.ReadFile(pos)
	if err != nil {
		return nil, err
	}
	src, err := filter.Decompress(codec, payload, r.fopts)
	if err != nil {
		payload.Close()
		return nil, err
	}
	return multiCloser{Reader: src, closers: []io.Closer{src, payload}}, nil
}

// VerifyFile re-hashes the raw payload at pos and compares it to want.
func (r *Reader) VerifyFile(pos int, want string) (bool, error) {
	_, payload, err := r.vol.ReadFile(pos)
	if err != nil {
		return false, err
	}
	defer payload.Close()
	got, err := xfer.HashReader(payload)
	if err != nil {
		return false, err
	}
	return got == want, nil
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
