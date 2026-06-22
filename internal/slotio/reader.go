package slotio

import (
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/slot"
	"github.com/Niloen/nbackup/internal/xfer"
)

// Reader reads slot contents back from a media.Store.
type Reader struct {
	store media.Store
}

// NewReader returns a Reader over store.
func NewReader(store media.Store) *Reader { return &Reader{store: store} }

// OpenArchive opens an archive's decompressed stream for restore. The caller is
// responsible for closing the returned reader, which closes both the
// decompressor and the underlying store object.
func (r *Reader) OpenArchive(slotID, file string) (io.ReadCloser, error) {
	obj, err := r.store.Open(slotID, file)
	if err != nil {
		return nil, err
	}
	src, err := xfer.NewZstdSource(obj)
	if err != nil {
		obj.Close()
		return nil, err
	}
	return multiCloser{Reader: src, closers: []io.Closer{src, obj}}, nil
}

// VerifyResult is the outcome of verifying one slot's archives.
type VerifyResult struct {
	Archives int      // number of archives checked
	Problems []string // human-readable description of each failed archive
}

// OK reports whether every archive matched its recorded checksum.
func (v VerifyResult) OK() bool { return len(v.Problems) == 0 }

// VerifySlot re-hashes every archive recorded in the slot's checksum file and
// compares it to the recorded value.
func (r *Reader) VerifySlot(slotID string) (VerifyResult, error) {
	data, err := readBytes(r.store, slotID, slot.FileChecksums)
	if err != nil {
		return VerifyResult{}, err
	}
	sums, err := slot.ParseChecksums(data)
	if err != nil {
		return VerifyResult{}, err
	}
	res := VerifyResult{Archives: len(sums)}
	for rel, want := range sums {
		got, herr := hashObject(r.store, slotID, rel)
		if herr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("%s MISSING (%v)", rel, herr))
			continue
		}
		if got != want {
			res.Problems = append(res.Problems, fmt.Sprintf("%s CHECKSUM MISMATCH", rel))
		}
	}
	return res, nil
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

// --- object I/O over a Store (shared by Writer and Reader) ---

func putBytes(store media.Store, slotID, name string, data []byte) error {
	w, err := store.Create(slotID, name)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

func readBytes(store media.Store, slotID, name string) ([]byte, error) {
	rc, err := store.Open(slotID, name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func hashObject(store media.Store, slotID, name string) (string, error) {
	rc, err := store.Open(slotID, name)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	return xfer.HashReader(rc)
}
