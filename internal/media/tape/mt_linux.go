//go:build linux

package tape

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"unsafe"

	"github.com/Niloen/nbackup/internal/record"
	"golang.org/x/sys/unix"
)

// mtDevice drives a real tape via the Linux st(4) driver, using its ioctls
// directly (MTIOCTOP / MTIOCGET) rather than shelling out to mt(1). The mt
// binaries disagree on basic commands — GNU cpio's mt spells end-of-data `eom`,
// Linux's mt-st spells it `eod`, and neither accepts the other — so the only
// portable, parseable control surface is the kernel interface Amanda's tape
// device also targets. Each operation opens the no-rewind device, positions, and
// does block I/O; the st driver keeps the head position between opens, so file
// numbers persist across the open/close of a single logical operation.
//
// Block discipline (the part the dirDevice never had to model): the device is put
// in VARIABLE-block mode (MTSETBLK 0) so each write() is one tape record of its
// exact length and each read() returns one whole record. NBackup verifies a
// payload by re-hashing the bytes it reads back to a filemark, so storage must be
// byte-exact — fixed-block mode would zero-pad the final block and break the hash.
// Writes are therefore chunked to <= block so no record exceeds it, and reads use a
// buffer of exactly block so a record never overflows the read buffer (which the st
// driver reports as ENOMEM). A file is one or more records terminated by a single
// filemark; file 0 is the label, written at BOT so it truncates any prior contents.
//
// Tape is single-stream: the mutex serializes every operation, and a read/append
// holds it from open until the returned stream is closed/committed.
type mtDevice struct {
	dev   string
	block int // tape record size in bytes: writes are chunked to <= this, reads buffered to it

	mu sync.Mutex
	// atBOT makes the next appendWriter write at beginning-of-tape instead of
	// appending at end-of-data. reset() sets it so a (re)label overwrites file 0
	// from BOT — which on tape truncates everything beyond the new write — rather
	// than appending a second label past the old data.
	atBOT bool
}

// Linux st(4) MTIOCTOP operation codes (from <linux/mtio.h>).
const (
	mtFSF    = 1  // forward-space filemarks
	mtWEOF   = 5  // write filemarks
	mtREW    = 6  // rewind
	mtEOM    = 12 // space to end of recorded data
	mtSETBLK = 20 // set block size (0 = variable)
)

// mtOp mirrors C `struct mtop` (the MTIOCTOP argument).
type mtOp struct {
	op    int16
	_     int16
	count int32
}

// mtGet mirrors C `struct mtget` (the MTIOCGET argument). The status fields are C
// `long` — modeled as Go int, which is 64-bit on LP64 and 32-bit on ILP32, exactly
// like C long on Linux — while the position fields are __kernel_daddr_t (int32).
type mtGet struct {
	typ, resid, dsreg, gstat, erreg int
	fileno, blkno                   int32
}

// The MTIOC* request numbers are computed from the struct sizes with the kernel's
// _IOR/_IOW encoding so they are correct on both LP64 and ILP32 Linux, rather than
// hardcoding the amd64 values.
var (
	mtIOCTOP = ioc(iocWrite, 'm', 1, unsafe.Sizeof(mtOp{}))
	mtIOCGET = ioc(iocRead, 'm', 2, unsafe.Sizeof(mtGet{}))
)

// ioctl request encoding (Linux asm-generic/ioctl.h).
const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocNRShift  = 0
	iocTypeShft = iocNRShift + iocNRBits
	iocSizeShft = iocTypeShft + iocTypeBits
	iocDirShift = iocSizeShft + 14 // + _IOC_SIZEBITS
	iocRead     = 2
	iocWrite    = 1
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<iocDirShift | typ<<iocTypeShft | nr<<iocNRShift | size<<iocSizeShft
}

func openMT(dev string, block int) (*mtDevice, error) {
	if block <= 0 {
		block = defaultTapeBlock
	}
	if block < record.HeaderBlock {
		// The fixed header block must fit a single tape record so one read() returns
		// it whole; a smaller record size would split the header and risk a read
		// buffer too small for it.
		return nil, fmt.Errorf("tape block_size %d is below the %d-byte header block", block, record.HeaderBlock)
	}
	if block > maxTapeBlock {
		// In variable-block mode each record must fit one st(4) driver buffer. The
		// driver guarantees a single contiguous allocation only up to this size (it
		// can stitch larger blocks from several chunks, but that may fail under memory
		// fragmentation), so we refuse a record size that leaves writes able to fail.
		return nil, fmt.Errorf("tape block_size %d exceeds the %d-byte maximum (the st driver's guaranteed single-buffer allocation)", block, maxTapeBlock)
	}
	return &mtDevice{dev: dev, block: block}, nil
}

