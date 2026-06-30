// Package tape implements tape-like media. A tape (cartridge) is a flat, sequential
// sequence of files addressed by file number, the first being a volume label.
//
// One medium is a media.Changer: a set of drives (data-transfer elements) fed from a
// set of slots (storage elements). A `loader` is the changer's backend — it
// inventories slots by barcode and binds a cartridge to a drive, producing the
// `device` (the mt analogue: positioning + block I/O of one mounted cartridge) the
// drive reads and writes. Two loaders exist: a file-backed one (dirLoader: slots are
// subdirectories, drives are persisted load-pointers; it can simulate either a robot
// or a hand-loaded drive), and a real single drive (realDriveLoader: one mtDevice, no
// slots, a human loads — media.ErrManualLoad). The changer (tapeChanger) is also a
// media.Volume that proxies the active drive, so the medium handle is a Volume above
// the librarian while the librarian uses the Changer facet for logistics.
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
	media.Register(media.Spec{
		Type: "tape",
		New:  newTapeVolume,
		// A library of removable reels: capacity is known (volumes * volume_size) but
		// reclamation is deferred to label rotation, so no concurrent-write capability —
		// a serial, whole-volume medium.
		Profile: media.NewVolumeProfile,
		Params:  []string{"dir", "device", "slots", "drives", "manual", "volume_size", "part_size", "block_size"},
	})
}

// newTapeVolume constructs a tape changer: a file-backed library/station rooted at
// `dir`, or a real drive at `device`. Either way the result is a tapeChanger, which
// is both a media.Changer (the librarian's logistics) and a media.Volume (the active
// drive's cartridge, so the medium handle is a Volume above the librarian).
func newTapeVolume(opts media.Options) (media.Volume, error) {
	// volume_size caps each emulated cartridge so it fills like a real reel; a real
	// drive reports EOT only by hitting it, so capacity there stays 0 (use part_size).
	var capacity int64
	if s := opts.Get("volume_size"); s != "" {
		c, err := sizeutil.ParseBytes(s)
		if err != nil {
			return nil, fmt.Errorf("volume_size: %w", err)
		}
		capacity = c
	}
	switch {
	case opts.Get("dir") != "":
		slots, err := atoiOpt(opts.Get("slots"), 1)
		if err != nil {
			return nil, fmt.Errorf("slots: %w", err)
		}
		drives, err := atoiOpt(opts.Get("drives"), 1)
		if err != nil {
			return nil, fmt.Errorf("drives: %w", err)
		}
		manual, err := boolOpt(opts.Get("manual"))
		if err != nil {
			return nil, fmt.Errorf("manual: %w", err)
		}
		ld, err := openDirLoader(opts.Get("dir"), capacity, slots, drives, manual)
		if err != nil {
			return nil, err
		}
		return newTapeChanger(ld, capacity)
	case opts.Get("device") != "":
		for _, k := range []string{"slots", "drives", "manual"} {
			if opts.Get(k) != "" {
				return nil, fmt.Errorf("`%s` applies only to a file-backed library (dir:); a real drive (device:) is a single hand-loaded drive", k)
			}
		}
		var block int
		if s := opts.Get("block_size"); s != "" {
			b, err := sizeutil.ParseBytes(s)
			if err != nil {
				return nil, fmt.Errorf("block_size: %w", err)
			}
			block = int(b)
		}
		dev, err := openMT(opts.Get("device"), block)
		if err != nil {
			return nil, err
		}
		return newTapeChanger(&realDriveLoader{dev: dev}, capacity)
	default:
		return nil, fmt.Errorf("tape medium requires 'dir' (file-backed library) or 'device' (real drive)")
	}
}

// atoiOpt parses an integer option, returning def when the value is empty.
func atoiOpt(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(s)
}

