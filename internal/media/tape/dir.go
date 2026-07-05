package tape

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"gocloud.dev/blob"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/media/bucket"
)

// slotName is the key prefix of storage slot i (1-based): slot-01, slot-02, …
func slotName(i int) string { return fmt.Sprintf("slot-%02d", i) }

// slotPrefix is the object-key prefix under which slot i's cartridge files live.
func slotPrefix(i int) string { return slotName(i) + "/" }

// simBarcode is a slot's stable simulated VolumeTag — the physical barcode a real
// library's scanner reads, deliberately distinct from the on-tape label so the
// barcode-vs-label split is exercised without hardware.
func simBarcode(slot int) string { return fmt.Sprintf("SIM%04d", slot) }

// driveBindFile records which slot each drive currently holds, so a load survives
// across CLI invocations (each opens a fresh handle).
const driveBindFile = ".drives"

// dirLoader is the emulated changer backend over a gocloud bucket: nSlots
// cartridges (slot-NN/ key prefixes, each a dirDevice) fed into nDrives drives.
// The bucket is a plain directory (dir: /path) or any object-store URL
// (dir: s3://…, gs://…), so the same emulator runs on disk or in the cloud. A
// drive holds a slot's cartridge by pointing at its prefix; "loading" sets that
// pointer (a real robot moves the cartridge, the sim just binds). With manual set
// it reports Manual() so the librarian runs the operator-prompt path, yet load()
// still effects the chosen cartridge — simulating a hand-loaded drive without
// hardware.
type dirLoader struct {
	// ctx in a struct is accepted debt, forced by media.Volume's ctx-less read path
	// (revisit if Volume ever grows a ctx).
	ctx      context.Context
	bucket   *blob.Bucket
	capacity int64
	nSlots   int
	nDrives  int
	isManual bool

	mu   sync.Mutex
	bind []int // drive -> slot (1-based; -1 = empty), persisted in driveBindFile
}

// openDirLoader opens the bucket at loc (a directory path or bucket URL) and
// reads the persisted drive bindings. Slots are the fixed key prefixes slot-01…
// slot-NN, so a blank library needs no stocking. nSlots and nDrives are floored
// at 1.
func openDirLoader(loc string, capacity int64, nSlots, nDrives int, manual bool) (*dirLoader, error) {
	ctx := context.Background()
	b, err := bucket.Open(ctx, loc)
	if err != nil {
		return nil, fmt.Errorf("open tape library %q: %w", loc, err)
	}
	if nSlots < 1 {
		nSlots = 1
	}
	if nDrives < 1 {
		nDrives = 1
	}
	l := &dirLoader{ctx: ctx, bucket: b, capacity: capacity, nSlots: nSlots, nDrives: nDrives, isManual: manual}
	l.bind = make([]int, nDrives)
	for i := range l.bind {
		l.bind[i] = -1
	}
	if data, err := b.ReadAll(ctx, driveBindFile); err == nil {
		var saved []int
		if json.Unmarshal(data, &saved) == nil {
			for i := 0; i < nDrives && i < len(saved); i++ {
				if saved[i] >= 1 && saved[i] <= nSlots {
					l.bind[i] = saved[i]
				}
			}
		}
	}
	return l, nil
}

func (l *dirLoader) driveCount() int      { return l.nDrives }
func (l *dirLoader) manual() bool         { return l.isManual }
func (l *dirLoader) driveNode(int) string { return "" } // file-backed: no OS device node

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
	return l.bucket.WriteAll(l.ctx, driveBindFile, b, nil)
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

// load binds slot's cartridge prefix to drive and persists the choice.
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
	dev, err := openDir(l.ctx, l.bucket, slotPrefix(slot), l.capacity)
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

// loaded reports drive's current binding by re-opening the bound slot's prefix.
func (l *dirLoader) loaded(drive int) (device, string, int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if drive < 0 || drive >= l.nDrives || l.bind[drive] < 0 {
		return nil, "", -1, false
	}
	slot := l.bind[drive]
	dev, err := openDir(l.ctx, l.bucket, slotPrefix(slot), l.capacity)
	if err != nil {
		return nil, "", -1, false
	}
	return dev, simBarcode(slot), slot, true
}

// dirDevice emulates a tape with a key prefix of numbered objects in a gocloud
// bucket — on a plain directory it is the fully-testable default for setups
// without a real drive; on an object-store URL the same emulator becomes a cloud
// tape library. Appends are serial (one head); files are numbered 000000,
// 000001… A non-zero capacity makes the emulated tape finite: a write that would
// run past it fails mid-stream with media.ErrVolumeFull (end-of-tape), as a real
// drive signals EOT.
type dirDevice struct {
	// ctx in a struct is accepted debt, forced by media.Volume's ctx-less read path
	// (revisit if Volume ever grows a ctx).
	ctx        context.Context
	bucket     *blob.Bucket
	prefix     string // "slot-NN/": the cartridge's key prefix
	capacity   int64  // bytes; 0 = unbounded
	mu         sync.Mutex
	next       int
	used       int64 // bytes currently written across all files
	hasForeign bool  // prefix holds non-NBackup keys (foreign media); see foreign()
}

