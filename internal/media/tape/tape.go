// Package tape implements media.Volume for tape-like media: a flat, sequential
// sequence of files addressed by file number, the first being a volume label.
// What differs between a real drive and a test/no-hardware setup is only the
// low-level positioning and block I/O, captured by the small `device` interface
// (an mt analogue). The Volume logic — header framing, file numbering, label,
// rebuild scan — is written once on top. `dir:` selects the directory-backed
// device (emulation, fully tested); `device:` selects the real mt/drive device.
package tape

import (
	"encoding/json"
	"fmt"
	"io"

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
		return &tape{dev: dev}, nil
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
	// reset truncates the volume to empty (next write becomes file 0). It is the
	// physical basis of (re)labeling — relabel = reset then write a new file 0.
	reset() error
}

type tape struct {
	dev device
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

// ReadLabel reads the file-0 label record. A blank tape (no files) reports
// ok=false; a non-empty tape whose file 0 is not our label is foreign.
func (t *tape) ReadLabel() (media.Label, bool, error) {
	n, err := t.dev.count()
	if err != nil {
		return media.Label{}, false, err
	}
	if n == 0 {
		return media.Label{}, false, nil // blank
	}
	h, rc, err := t.ReadFile(0)
	if err != nil {
		return media.Label{}, false, err
	}
	defer rc.Close()
	if h.Kind != media.KindLabel {
		return media.Label{}, false, media.ErrForeignVolume
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		return media.Label{}, false, err
	}
	var lbl media.Label
	if err := json.Unmarshal(data, &lbl); err != nil || lbl.Magic != media.LabelMagic {
		return media.Label{}, false, media.ErrForeignVolume
	}
	return lbl, true, nil
}

// WriteLabel resets the volume and writes lbl as file 0, destroying any prior
// contents. The caller is responsible for deciding this is allowed.
func (t *tape) WriteLabel(lbl media.Label) error {
	if err := t.dev.reset(); err != nil {
		return err
	}
	lbl.Magic = media.LabelMagic
	data, err := json.Marshal(lbl)
	if err != nil {
		return err
	}
	_, err = t.AppendFile(media.Header{Kind: media.KindLabel, CreatedAt: lbl.WrittenAt},
		func(w io.Writer) error { _, e := w.Write(data); return e })
	return err
}
