// Package disk implements media.Volume backed by a filesystem directory (local
// or networked, e.g. an NFS mount). Each
// file is stored as two files under slots/<slot>/: a clean payload
// (<NNNNNN>-<dle>-L<n>.tar.<ext>) usable directly with stock tools
// (`zstd -dc … | tar -xf -`, no header to skip), and a JSON header sidecar
// (<NNNNNN>-<dle>-L<n>.hdr). Positions are volume-global file numbers paired by
// their numeric filename prefix.
package disk

import (
	"encoding/json"
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
	media.RegisterVolume("disk", func(opts media.Options) (media.Volume, error) {
		path := opts.Get("path")
		if path == "" {
			return nil, fmt.Errorf("disk medium requires a path")
		}
		return open(filepath.Join(path, "slots"))
	})
	media.RegisterProfile("disk", media.NewSizeProfile)
}

// entry pairs a file's header sidecar and payload (paths relative to root).
type entry struct {
	hdr     string
	payload string
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

var slug = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// stem is the friendly filename base (without extension) for a file.
func stem(pos int, h media.Header) string {
	if h.Kind == media.KindSeal {
		return fmt.Sprintf("%06d-seal", pos)
	}
	return fmt.Sprintf("%06d-%s-L%d", pos, slug.ReplaceAllString(h.DLE, "_"), h.Level)
}

// payloadExt is the extension for a file's payload, so disk files are recognizable
// and directly usable. Kept local so the medium doesn't depend on package filter.
func payloadExt(h media.Header) string {
	if h.Kind == media.KindSeal {
		return ".json"
	}
	switch h.Codec {
	case "gzip":
		return ".tar.gz"
	case "none", "":
		return ".tar"
	default: // zstd and any future codec named after its extension
		return ".tar." + codecExt(h.Codec)
	}
}

func codecExt(codec string) string {
	if codec == "zstd" {
		return "zst"
	}
	return codec
}

func (v *volume) AppendFile(h media.Header, write func(w io.Writer) error) (int, error) {
	v.mu.Lock()
	pos := v.next
	v.next++
	v.mu.Unlock()

	base := stem(pos, h)
	rel := filepath.Join(h.Slot, base+payloadExt(h))
	hdrRel := filepath.Join(h.Slot, base+".hdr")
	full := filepath.Join(v.root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return 0, err
	}
	// Payload first (a clean archive), then the header sidecar.
	f, err := os.Create(full)
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
	hb, err := json.Marshal(h)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(v.root, hdrRel), append(hb, '\n'), 0o644); err != nil {
		return 0, err
	}

	v.mu.Lock()
	v.idx[pos] = entry{hdr: hdrRel, payload: rel}
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
	h, err := readHeader(filepath.Join(v.root, e.hdr))
	if err != nil {
		return media.Header{}, nil, err
	}
	f, err := os.Open(filepath.Join(v.root, e.payload))
	if err != nil {
		return media.Header{}, nil, err
	}
	return h, f, nil
}

func (v *volume) Files() ([]media.FileInfo, error) {
	v.mu.Lock()
	entries := make(map[int]entry, len(v.idx))
	for pos, e := range v.idx {
		entries[pos] = e
	}
	v.mu.Unlock()

	out := make([]media.FileInfo, 0, len(entries))
	for pos, e := range entries {
		h, err := readHeader(filepath.Join(v.root, e.hdr))
		if err != nil {
			return nil, err
		}
		out = append(out, media.FileInfo{Pos: pos, Header: h})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pos < out[j].Pos })
	return out, nil
}

func (v *volume) RemoveSlot(slot string) error {
	if err := os.RemoveAll(filepath.Join(v.root, slot)); err != nil {
		return err
	}
	v.mu.Lock()
	for pos, e := range v.idx {
		if strings.HasPrefix(e.payload, slot+string(filepath.Separator)) {
			delete(v.idx, pos)
		}
	}
	v.mu.Unlock()
	return nil
}

// scan builds the position index from filenames only — it does not read headers,
// so Open stays cheap. Each position has a .hdr and a payload paired by prefix.
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
				continue
			}
			rel := filepath.Join(sd.Name(), fe.Name())
			e := v.idx[pos]
			if strings.HasSuffix(fe.Name(), ".hdr") {
				e.hdr = rel
			} else {
				e.payload = rel
			}
			v.idx[pos] = e
			if pos > max {
				max = pos
			}
		}
	}
	v.next = max + 1
	return nil
}

func readHeader(path string) (media.Header, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return media.Header{}, err
	}
	var h media.Header
	if err := json.Unmarshal(data, &h); err != nil {
		return media.Header{}, fmt.Errorf("parse header %s: %w", path, err)
	}
	return h, nil
}
