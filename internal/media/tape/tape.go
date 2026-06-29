// Package tape implements tape-like media. A tape is a flat, sequential sequence
// of files addressed by file number, the first being a volume label.
//
// One medium maps to one of three shapes. A robotic library of bays behind one drive
// (roboticChanger, a dirChanger) is a media.Changer. The two single-drive shapes —
// a real drive an operator loads by hand (driveChanger) and the disk-emulated station
// whose reels are directories the software can enumerate and load (shelfChanger, a
// manualStation) — are NOT changers: they are a media.Drive (the one loaded volume)
// plus a media.Shelf (the operator-managed room), the emulator functional and the
// real drive degenerate. All reuse the same I/O core (the `tape` struct) over a
// mounted `device` — the mt analogue (positioning + block I/O of one mounted tape).
package tape

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

func init() {
	media.RegisterVolume("tape", func(opts media.Options) (media.Volume, error) {
		switch {
		case opts.Get("dir") != "":
			// The emulated medium is finite: volume_size caps each tape so it fills
			// like a real reel.
			var capacity int64
			if s := opts.Get("volume_size"); s != "" {
				c, err := sizeutil.ParseBytes(s)
				if err != nil {
					return nil, fmt.Errorf("volume_size: %w", err)
				}
				capacity = c
			}
			// mode: manual is the single-drive station — one drive whose content the
			// operator swaps from an offline room of reels (a ShelfStation). The default
			// is the robotic library — many physical bays a robot switches between (a
			// Library). They count different things, so they use different keys: `reels`
			// (how many tapes are in the room) vs `bays` (positions).
			if opts.Get("mode") == "manual" {
				if opts.Get("bays") != "" {
					return nil, fmt.Errorf("manual tape station has a single drive, not bays; use `reels` for how many tapes are in the room")
				}
				reels, err := atoiOpt(opts.Get("reels"), 1)
				if err != nil {
					return nil, fmt.Errorf("reels: %w", err)
				}
				mc, err := openManualStation(opts.Get("dir"), capacity, reels)
				if err != nil {
					return nil, err
				}
				t := &tape{}
				if dev, reel, ok := mc.loaded(); ok {
					t.dev, t.bay = dev, reel
				}
				return &shelfChanger{tape: t, mc: mc}, nil
			}
			if opts.Get("reels") != "" {
				return nil, fmt.Errorf("`reels` applies only to a manual tape station (mode: manual); a robotic library counts `bays`")
			}
			bays, err := atoiOpt(opts.Get("bays"), 1)
			if err != nil {
				return nil, fmt.Errorf("bays: %w", err)
			}
			dc, err := openDirChanger(opts.Get("dir"), capacity, bays)
			if err != nil {
				return nil, err
			}
			t := &tape{}
			if dev, bay, ok := dc.loaded(); ok {
				t.dev, t.bay = dev, bay
			}
			return &roboticChanger{tape: t, ch: dc}, nil
		case opts.Get("device") != "":
			// A real standalone drive: one fixed device the operator loads by hand. It
			// is a Station (report what is loaded), not a Library (no addressable bays).
			var capacity int64
			if s := opts.Get("volume_size"); s != "" {
				c, err := sizeutil.ParseBytes(s)
				if err != nil {
					return nil, fmt.Errorf("volume_size: %w", err)
				}
				capacity = c
			}
			dev, err := openMT(opts.Get("device"))
			if err != nil {
				return nil, err
			}
			return &driveChanger{tape: &tape{dev: dev}, capacity: capacity}, nil
		default:
			return nil, fmt.Errorf("tape medium requires 'dir' (virtual tape library) or 'device' (real drive)")
		}
	})
	media.RegisterProfile("tape", media.NewVolumeProfile)
	media.RegisterParams("tape", "dir", "device", "bays", "volume_size", "mode", "reels", "part_size")
}

// atoiOpt parses an integer option, returning def when the value is empty.
func atoiOpt(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(s)
}

// device is the mt-level seam: one mounted tape as a sequence of files addressed
// by number. Implementations emulate a directory (dirDevice) or drive a real tape
// (mtDevice).
type device interface {
	// appendWriter begins a file at end-of-data and returns a writer for it, holding the device
	// serially until the writer is committed or aborted (a tape is one-writer-at-a-time). Commit
	// finalizes the file (filemark) and returns its number; Abort discards the partial.
	appendWriter() (deviceWriter, error)
	// readFile fast-forwards to file pos and returns its bytes (caller closes).
	readFile(pos int) (io.ReadCloser, error)
	// count returns the number of files on the volume (the next file number).
	count() (int, error)
	// reset truncates the volume to empty (next write becomes file 0). It is the
	// physical basis of (re)labeling — relabel = reset then write a new file 0.
	reset() error
	// bytesUsed reports the bytes written on the mounted volume, or 0 when the
	// device cannot see its own fill (a real drive only learns it by hitting EOT).
	bytesUsed() int64
}