const (
	// defaultTapeBlock is the record size used when the medium sets no block_size.
	defaultTapeBlock = 64 * 1024
	// maxTapeBlock caps block_size (and the read grow-and-retry below) at the st(4)
	// driver's guaranteed single-allocation buffer: 256 KiB on 64-bit. Staying within
	// it keeps a variable-block write from ever failing to allocate a buffer.
	maxTapeBlock = 256 * 1024
)

// mtIoctlOp issues one MTIOCTOP positioning/control command on fd.
func mtIoctlOp(fd uintptr, op int16, count int32) error {
	o := mtOp{op: op, count: count}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, mtIOCTOP, uintptr(unsafe.Pointer(&o))); errno != 0 {
		return errno
	}
	return nil
}

// mtFileno returns the current file (record-group) number via MTIOCGET.
func mtFileno(fd uintptr) (int, error) {
	var g mtGet
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, mtIOCGET, uintptr(unsafe.Pointer(&g))); errno != 0 {
		return 0, errno
	}
	return int(g.fileno), nil
}

// openDev opens the no-rewind device and puts it in variable-block mode. A write
// path opens read-write; a read path opens read-only (so a write-protected tape is
// still readable) and tolerates a rejected MTSETBLK, since reading a variable-block
// record only needs a large-enough buffer regardless of the device's set size.
func (m *mtDevice) openDev(write bool) (*os.File, error) {
	flag := os.O_RDONLY
	if write {
		flag = os.O_RDWR
	}
	f, err := os.OpenFile(m.dev, flag, 0)
	if err != nil {
		return nil, err
	}
	if err := mtIoctlOp(f.Fd(), mtSETBLK, 0); err != nil && write {
		f.Close()
		return nil, fmt.Errorf("set variable block mode on %s: %w", m.dev, err)
	}
	return f, nil
}

// count returns the number of files on the mounted tape: space to end of recorded
// data and read the file number there. A blank tape reports 0.
func (m *mtDevice) count() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.openDev(false)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := mtIoctlOp(f.Fd(), mtEOM, 0); err != nil {
		return 0, fmt.Errorf("mt eom on %s: %w", m.dev, err)
	}
	n, err := mtFileno(f.Fd())
	if err != nil {
		return 0, err
	}
	if n < 0 {
		// The st driver reports fileno -1 when the position is undefined — a blank tape
		// after MTEOM sits at BOT with no files. Report 0 rather than let a negative count
		// reach a make([]…, 0, n) (which panics) or a file-scan loop.
		return 0, nil
	}
	return n, nil
}

// bytesUsed is unknowable for a real drive: software cannot see a tape's fill
// without hitting EOT, so capacity tracking falls back to the reactive ErrVolumeFull.
func (m *mtDevice) bytesUsed() int64 { return 0 }

// reset rewinds and arms the next append to write at BOT. The label write that
// follows overwrites file 0, which on tape truncates everything beyond it — the
// physical basis of (re)labeling.
func (m *mtDevice) reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.openDev(true)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := mtIoctlOp(f.Fd(), mtREW, 0); err != nil {
		return fmt.Errorf("mt rewind on %s: %w", m.dev, err)
	}
	m.atBOT = true
	return nil
}

func (m *mtDevice) appendWriter() (deviceWriter, error) {
	m.mu.Lock()
	// The lock and fd live past this function on success — handed to the returned
	// writer, which releases them in Commit/Abort. So they cannot be plainly
	// deferred (the held lock IS the single-stream serialization); instead this
	// guard rolls them back on every error path and clears once ownership transfers.
	ok := false
	f, err := m.openDev(true)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	defer func() {
		if !ok {
			f.Close()
			m.mu.Unlock()
		}
	}()
	// Position where the next file begins: BOT for a (re)label just reset(), else
	// end-of-data to append after the last file.
	posOp := int16(mtEOM)
	if m.atBOT {
		posOp = mtREW
	}
	if err := mtIoctlOp(f.Fd(), posOp, 0); err != nil {
		return nil, fmt.Errorf("position %s for append: %w", m.dev, err)
	}
	pos, err := mtFileno(f.Fd())
	if err != nil {
		return nil, err
	}
	m.atBOT = false
	ok = true
	return &mtFileWriter{m: m, f: f, pos: pos}, nil
}

// mtFileWriter writes one tape file (record group). It chunks the byte stream into
// records of at most m.block so no record exceeds the read buffer, and ends the file
// with exactly one filemark on Commit. The device lock is held until Commit/Abort.
type mtFileWriter struct {
	m   *mtDevice
	f   *os.File
	pos int
}

