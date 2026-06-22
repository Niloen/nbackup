// Package localdisk implements media.Volume backed by a local directory. Slot
// files live under <path>/slots/<slot>/, each named "<pos>-<descriptor>" with a
// fixed header block followed by the payload. Positions are volume-global file
// numbers; a friendly descriptor derived from the header keeps the tree readable.
package localdisk

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Niloen/nbackup/internal/media"
)

func init() {
	media.RegisterVolume("local-disk", func(opts media.Options) (media.Volume, error) {
		path := opts.Get("path")
		if path == "" {
			return nil, fmt.Errorf("local-disk medium requires a path")
		}
		return open(filepath.Join(path, "slots"))
	})
	media.RegisterProfile("local-disk", media.NewSizeProfile)
}

type entry struct {
	rel    string // path relative to root
	header media.Header
}

type volume struct {
	root string
	mu   sync.Mutex
	next int
	idx  map[int]entry
}

func open(root string) (*volume, error) {
	v := &volume{root: root, idx: map[int]entry{}}
	if err := v.scan(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *volume) Name() string { return "local-disk" }

var slug = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func descriptor(pos int, h media.Header) string {
	switch h.Kind {
	case media.KindSeal:
		return fmt.Sprintf("%06d-seal", pos)
	default:
		name := slug.ReplaceAllString(h.DLE, "_")
		return fmt.Sprintf("%06d-%s-L%d", pos, name, h.Level)
	}
}

func (v *volume) AppendFile(h media.Header, write func(w io.Writer) error) (int, error) {
	v.mu.Lock()
	pos := v.next
	v.next++
	v.mu.Unlock()

	rel := filepath.Join(h.Slot, descriptor(pos, h))
	full := filepath.Join(v.root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(full)
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

	v.mu.Lock()
	v.idx[pos] = entry{rel: rel, header: h}
	v.mu.Unlock()
	return pos, nil
}

func (v *volume) ReadFile(pos int) (media.Header, io.ReadCloser, error) {
	v.mu.Lock()
	e, ok := v.idx[pos]
	v.mu.Unlock()
	if !ok {
		return media.Header{}, nil, fmt.Errorf("no file at position %d", pos)
	}
	f, err := os.Open(filepath.Join(v.root, e.rel))
	if err != nil {
		return media.Header{}, nil, err
	}
	h, err := media.DecodeHeader(f)
	if err != nil {
		f.Close()
		return media.Header{}, nil, err
	}
	return h, f, nil // f is now positioned at the payload
}

func (v *volume) Files() ([]media.FileInfo, error) {
	v.mu.Lock()
	out := make([]media.FileInfo, 0, len(v.idx))
	for pos, e := range v.idx {
		out = append(out, media.FileInfo{Pos: pos, Header: e.header})
	}
	v.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Pos < out[j].Pos })
	return out, nil
}

func (v *volume) RemoveSlot(slot string) error {
	if err := os.RemoveAll(filepath.Join(v.root, slot)); err != nil {
		return err
	}
	v.mu.Lock()
	for pos, e := range v.idx {
		if e.header.Slot == slot {
			delete(v.idx, pos)
		}
	}
	v.mu.Unlock()
	return nil
}

// scan rebuilds the in-memory index from the directory tree, reading each file's
// header block and parsing its position from the filename.
func (v *volume) scan() error {
	slots, err := os.ReadDir(v.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	max := -1
	for _, sd := range slots {
		if !sd.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(v.root, sd.Name()))
		if err != nil {
			return err
		}
		for _, fe := range files {
			if fe.IsDir() {
				continue
			}
			pos, err := strconv.Atoi(strings.SplitN(fe.Name(), "-", 2)[0])
			if err != nil {
				continue // not a volume file
			}
			rel := filepath.Join(sd.Name(), fe.Name())
			h, err := readHeader(filepath.Join(v.root, rel))
			if err != nil {
				return err
			}
			v.idx[pos] = entry{rel: rel, header: h}
			if pos > max {
				max = pos
			}
		}
	}
	v.next = max + 1
	return nil
}

func readHeader(path string) (media.Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return media.Header{}, err
	}
	defer f.Close()
	return media.DecodeHeader(bufio.NewReaderSize(f, media.HeaderBlock))
}