// tape is the I/O core shared by all three medium shapes: it frames files and the
// label over the single currently-mounted device. The positioning surface (which
// bay/reel is in the drive) lives in the wrappers — roboticChanger, driveChanger,
// shelfChanger — that embed it.
type tape struct {
	dev device // mounted device; nil when the drive is empty
	bay string // mounted bay/reel id (for display); "" when empty
}

func (t *tape) requireDev() (device, error) {
	if t.dev == nil {
		return nil, media.ErrNoVolume
	}
	return t.dev, nil
}

// deviceWriter is one tape file's writer: the device lock is held from appendWriter until Commit
// (finalize + filemark, returning the file number) or Abort (discard the partial). A tape is
// one-writer-at-a-time, so the held lock IS the serialization.
type deviceWriter interface {
	io.Writer
	Commit() (pos int, err error)
	Abort()
}

// AppendFile frames an inline header block ahead of the payload (a tape cannot carry a sidecar) and
// appends it as the next file on the mounted bay. The device hands back a writer that holds the drive
// serially; the consumer writes the payload, and Close commits it (filemark) — or, if ctx was
// canceled, aborts (the append-only partial tail is left for the rebuild scan to ignore).
func (t *tape) AppendFile(ctx context.Context, h record.Header) (media.FileWriter, error) {
	dev, err := t.requireDev()
	if err != nil {
		return nil, err
	}
	dw, err := dev.appendWriter()
	if err != nil {
		return nil, err
	}
	if err := record.EncodeHeader(dw, h); err != nil {
		dw.Abort()
		return nil, err
	}
	return &tapeWriter{ctx: ctx, dw: dw}, nil
}

// tapeWriter is the media.FileWriter over a device writer: it adds the ctx-keyed commit/abort choice.
type tapeWriter struct {
	ctx context.Context
	dw  deviceWriter
	pos int
}

func (t *tapeWriter) Pos() int                    { return t.pos }
func (t *tapeWriter) Write(p []byte) (int, error) { return t.dw.Write(p) }

func (t *tapeWriter) Close() error {
	if t.ctx.Err() != nil {
		t.dw.Abort()
		return t.ctx.Err()
	}
	pos, err := t.dw.Commit()
	if err != nil {
		return err
	}
	t.pos = pos
	return nil
}

// ReadFile fast-forwards to a file number on the mounted bay and decodes its
// leading header; the returned stream is positioned at the payload.
func (t *tape) ReadFile(pos int) (record.Header, io.ReadCloser, error) {
	dev, err := t.requireDev()
	if err != nil {
		return record.Header{}, nil, err
	}
	rc, err := dev.readFile(pos)
	if err != nil {
		return record.Header{}, nil, err
	}
	h, err := record.DecodeHeader(rc)
	if err != nil {
		rc.Close()
		return record.Header{}, nil, err
	}
	return h, rc, nil
}

// Files scans the whole mounted bay reading each header. This is the catalog-
// rebuild path for one tape (a full pass); normal reads seek by file number from
// the catalog and never call this.
func (t *tape) Files() ([]record.FileInfo, error) {
	dev, err := t.requireDev()
	if err != nil {
		return nil, err
	}
	n, err := dev.count()
	if err != nil {
		return nil, err
	}
	out := make([]record.FileInfo, 0, n)
	for pos := 0; pos < n; pos++ {
		h, rc, err := t.ReadFile(pos)
		if err != nil {
			// A record whose header will not decode is a partial tail left by an
			// interrupted append (writes are serialized, so a partial is always last):
			// not a committed file, so skip it rather than abort the rebuild. As with
			// fslike, bit-rot on a committed record is verify's job, not enumeration's.
			continue
		}
		rc.Close()
		out = append(out, record.FileInfo{Pos: pos, Header: h})
	}
	return out, nil
}

// RemoveFile is unsupported: tape reclaims space by relabeling the whole volume,
// not by deleting individual files. It returns the shared sentinel so callers can
// fall back to whole-volume reuse (errors.Is) instead of treating it as fatal.
func (t *tape) RemoveFile(int) error {
	return fmt.Errorf("tape: %w", media.ErrNoFileRemoval)
}

// ReadLabel reads the mounted bay's file-0 label record. A blank tape (no files)
// reports ok=false; a non-empty tape whose file 0 is not our label is foreign.
func (t *tape) ReadLabel() (record.Label, bool, error) {
	dev, err := t.requireDev()
	if err != nil {
		return record.Label{}, false, err
	}
	return readLabel(dev)
}

