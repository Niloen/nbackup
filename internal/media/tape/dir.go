package tape

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// dirDevice emulates a tape with a directory of numbered files (Amanda's file:
// device). It is the fully-testable backend and the default for setups without a
// real drive. Appends are serial (one head); files are numbered 000000, 000001…
type dirDevice struct {
	dir  string
	mu   sync.Mutex
	next int
}

func openDir(dir string) (*dirDevice, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	d := &dirDevice{dir: dir}
	entries, err := os.ReadDir(dir) // filenames only — cheap, no header reads
	if err != nil {
		return nil, err
	}
	max := -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n, err := strconv.Atoi(e.Name()); err == nil && n > max {
			max = n
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

func (d *dirDevice) writeFile(write func(w io.Writer) error) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pos := d.next
	f, err := os.Create(d.path(pos))
	if err != nil {
		return 0, err
	}
	if err := write(f); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	d.next = pos + 1
	return pos, nil
}

func (d *dirDevice) readFile(pos int) (io.ReadCloser, error) {
	f, err := os.Open(d.path(pos))
	if err != nil {
		return nil, fmt.Errorf("no file at position %d: %w", pos, err)
	}
	return f, nil
}