// foreign reports whether the cartridge prefix holds keys that are not NBackup's
// own NNNNNN-numbered files — non-NBackup data the overwrite guard must refuse,
// distinct from a genuinely empty (blank) cartridge. The label protocol consults it
// so foreign content in an emulated cartridge is detected just as a foreign file-0
// label is on a real tape, rather than being mistaken for blank.
func (d *dirDevice) foreign() bool { return d.hasForeign }

func openDir(ctx context.Context, b *blob.Bucket, prefix string, capacity int64) (*dirDevice, error) {
	d := &dirDevice{ctx: ctx, bucket: b, prefix: prefix, capacity: capacity}
	// Key names only — cheap, no object reads. The delimiter keeps the scan flat,
	// skipping nested "directories" as the os.ReadDir-based emulator skipped subdirs.
	iter := b.List(&blob.ListOptions{Prefix: prefix, Delimiter: "/"})
	max := -1
	for {
		obj, err := iter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if obj.IsDir {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(obj.Key, prefix))
		if err != nil {
			// A key that is not one of our NNNNNN-numbered files: this cartridge holds
			// non-NBackup data. Flag it foreign so the label guard refuses to overwrite it,
			// rather than counting only our files and mistaking the cartridge for blank.
			d.hasForeign = true
			continue
		}
		if n > max {
			max = n
		}
		d.used += obj.Size
	}
	d.next = max + 1
	return d, nil
}

func (d *dirDevice) key(pos int) string { return d.prefix + fmt.Sprintf("%06d", pos) }

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
	// The writer is bound to a cancelable ctx: Close commits the object; canceling
	// first makes Close discard it, so Abort leaves nothing behind (the head does
	// not advance past a partial).
	ctx, cancel := context.WithCancel(d.ctx)
	w, err := d.bucket.NewWriter(ctx, d.key(pos), nil)
	if err != nil {
		cancel()
		d.mu.Unlock()
		return nil, err
	}
	// Cap the write at the remaining capacity so an over-long file hits EOT mid-stream; on EOT the
	// partial file is discarded (the tape cannot hold it).
	return &dirFileWriter{d: d, w: w, cancel: cancel, pos: pos, cw: &capWriter{w: w, remaining: d.remaining()}}, nil
}

// dirFileWriter writes one file to the emulated tape; the device lock is held until Commit/Abort.
type dirFileWriter struct {
	d      *dirDevice
	w      *blob.Writer
	cancel context.CancelFunc
	pos    int
	cw     *capWriter
}

func (w *dirFileWriter) Write(p []byte) (int, error) { return w.cw.Write(p) }

func (w *dirFileWriter) Commit() (int, error) {
	defer w.d.mu.Unlock()
	defer w.cancel()
	if err := w.w.Close(); err != nil {
		return 0, err // a failed Close saves no object, so nothing to clean up
	}
	w.d.used += w.cw.written
	w.d.next = w.pos + 1
	return w.pos, nil
}

func (w *dirFileWriter) Abort() {
	defer w.d.mu.Unlock()
	w.cancel()  // abandon the upload: Close on a canceled ctx discards the partial
	w.w.Close() //nolint:errcheck — the error is the cancellation we caused
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
// emulator's analogue of a drive returning EOT part-way through a record.
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
	r, err := d.bucket.NewReader(d.ctx, d.key(pos), nil)
	if err != nil {
		return nil, fmt.Errorf("no file at position %d: %w", pos, err)
	}
	return r, nil
}

// reset deletes every file so the next write starts at file 0 — the emulator's
// equivalent of overwriting a tape from BOT. It removes foreign (non-numbered)
// keys too: relabeling overwrites the whole volume, so a forced relabel of a
// foreign cartridge leaves a clean tape rather than co-mingling our label with the
// stranger's files.
func (d *dirDevice) reset() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	iter := d.bucket.List(&blob.ListOptions{Prefix: d.prefix, Delimiter: "/"})
	for {
		obj, err := iter.Next(d.ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if obj.IsDir {
			continue
		}
		if err := d.bucket.Delete(d.ctx, obj.Key); err != nil {
			return err
		}
	}
	d.next = 0
	d.used = 0
	d.hasForeign = false
	return nil
}