func (w *mtFileWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > w.m.block {
			n = w.m.block
		}
		written, err := w.f.Write(p[:n])
		total += written
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

func (w *mtFileWriter) Commit() (int, error) {
	defer w.m.mu.Unlock()
	// Write exactly one filemark to end the file. Closing the device would itself
	// write a filemark, but the st driver does not add a second when one was just
	// written, so the file number advances by exactly one.
	if err := mtIoctlOp(w.f.Fd(), mtWEOF, 1); err != nil {
		w.f.Close()
		return 0, fmt.Errorf("mt weof on %s: %w", w.m.dev, err)
	}
	if err := w.f.Close(); err != nil {
		return 0, err
	}
	return w.pos, nil
}

func (w *mtFileWriter) Abort() {
	defer w.m.mu.Unlock()
	// Leave the unfinalized records without a closing filemark; the rebuild scan
	// skips a file whose header will not decode (an interrupted append is always
	// the last file written).
	w.f.Close()
}

// readFile positions to file pos and returns its records as a stream ending at the
// trailing filemark. The reader is buffered to m.block so a small consumer read
// (e.g. the fixed header block) is served from one whole-record device read rather
// than failing with ENOMEM. The device lock is held until the stream is closed.
func (m *mtDevice) readFile(pos int) (io.ReadCloser, error) {
	m.mu.Lock()
	// As in appendWriter: lock + fd are handed to the returned reader on success
	// (released in Close), so the guard rolls them back on every error path instead
	// of a plain defer.
	ok := false
	f, err := m.openDev(false)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	defer func() {
		if !ok {
			f.Close()
			m.mu.Unlock()
		}
	}()
	// Absolute seek: rewind to BOT, then forward-space pos filemarks to land at the
	// start of file pos (BOT is file 0, so no off-by-one).
	if err := mtIoctlOp(f.Fd(), mtREW, 0); err != nil {
		return nil, fmt.Errorf("mt rewind on %s: %w", m.dev, err)
	}
	if pos > 0 {
		if err := mtIoctlOp(f.Fd(), mtFSF, int32(pos)); err != nil {
			return nil, fmt.Errorf("mt fsf %d on %s: %w", pos, m.dev, err)
		}
	}
	if got, err := mtFileno(f.Fd()); err == nil && got != pos {
		return nil, fmt.Errorf("tape positioning on %s: wanted file %d, landed at %d", m.dev, pos, got)
	}
	ok = true
	return &mtReader{m: m, f: f, buf: make([]byte, m.block)}, nil
}

// mtReader streams one tape file, one whole record at a time. In variable-block mode
// the st(4) driver returns ENOMEM if a read buffer is smaller than the record on
// tape, so rather than trust that block matches the records actually written, it
// starts at block and doubles the buffer on ENOMEM up to maxTapeBlock (Amanda's
// grow-and-retry) — a tape written with a larger block_size still reads. A failed
// too-small read leaves the tape position unchanged, so the retry re-reads the same
// record. It releases the device lock on Close.
type mtReader struct {
	m   *mtDevice
	f   *os.File
	buf []byte
	off int // unread bytes are buf[off:end]
	end int
}

func (r *mtReader) Read(p []byte) (int, error) {
	if r.off >= r.end {
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf[r.off:r.end])
	r.off += n
	return n, nil
}

// fill reads the next whole record into r.buf, growing and retrying when the driver
// reports the buffer was too small for the record.
func (r *mtReader) fill() error {
	buf, n, err := growReadRecord(r.f.Read, r.buf)
	r.buf = buf
	if err != nil {
		return err
	}
	r.off, r.end = 0, n
	return nil
}

// growReadRecord reads one whole variable-block record via read, growing buf (up to
// maxTapeBlock) and retrying whenever the st(4) driver reports the buffer was too
// small for the record on tape. It returns the possibly-reallocated buffer and the
// record length; n==0 with io.EOF is the trailing filemark (end of the file). A
// failed too-small read leaves the tape position unchanged, so the retry re-reads the
// same record into the larger buffer. This is the pure core of mtReader.fill, split
// out so the grow-and-retry can be exercised without a real drive.
func growReadRecord(read func([]byte) (int, error), buf []byte) ([]byte, int, error) {
	for {
		n, err := read(buf)
		if n > 0 {
			return buf, n, nil
		}
		if err == nil || errors.Is(err, io.EOF) {
			return buf, 0, io.EOF // a zero-length read is the trailing filemark: end of this file
		}
		if isSmallReadBuffer(err) && len(buf) < maxTapeBlock {
			grown := len(buf) * 2
			if grown > maxTapeBlock {
				grown = maxTapeBlock
			}
			buf = make([]byte, grown)
			continue
		}
		return buf, 0, err
	}
}

// isSmallReadBuffer reports the st(4) "block larger than the read buffer" errnos.
func isSmallReadBuffer(err error) bool {
	return errors.Is(err, unix.ENOMEM) || errors.Is(err, unix.EOVERFLOW) || errors.Is(err, unix.EINVAL)
}

func (r *mtReader) Close() error {
	defer r.m.mu.Unlock()
	return r.f.Close()
}

var (
	_ io.ReadCloser = (*mtReader)(nil)
	_ deviceWriter  = (*mtFileWriter)(nil)
)