// boolOpt parses a boolean option, defaulting to false when empty.
func boolOpt(s string) (bool, error) {
	if s == "" {
		return false, nil
	}
	return strconv.ParseBool(s)
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

// tape is the per-cartridge I/O core: it frames files and the label over one mounted
// device (the cartridge currently bound to a drive). One tape backs each tapeDrive;
// the positioning surface (which cartridge is in which drive) lives in the tapeChanger.
type tape struct {
	dev device // mounted device; nil when the drive is empty
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

// loader is a changer's backend: it inventories cartridges by slot/barcode and binds
// a cartridge to a drive, producing the device the drive reads and writes. The
// file-backed dirLoader maps slots to subdirectories; realDriveLoader is one real
// drive a human loads (no slots). A future mtx loader would drive a SCSI robot.
type loader interface {
	driveCount() int
	manual() bool
	slots() ([]media.SlotStatus, error)
	// load binds the cartridge in slot to drive, returning its device and barcode.
	load(slot, drive int) (dev device, barcode string, err error)
	unload(drive int) error
	// loaded reports drive's current binding (device, barcode, home slot), if any.
	loaded(drive int) (dev device, barcode string, fromSlot int, ok bool)
}

// tapeDrive is one data-transfer element: the per-cartridge I/O core (tape) over the
// device currently bound to it, plus the bound cartridge's barcode and home slot. It
// is a media.Drive (a Volume that reports what is loaded).
type tapeDrive struct {
	*tape
	barcode  string
	fromSlot int
	capacity int64
}

// Loaded reports the cartridge in this drive (media.Drive); ok is false when empty.
func (d *tapeDrive) Loaded() (media.VolumeStatus, bool) {
	if d.dev == nil {
		return media.VolumeStatus{}, false
	}
	st := deviceStatus("", d.dev, d.capacity)
	st.Barcode = d.barcode
	return st, true
}

// tapeChanger is a tape library: K drives fed from a loader's slots. It is a
// media.Changer (the librarian's logistics) and — by embedding drive 0 — a
// media.Volume/Labeled that proxies the active drive, so the medium handle is a
// Volume above the librarian while the librarian uses the Changer facet below.
type tapeChanger struct {
	*tapeDrive // drive 0: the active-drive Volume/Labeled facet (drives[0])
	drives     []*tapeDrive
	ld         loader
}

// newTapeChanger builds the K drives from the loader's initial (persisted) state.
func newTapeChanger(ld loader, capacity int64) (*tapeChanger, error) {
	drives := make([]*tapeDrive, ld.driveCount())
	for i := range drives {
		d := &tapeDrive{tape: &tape{}, fromSlot: -1, capacity: capacity}
		if dev, barcode, slot, ok := ld.loaded(i); ok {
			d.dev, d.barcode, d.fromSlot = dev, barcode, slot
		}
		drives[i] = d
	}
	return &tapeChanger{tapeDrive: drives[0], drives: drives, ld: ld}, nil
}

func (c *tapeChanger) Manual() bool                       { return c.ld.manual() }
func (c *tapeChanger) Drive(i int) media.Drive            { return c.drives[i] }
func (c *tapeChanger) Slots() ([]media.SlotStatus, error) { return c.ld.slots() }

// Drives inventories every drive and what it holds (media.Changer).
func (c *tapeChanger) Drives() ([]media.DriveStatus, error) {
	out := make([]media.DriveStatus, len(c.drives))
	for i, d := range c.drives {
		st, ok := d.Loaded()
		out[i] = media.DriveStatus{Drive: i, Loaded: ok, FromSlot: d.fromSlot, Volume: st}
	}
	return out, nil
}

// Load binds the cartridge in slot to drive (media.Changer), rebinding the drive's
// device so its Volume operations act on the new cartridge.
func (c *tapeChanger) Load(slot, drive int) error {
	dev, barcode, err := c.ld.load(slot, drive)
	if err != nil {
		return err
	}
	d := c.drives[drive]
	d.dev, d.barcode, d.fromSlot = dev, barcode, slot
	return nil
}

// Unload returns the cartridge in drive to its slot (media.Changer).
func (c *tapeChanger) Unload(drive int) error {
	if err := c.ld.unload(drive); err != nil {
		return err
	}
	d := c.drives[drive]
	d.dev, d.barcode, d.fromSlot = nil, "", -1
	return nil
}

// realDriveLoader is a single real drive a human loads: one fixed device, no
// addressable slots, and a Load that refuses (media.ErrManualLoad) because only a
// human moves the reel. The librarian prompts the operator and re-reads the drive.
type realDriveLoader struct{ dev device }

func (r *realDriveLoader) driveCount() int                    { return 1 }
func (r *realDriveLoader) manual() bool                       { return true }
func (r *realDriveLoader) slots() ([]media.SlotStatus, error) { return nil, nil }
func (r *realDriveLoader) load(slot, drive int) (device, string, error) {
	return nil, "", media.ErrManualLoad
}
func (r *realDriveLoader) unload(int) error { return media.ErrManualLoad }
func (r *realDriveLoader) loaded(drive int) (device, string, int, bool) {
	if drive != 0 {
		return nil, "", -1, false
	}
	return r.dev, "", -1, true // the drive always has its device; whether a tape is in it is read on access
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
