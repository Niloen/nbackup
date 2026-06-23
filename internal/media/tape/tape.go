// Package tape implements media.Volume + media.Changer for tape-like media. A
// tape is a flat, sequential sequence of files addressed by file number, the
// first being a volume label. A medium is a *library* of physical bays behind one
// drive: at most one bay is mounted at a time, and writes/reads act on it.
//
// Two seams keep the hardware differences small. The `device` interface is the
// mt analogue (positioning + block I/O of one mounted tape); the `changer`
// interface is the robot analogue (which bay is in the drive). `dir:` selects a
// directory-backed library (emulation, fully tested) with a finite per-bay
// capacity; `device:` selects a real single drive (a one-bay library).
package tape

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

func init() {
	media.RegisterVolume("tape", func(opts media.Options) (media.Volume, error) {
		var ch changer
		switch {
		case opts.Get("dir") != "":
			// The emulated library is finite: tape_size caps each bay so a tape
			// fills like a real reel; `tapes` is how many physical bays exist.
			var capacity int64
			if s := opts.Get("tape_size"); s != "" {
				c, err := sizeutil.ParseBytes(s)
				if err != nil {
					return nil, fmt.Errorf("tape_size: %w", err)
				}
				capacity = c
			}
			tapes := 1
			if s := opts.Get("tapes"); s != "" {
				n, err := strconv.Atoi(s)
				if err != nil {
					return nil, fmt.Errorf("tapes: %w", err)
				}
				tapes = n
			}
			dc, err := openDirChanger(opts.Get("dir"), capacity, tapes)
			if err != nil {
				return nil, err
			}
			ch = dc
		case opts.Get("device") != "":
			dev, err := openMT(opts.Get("device"))
			if err != nil {
				return nil, err
			}
			ch = &singleChanger{dev: dev} // a real drive is a one-bay library
		default:
			return nil, fmt.Errorf("tape medium requires 'dir' (virtual tape library) or 'device' (real drive)")
		}
		t := &tape{ch: ch}
		if dev, bay, ok := ch.loaded(); ok {
			t.dev, t.bay = dev, bay
		}
		return t, nil
	})
	media.RegisterProfile("tape", media.NewVolumeProfile)
}

// device is the mt-level seam: one mounted tape as a sequence of files addressed
// by number. Implementations emulate a directory (dirDevice) or drive a real tape
// (mtDevice).
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

// changer is the robot-level seam: which physical bay is mounted in the drive.
// It addresses bays only — labels are read from the mounted device above it.
type changer interface {
	mount(bay string) (device, error)          // load a bay, persist the choice
	loaded() (dev device, bay string, ok bool) // the mounted bay, ok=false if empty
	bays() ([]media.BayStatus, error)          // inventory the library
}

// tape is one drive over a library: it forwards file I/O to the mounted bay's
// device and exposes the changer for mounting other bays.
type tape struct {
	ch  changer
	dev device // mounted bay's device; nil when the drive is empty
	bay string // mounted bay id; "" when empty
}

func (t *tape) Name() string { return "tape" }

func (t *tape) requireDev() (device, error) {
	if t.dev == nil {
		return nil, media.ErrNoTape
	}
	return t.dev, nil
}

// AppendFile frames an inline header block ahead of the payload (a tape cannot
// carry a sidecar) and appends it as the next file on the mounted bay.
func (t *tape) AppendFile(h media.Header, write func(w io.Writer) error) (int, error) {
	dev, err := t.requireDev()
	if err != nil {
		return 0, err
	}
	return dev.writeFile(func(w io.Writer) error {
		if err := media.EncodeHeader(w, h); err != nil {
			return err
		}
		return write(w)
	})
}

// ReadFile fast-forwards to a file number on the mounted bay and decodes its
// leading header; the returned stream is positioned at the payload.
func (t *tape) ReadFile(pos int) (media.Header, io.ReadCloser, error) {
	dev, err := t.requireDev()
	if err != nil {
		return media.Header{}, nil, err
	}
	rc, err := dev.readFile(pos)
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

// Files scans the whole mounted bay reading each header. This is the catalog-
// rebuild path for one tape (a full pass, as Amanda re-reads a tape); normal
// reads seek by file number from the catalog and never call this.
func (t *tape) Files() ([]media.FileInfo, error) {
	dev, err := t.requireDev()
	if err != nil {
		return nil, err
	}
	n, err := dev.count()
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

// ReadLabel reads the mounted bay's file-0 label record. A blank tape (no files)
// reports ok=false; a non-empty tape whose file 0 is not our label is foreign.
func (t *tape) ReadLabel() (media.Label, bool, error) {
	dev, err := t.requireDev()
	if err != nil {
		return media.Label{}, false, err
	}
	return readLabel(dev)
}

// WriteLabel resets the mounted bay and writes lbl as file 0, destroying any
// prior contents. The caller is responsible for deciding this is allowed.
func (t *tape) WriteLabel(lbl media.Label) error {
	dev, err := t.requireDev()
	if err != nil {
		return err
	}
	if err := dev.reset(); err != nil {
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

// Mount loads a bay into the drive (media.Changer).
func (t *tape) Mount(bay string) error {
	dev, err := t.ch.mount(bay)
	if err != nil {
		return err
	}
	t.dev, t.bay = dev, bay
	return nil
}

// Loaded reports the mounted bay (media.Changer).
func (t *tape) Loaded() (string, bool) { return t.bay, t.dev != nil }

// Bays inventories the library (media.Changer).
func (t *tape) Bays() ([]media.BayStatus, error) { return t.ch.bays() }

// readLabel decodes a mounted device's file-0 label. ok=false on a blank tape;
// ErrForeignVolume when file 0 is present but is not one of ours.
func readLabel(dev device) (media.Label, bool, error) {
	n, err := dev.count()
	if err != nil {
		return media.Label{}, false, err
	}
	if n == 0 {
		return media.Label{}, false, nil // blank
	}
	rc, err := dev.readFile(0)
	if err != nil {
		return media.Label{}, false, err
	}
	defer rc.Close()
	h, err := media.DecodeHeader(rc)
	if err != nil {
		return media.Label{}, false, err
	}
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

// singleChanger presents a real single drive as a one-bay library: there is one
// fixed position ("drive") that is always loaded. The operator swaps physical
// reels by hand; we cannot model that, so Mount only succeeds for that bay.
type singleChanger struct{ dev device }

func (s *singleChanger) mount(bay string) (device, error) { return s.dev, nil }

func (s *singleChanger) loaded() (device, string, bool) { return s.dev, "drive", true }

func (s *singleChanger) bays() ([]media.BayStatus, error) {
	n, _ := s.dev.count()
	st := media.BayStatus{Bay: "drive", Files: n, Blank: n == 0}
	if lbl, ok, _ := readLabel(s.dev); ok {
		st.Label = lbl.Name
	}
	return []media.BayStatus{st}, nil
}
