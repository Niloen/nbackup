package tape

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/Niloen/nbackup/internal/media"
)

// slotName is the directory name of storage slot i (1-based): slot-01, slot-02, …
func slotName(i int) string { return fmt.Sprintf("slot-%02d", i) }

// simBarcode is a slot's stable simulated VolumeTag — the physical barcode a real
// library's scanner reads, deliberately distinct from the on-tape label so the
// barcode-vs-label split is exercised without hardware.
func simBarcode(slot int) string { return fmt.Sprintf("SIM%04d", slot) }

// driveBindFile records which slot each drive currently holds, so a load survives
// across CLI invocations (each opens a fresh handle).
const driveBindFile = ".drives"

// dirLoader is the file-backed changer backend: nSlots cartridges (slot-NN
// subdirectories, each a dirDevice) fed into nDrives drives. A drive holds a slot's
// cartridge by pointing at its directory; "loading" sets that pointer (a real robot
// moves the cartridge, the sim just binds). With manual set it reports Manual() so
// the librarian runs the operator-prompt path, yet load() still effects the chosen
// cartridge — simulating a hand-loaded drive without hardware.
type dirLoader struct {
	root     string
	capacity int64
	nSlots   int
	nDrives  int
	isManual bool

	mu   sync.Mutex
	bind []int // drive -> slot (1-based; -1 = empty), persisted in driveBindFile
}

// openDirLoader stocks root with nSlots blank cartridges and reads the persisted
// drive bindings. nSlots and nDrives are floored at 1.
func openDirLoader(root string, capacity int64, nSlots, nDrives int, manual bool) (*dirLoader, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if nSlots < 1 {
		nSlots = 1
	}
	if nDrives < 1 {
		nDrives = 1
	}
	for i := 1; i <= nSlots; i++ {
		if err := os.MkdirAll(filepath.Join(root, slotName(i)), 0o755); err != nil {
			return nil, err
		}
	}
	l := &dirLoader{root: root, capacity: capacity, nSlots: nSlots, nDrives: nDrives, isManual: manual}
	l.bind = make([]int, nDrives)
	for i := range l.bind {
		l.bind[i] = -1
	}
	if b, err := os.ReadFile(filepath.Join(root, driveBindFile)); err == nil {
		var saved []int
		if json.Unmarshal(b, &saved) == nil {
			for i := 0; i < nDrives && i < len(saved); i++ {
				if saved[i] >= 1 && saved[i] <= nSlots {
					l.bind[i] = saved[i]
				}
			}
		}
	}
	return l, nil
}

func (l *dirLoader) driveCount() int { return l.nDrives }
func (l *dirLoader) manual() bool    { return l.isManual }

// loadedIn reports the drive currently holding slot s, or -1. Caller holds l.mu.
func (l *dirLoader) loadedIn(s int) int {
	for d, b := range l.bind {
		if b == s {
			return d
		}
	}
	return -1
}

func (l *dirLoader) persist() error {
	b, err := json.Marshal(l.bind)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(l.root, driveBindFile), b, 0o644)
}

// slots inventories the storage elements. A slot whose cartridge is currently in a
// drive reports empty, as a real library shows a slot vacated by a load.
func (l *dirLoader) slots() ([]media.SlotStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]media.SlotStatus, 0, l.nSlots)
	for s := 1; s <= l.nSlots; s++ {
		full := l.loadedIn(s) < 0
		bc := ""
		if full {
			bc = simBarcode(s)
		}
		out = append(out, media.SlotStatus{Slot: s, Barcode: bc, Full: full})
	}
	return out, nil
}

// load binds slot's cartridge directory to drive and persists the choice.
func (l *dirLoader) load(slot, drive int) (device, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if drive < 0 || drive >= l.nDrives {
		return nil, "", fmt.Errorf("no drive %d (the changer has %d)", drive, l.nDrives)
	}
	if slot < 1 || slot > l.nSlots {
		return nil, "", fmt.Errorf("no slot %d (the changer has %d)", slot, l.nSlots)
	}
	if d := l.loadedIn(slot); d >= 0 && d != drive {
		return nil, "", fmt.Errorf("slot %d is already loaded in drive %d", slot, d)
	}
	dev, err := openDir(filepath.Join(l.root, slotName(slot)), l.capacity)
	if err != nil {
		return nil, "", err
	}
	l.bind[drive] = slot
	if err := l.persist(); err != nil {
		return nil, "", err
	}
	return dev, simBarcode(slot), nil
}

func (l *dirLoader) unload(drive int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if drive < 0 || drive >= l.nDrives {
		return fmt.Errorf("no drive %d (the changer has %d)", drive, l.nDrives)
	}
	l.bind[drive] = -1
	return l.persist()
}

// loaded reports drive's current binding by re-opening the bound slot's directory.
func (l *dirLoader) loaded(drive int) (device, string, int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if drive < 0 || drive >= l.nDrives || l.bind[drive] < 0 {
		return nil, "", -1, false
	}
	slot := l.bind[drive]
	dev, err := openDir(filepath.Join(l.root, slotName(slot)), l.capacity)
	if err != nil {
		return nil, "", -1, false
	}
	return dev, simBarcode(slot), slot, true
}

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
