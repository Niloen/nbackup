package tape

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/Niloen/nbackup/internal/media"
)

// bayPrefix names a library's physical positions: bay-01, bay-02, …
const bayPrefix = "bay-"

// bayName is the directory name of physical position i (1-based): bay-01, bay-02…
func bayName(i int) string { return fmt.Sprintf("%s%02d", bayPrefix, i) }

// dirChanger emulates a tape library as a directory of bays (subdirectories),
// each holding one cartridge (a dirDevice). It is label-agnostic: it mounts bays
// by name and reports a bay's label only as a convenience inventory (a stand-in
// for a barcode reader), exactly the seam a real autochanger exposes. It is a thin
// wrapper over the shared dirLibrary core, exposing every bay as inventory.
type dirChanger struct{ *dirLibrary }

func openDirChanger(root string, capacity int64, tapes int) (*dirChanger, error) {
	lib, err := openDirLibrary(root, capacity, bayPrefix, tapes)
	if err != nil {
		return nil, err
	}
	return &dirChanger{lib}, nil
}

func (c *dirChanger) loaded() (device, string, bool) { return c.loadedDevice() }

// bays inventories every bay in the library.
func (c *dirChanger) bays() ([]media.VolumeStatus, error) { return c.entries(false) }

// dirDevice emulates a tape with a directory of numbered files. It is the
// fully-testable backend and the default for setups without a real drive.
// Appends are serial (one head); files are numbered 000000, 000001…
// A non-zero capacity makes the emulated tape finite: a write that would run past
// it fails mid-stream with media.ErrVolumeFull (end-of-tape), as a real drive
// signals EOT.
type dirDevice struct {
	dir        string
	capacity   int64 // bytes; 0 = unbounded
	mu         sync.Mutex
	next       int
	used       int64 // bytes currently written across all files
	hasForeign bool  // dir holds non-NBackup files (foreign media); see foreign()
}

// foreign reports whether the bay directory holds files that are not NBackup's
// own NNNNNN-numbered files — non-NBackup data the overwrite guard must refuse,
// distinct from a genuinely empty (blank) bay. The label protocol consults it so
// foreign content in a file-backed bay is detected just as a foreign file-0 label
// is on a real tape, rather than being mistaken for blank.
func (d *dirDevice) foreign() bool { return d.hasForeign }

func openDir(dir string, capacity int64) (*dirDevice, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	d := &dirDevice{dir: dir, capacity: capacity}
	entries, err := os.ReadDir(dir) // filenames only — cheap, no header reads
	if err != nil {
		return nil, err
	}
	max := -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			// A file that is not one of our NNNNNN-numbered files: this directory holds
			// non-NBackup data. Flag it foreign so the label guard refuses to overwrite
			// it, rather than counting only our files and mistaking the bay for blank.
			d.hasForeign = true
			continue
		}
		if n > max {
			max = n
		}
		if info, err := e.Info(); err == nil {
			d.used += info.Size()
		}
	}
	d.next = max + 1
	return d, nil
}

func (d *dirDevice) path(pos int) string { return filepath.Join(d.dir, fmt.Sprintf("%06d", pos)) }

func (d *dirDevice) count() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.next, nil
}

func (d *dirDevice) bytesUsed() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.used
}

func (d *dirDevice) appendWriter() (deviceWriter, error) {
	d.mu.Lock()
	pos := d.next
	f, err := os.Create(d.path(pos))
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	// Cap the write at the remaining capacity so an over-long file hits EOT mid-stream; on EOT the
	// partial file is discarded (the tape cannot hold it).
	return &dirFileWriter{d: d, f: f, pos: pos, cw: &capWriter{w: f, remaining: d.remaining()}}, nil
}

// dirFileWriter writes one file to the dir-backed tape; the device lock is held until Commit/Abort.
type dirFileWriter struct {
	d   *dirDevice
	f   *os.File
	pos int
	cw  *capWriter
}

func (w *dirFileWriter) Write(p []byte) (int, error) { return w.cw.Write(p) }

func (w *dirFileWriter) Commit() (int, error) {
	defer w.d.mu.Unlock()
	if err := w.f.Close(); err != nil {
		os.Remove(w.d.path(w.pos))
		return 0, err
	}
	w.d.used += w.cw.written
	w.d.next = w.pos + 1
	return w.pos, nil
}

func (w *dirFileWriter) Abort() {
	defer w.d.mu.Unlock()
	w.f.Close()
	os.Remove(w.d.path(w.pos)) // drop the partial; the head does not advance
}

// remaining is the writable bytes left on the volume (max int64 when unbounded).
func (d *dirDevice) remaining() int64 {
	if d.capacity <= 0 {
		return 1<<63 - 1
	}
	if d.used >= d.capacity {
		return 0
	}
	return d.capacity - d.used
}

// capWriter passes bytes through until it would exceed the tape's remaining
// capacity, then writes what fits and reports media.ErrVolumeFull — the
// directory analogue of a drive returning EOT part-way through a record.
type capWriter struct {
	w         io.Writer
	remaining int64
	written   int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if int64(len(p)) <= c.remaining {
		n, err := c.w.Write(p)
		c.remaining -= int64(n)
		c.written += int64(n)
		return n, err
	}
	var n int
	if c.remaining > 0 {
		n, _ = c.w.Write(p[:c.remaining])
		c.written += int64(n)
		c.remaining = 0
	}
	return n, media.ErrVolumeFull
}

func (d *dirDevice) readFile(pos int) (io.ReadCloser, error) {
	f, err := os.Open(d.path(pos))
	if err != nil {
		return nil, fmt.Errorf("no file at position %d: %w", pos, err)
	}
	return f, nil
}

// reset deletes every file so the next write starts at file 0 — the directory
// equivalent of overwriting a tape from BOT. It removes foreign (non-numbered)
// files too: relabeling overwrites the whole volume, so a forced relabel of a
// foreign bay leaves a clean tape rather than co-mingling our label with the
// stranger's files.
func (d *dirDevice) reset() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(d.dir, e.Name())); err != nil {
			return err
		}
	}
	d.next = 0
	d.used = 0
	d.hasForeign = false
	return nil
}
