// Package tape implements media.Volume for tape-like media: a flat, sequential
// sequence of files addressed by file number, the first being a volume label.
// What differs between a real drive and a test/no-hardware setup is only the
// low-level positioning and block I/O, captured by the small `device` interface
// (an mt analogue). The Volume logic — header framing, file numbering, label,
// rebuild scan — is written once on top. `dir:` selects the directory-backed
// device (emulation, fully tested); `device:` selects the real mt/drive device.
package tape

import (
	"fmt"
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/media"
)

func init() {
	media.RegisterVolume("tape", func(opts media.Options) (media.Volume, error) {
		var (
			dev device
			err error
		)
		switch {
		case opts.Get("dir") != "":
			dev, err = openDir(opts.Get("dir"))
		case opts.Get("device") != "":
			dev, err = openMT(opts.Get("device"))
		default:
			return nil, fmt.Errorf("tape medium requires 'dir' (virtual tape) or 'device' (real drive)")
		}
		if err != nil {
			return nil, err
		}
		return newTape(dev, opts.Get("label"))
	})
	media.RegisterProfile("tape", media.NewVolumeProfile)
}

// device is the mt-level seam: a tape as a sequence of files addressed by number.
// Implementations emulate a directory (dirDevice) or drive a real tape (mtDevice).
type device interface {
	// writeFile appends a file at end-of-data and returns its file number.
	writeFile(write func(w io.Writer) error) (pos int, err error)
	// readFile fast-forwards to file pos and returns its bytes (caller closes).
	readFile(pos int) (io.ReadCloser, error)
	// count returns the number of files on the volume (the next file number).
	count() (int, error)
}

type tape struct {
	dev device
}

func newTape(dev device, label string) (*tape, error) {
	t := &tape{dev: dev}
	n, err := dev.count()
	if err != nil {
		return nil, err
	}
	if n == 0 { // a fresh volume gets a label at file 0
		if label == "" {
			label = "nbackup"
		}
		if _, err := t.AppendFile(media.Header{Kind: media.KindLabel, DLE: label, CreatedAt: time.Now().UTC()},
			func(io.Writer) error { return nil }); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (t *tape) Name() string { return "tape" }

// AppendFile frames an inline header block ahead of the payload (a tape cannot
// carry a sidecar) and appends it as the next file.
func (t *tape) AppendFile(h media.Header, write func(w io.Writer) error) (int, error) {
	return t.dev.writeFile(func(w io.Writer) error {
		if err := media.EncodeHeader(w, h); err != nil {
			return err
		}
		return write(w)
	})
}

// ReadFile fast-forwards to a file number and decodes its leading header; the
// returned stream is positioned at the payload.
func (t *tape) ReadFile(pos int) (media.Header, io.ReadCloser, error) {
	rc, err := t.dev.readFile(pos)
	if err != nil {
		return media.Header{}, nil, err
	}
	h, err := media.DecodeHeader(rc)
	if err != nil {
		rc.Close()
		return media.Header{}, nil, err
	}
	return h, rc, nil
}

// Files scans the whole volume reading each header. This is the catalog-rebuild
// path (a full pass, as Amanda re-reads a tape); normal reads seek by file number
// from the catalog and never call this.
func (t *tape) Files() ([]media.FileInfo, error) {
	n, err := t.dev.count()
	if err != nil {
		return nil, err
	}
	out := make([]media.FileInfo, 0, n)
	for pos := 0; pos < n; pos++ {
		h, rc, err := t.ReadFile(pos)
		if err != nil {
			return nil, err
		}
		rc.Close()
		out = append(out, media.FileInfo{Pos: pos, Header: h})
	}
	return out, nil
}

// RemoveSlot is unsupported: tape reclaims space by relabeling the whole volume,
// not by deleting individual files.
func (t *tape) RemoveSlot(string) error {
	return fmt.Errorf("tape: per-slot removal unsupported; reuse is whole-volume (relabel)")
}
