// Package tape implements media.Volume as a virtual tape: a flat, sequential
// sequence of numbered files (Amanda's "file:" device, the standard way to test
// the tape model). It captures what makes tape distinct from random-access disk:
// files are addressed by absolute file number, the first file is a volume label,
// appends are strictly serial, and reclamation is whole-volume (no per-slot
// delete) — you relabel a tape to reuse it. A real /dev/nst0 drive would back the
// same Volume via mt(1) fast-forward; only the I/O backend differs.
package tape

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/media"
)

func init() {
	media.RegisterVolume("tape", func(opts media.Options) (media.Volume, error) {
		dir := opts.Get("dir")
		if dir == "" {
			return nil, fmt.Errorf("tape medium requires a 'dir' (virtual tape directory); real tape drives are not yet supported")
		}
		return open(dir, opts.Get("label"))
	})
	media.RegisterProfile("tape", media.NewVolumeProfile)
}

type tape struct {
	dir  string
	mu   sync.Mutex
	next int
	idx  map[int]media.Header
}

func open(dir, label string) (*tape, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	t := &tape{dir: dir, idx: map[int]media.Header{}}
	if err := t.scan(); err != nil {
		return nil, err
	}
	if t.next == 0 { // fresh tape: write the volume label as file 0
		if label == "" {
			label = filepath.Base(dir)
		}
		if _, err := t.AppendFile(media.Header{Kind: media.KindLabel, Slot: "", DLE: label, CreatedAt: time.Now().UTC()},
			func(io.Writer) error { return nil }); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (t *tape) Name() string { return "tape" }

func (t *tape) path(pos int) string { return filepath.Join(t.dir, fmt.Sprintf("%06d", pos)) }

// AppendFile writes the next file. It holds the lock for the whole write: a tape
// has one head, so appends are strictly serial.
func (t *tape) AppendFile(h media.Header, write func(w io.Writer) error) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	pos := t.next
	f, err := os.Create(t.path(pos))
	if err != nil {
		return 0, err
	}
	if err := media.EncodeHeader(f, h); err != nil {
		f.Close()
		return 0, err
	}
	if err := write(f); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	t.next = pos + 1
	t.idx[pos] = h
	return pos, nil
}

// ReadFile fast-forwards to a file number and returns its header and payload.
func (t *tape) ReadFile(pos int) (media.Header, io.ReadCloser, error) {
	f, err := os.Open(t.path(pos))
	if err != nil {
		return media.Header{}, nil, fmt.Errorf("no file at position %d: %w", pos, err)
	}
	h, err := media.DecodeHeader(f)
	if err != nil {
		f.Close()
		return media.Header{}, nil, err
	}
	return h, f, nil
}

func (t *tape) Files() ([]media.FileInfo, error) {
	t.mu.Lock()
	out := make([]media.FileInfo, 0, len(t.idx))
	for pos, h := range t.idx {
		out = append(out, media.FileInfo{Pos: pos, Header: h})
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Pos < out[j].Pos })
	return out, nil
}

// RemoveSlot is unsupported: a tape reclaims space by relabeling the whole volume,
// not by deleting individual files.
func (t *tape) RemoveSlot(string) error {
	return fmt.Errorf("tape: per-slot removal unsupported; reuse is whole-volume (relabel)")
}

func (t *tape) scan() error {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return err
	}
	max := -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		pos, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a tape file
		}
		h, err := readHeader(t.path(pos))
		if err != nil {
			return err
		}
		t.idx[pos] = h
		if pos > max {
			max = pos
		}
	}
	t.next = max + 1
	return nil
}

func readHeader(path string) (media.Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return media.Header{}, err
	}
	defer f.Close()
	return media.DecodeHeader(f)
}