// WriteLabel resets the mounted bay and writes lbl as file 0, destroying any
// prior contents. The caller is responsible for deciding this is allowed.
func (t *tape) WriteLabel(lbl record.Label) error {
	dev, err := t.requireDev()
	if err != nil {
		return err
	}
	if err := dev.reset(); err != nil {
		return err
	}
	lbl.Magic = record.LabelMagic
	data, err := json.Marshal(lbl)
	if err != nil {
		return err
	}
	w, err := t.AppendFile(context.Background(), record.Header{Kind: record.KindLabel, CreatedAt: lbl.WrittenAt})
	if err != nil {
		return err
	}
	_, werr := w.Write(data)
	if cerr := w.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// roboticChanger is a robotic tape library (media.Changer): a dirChanger of bays
// the "robot" mounts into the one drive, addressed by bay id. It reaches every tape
// through its bays, so it does NOT implement media.Shelf.
type roboticChanger struct {
	*tape
	ch *dirChanger
}

// Mount loads a bay into the drive (media.Changer).
func (l *roboticChanger) Mount(bay string) error {
	dev, err := l.ch.mount(bay)
	if err != nil {
		return err
	}
	l.dev, l.bay = dev, bay
	return nil
}

// Loaded reports the volume mounted in the drive (media.Drive); ok is false when
// the drive is empty.
func (l *roboticChanger) Loaded() (media.VolumeStatus, bool) {
	if l.dev == nil {
		return media.VolumeStatus{}, false
	}
	return deviceStatus(l.bay, l.dev, l.ch.capacity), true
}

// Bays inventories the library (media.Changer).
func (l *roboticChanger) Bays() ([]media.VolumeStatus, error) { return l.ch.bays() }

// driveChanger is a real standalone drive: a media.Drive (one fixed device the
// operator loads by hand) plus a degenerate media.Shelf — an empty room and an
// Insert that errors, because a human, not software, changes the reel. It is NOT a
// media.Changer: there is no robot and no bays to mount.
type driveChanger struct {
	*tape
	capacity int64
}

// Loaded reports the volume in the drive (media.Drive). A real drive is always
// "loaded" with its device; ok is false only if it could not be opened.
func (s *driveChanger) Loaded() (media.VolumeStatus, bool) {
	if s.dev == nil {
		return media.VolumeStatus{}, false
	}
	return deviceStatus("", s.dev, s.capacity), true
}

// Shelf reports an empty room (media.Shelf): software cannot enumerate a real
// drive's reels.
func (s *driveChanger) Shelf() ([]media.VolumeStatus, error) { return nil, nil }

// Insert errors (media.Shelf): only a human loads a reel into a real drive.
func (s *driveChanger) Insert(string) error {
	return fmt.Errorf("real tape drive: load the reel by hand, then retry")
}

// deviceStatus inventories one mounted device: its label, fill, and file count.
func deviceStatus(id string, dev device, capacity int64) media.VolumeStatus {
	n, _ := dev.count()
	st := media.VolumeStatus{ID: id, Capacity: capacity, Files: n, Blank: n == 0, Used: dev.bytesUsed()}
	lbl, ok, err := readLabel(dev)
	switch {
	case ok:
		st.Label, st.Pool = lbl.Name, lbl.Pool
	case err == media.ErrForeignVolume:
		// Foreign data: not blank and not writable until a forced relabel.
		st.Foreign, st.Blank = true, false
	case err != nil:
		// A corrupt or truncated header (e.g. "unexpected EOF") is neither a blank
		// tape nor safely writable — surface it as foreign so inventory and the
		// overwrite guard treat it consistently (a forced relabel reclaims it).
		st.Foreign, st.Blank = true, false
	}
	return st
}

// readLabel decodes a mounted device's file-0 label. ok=false on a blank tape;
// ErrForeignVolume when file 0 is present but is not one of ours.
func readLabel(dev device) (record.Label, bool, error) {
	// A file-backed bay that holds non-NBackup files is foreign, not blank — its own
	// files are unnumbered so they would not be counted below, and the overwrite
	// guard must refuse it rather than treat the bay as writable.
	if f, ok := dev.(interface{ foreign() bool }); ok && f.foreign() {
		return record.Label{}, false, media.ErrForeignVolume
	}
	n, err := dev.count()
	if err != nil {
		return record.Label{}, false, err
	}
	if n == 0 {
		return record.Label{}, false, nil // blank
	}
	rc, err := dev.readFile(0)
	if err != nil {
		return record.Label{}, false, err
	}
	defer rc.Close()
	h, err := record.DecodeHeader(rc)
	if err != nil {
		return record.Label{}, false, err
	}
	if h.Kind != record.KindLabel {
		return record.Label{}, false, media.ErrForeignVolume
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		return record.Label{}, false, err
	}
	var lbl record.Label
	if err := json.Unmarshal(data, &lbl); err != nil || lbl.Magic != record.LabelMagic {
		return record.Label{}, false, media.ErrForeignVolume
	}
	return lbl, true, nil
}
