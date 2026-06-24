package tape

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// tapeBlock is the fixed block size written to the device.
const tapeBlock = 32 * 1024

// mtDevice drives a real tape via mt(1) plus raw reads/writes of the no-rewind
// device (so position persists between commands). Appends go to end-of-data and
// are closed with a filemark; reads fast-forward with `mt asf`.
//
// NOTE: this backend cannot be exercised in CI (it needs a physical/virtual
// drive). It is structurally complete and reviewed, but unverified by tests; the
// dirDevice is what the test suite covers. Tape is single-stream, so operations
// must not overlap.
type mtDevice struct {
	dev string
	mu  sync.Mutex
}

func openMT(dev string) (*mtDevice, error) {
	if _, err := exec.LookPath("mt"); err != nil {
		return nil, fmt.Errorf("tape device %q needs mt(1) on PATH: %w", dev, err)
	}
	return &mtDevice{dev: dev}, nil
}

func (m *mtDevice) mt(args ...string) error {
	out, err := exec.Command("mt", append([]string{"-f", m.dev}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mt %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *mtDevice) fileNumber() (int, error) {
	out, err := exec.Command("mt", "-f", m.dev, "status").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("mt status: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseFileNumber(string(out))
}

func (m *mtDevice) count() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.mt("eom"); err != nil {
		return 0, err
	}
	return m.fileNumber()
}

// bytesUsed is unknowable for a real drive: software cannot see a tape's fill
// without hitting EOT, so capacity tracking falls back to the reactive ErrVolumeFull.
func (m *mtDevice) bytesUsed() int64 { return 0 }

func (m *mtDevice) writeFile(write func(w io.Writer) error) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.mt("eom"); err != nil {
		return 0, err
	}
	pos, err := m.fileNumber()
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(m.dev, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriterSize(f, tapeBlock)
	if err := write(bw); err != nil {
		f.Close()
		return 0, err
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil { // closing the device writes a filemark too on most drivers
		return 0, err
	}
	if err := m.mt("weof", "1"); err != nil {
		return 0, err
	}
	return pos, nil
}

// reset rewinds to beginning-of-tape; the subsequent label write at file 0 (plus
// its filemark) logically discards everything beyond it.
func (m *mtDevice) reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mt("rewind")
}

func (m *mtDevice) readFile(pos int) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.mt("asf", strconv.Itoa(pos)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(m.dev, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	return f, nil // reads return data up to the filemark, then EOF
}

// parseFileNumber extracts the current file number from `mt status` output, whose
// exact wording varies by driver (e.g. "File number=3, block number=0").
func parseFileNumber(status string) (int, error) {
	lower := strings.ToLower(status)
	i := strings.Index(lower, "file number")
	if i < 0 {
		return 0, fmt.Errorf("mt status: no file number in %q", status)
	}
	rest := status[i+len("file number"):]
	j := 0
	for j < len(rest) && (rest[j] < '0' || rest[j] > '9') {
		j++
	}
	k := j
	for k < len(rest) && rest[k] >= '0' && rest[k] <= '9' {
		k++
	}
	if j == k {
		return 0, fmt.Errorf("mt status: cannot parse file number from %q", status)
	}
	return strconv.Atoi(rest[j:k])
}
