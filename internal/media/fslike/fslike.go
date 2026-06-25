// Package fslike implements the media.Volume layout shared by the address-
// identified media (disk, cloud): each file is two artifacts under
// slots/<slot>/ — a clean payload (<NNNNNN>-<dle>-L<n>.tar.<ext>, directly usable
// with stock tools) and a JSON header sidecar (<NNNNNN>-…-L<n>.hdr). Positions are
// volume-global file numbers paired by their numeric filename prefix.
//
// The layout (stems, extensions, slot subtrees, payload-first atomicity, the
// scan that indexes by filename prefix) lives here once; a backing Store supplies
// only the storage primitives (a local directory for disk, an object bucket for
// cloud). Because both media share this code, a slot streams disk<->cloud byte-for-
// byte unchanged.
package fslike

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Niloen/nbackup/internal/record"
)

// Object is one stored file as reported by Store.List: an opaque key the Store can
// read/open, the slot it belongs to, and its basename (whose numeric prefix gives
// the position).
type Object struct {
	Key  string
	Slot string
	Base string
}

// Store is the storage seam a fslike.Volume runs on. Keys are opaque handles the
// Store mints (via Key) and consumes; the fslike layer never parses them, so disk
// (filesystem paths) and cloud (object keys) supply whatever form suits the backend.
type Store interface {
	// Key builds the storage key for a file named name (base+ext) in slot.
	Key(slot, name string) string
	// Write streams write's output to key atomically: a failed or aborted write must
	// not leave a committed file (so an interrupted append is a sidecar-less orphan).
	Write(key string, write func(w io.Writer) error) error
	// WriteAll writes b to key (the header sidecar).
	WriteAll(key string, b []byte) error
	// ReadAll reads the whole file at key (the header sidecar).
	ReadAll(key string) ([]byte, error)
	// Open opens the file at key for streaming; the caller closes it.
	Open(key string) (io.ReadCloser, error)
	// List returns every stored file, for the catalog-rebuild scan.
	List() ([]Object, error)
	// RemoveTree deletes every file belonging to slot.
	RemoveTree(slot string) error
}

// entry pairs a file's header sidecar and payload keys with its slot. incomplete
// marks a position missing one of the two — an interrupted append leaves a payload
// with no header sidecar; such an orphan is indexed but skipped, never read.
type entry struct {
	hdr        string
	payload    string
	slot       string
	incomplete bool
}

// Volume is the shared media.Volume implementation over a Store.
type Volume struct {
	store Store
	mu    sync.Mutex
	next  int
	idx   map[int]entry
}

// Open builds a Volume on store and indexes it (the cheap, filename-only scan).
func Open(store Store) (*Volume, error) {
	v := &Volume{store: store, idx: map[int]entry{}}
	if err := v.scan(); err != nil {
		return nil, err
	}
	return v, nil
}

var slug = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// stem is the friendly filename base (without extension) for a file.
func stem(pos int, h record.Header) string {
	if h.Kind == record.KindSeal {
		return fmt.Sprintf("%06d-seal", pos)
	}
	return fmt.Sprintf("%06d-%s-L%d", pos, slug.ReplaceAllString(h.DLE, "_"), h.Level)
}

// payloadExt is the extension for a file's payload, so it is recognizable and
// directly usable with stock tools. Kept here so the media don't depend on package
// filter.
func payloadExt(h record.Header) string {
	if h.Kind == record.KindSeal {
		return ".json"
	}
	switch h.Compress {
	case "gzip":
		return ".tar.gz"
	case "none", "":
		return ".tar"
	default: // zstd and any future codec named after its extension
		return ".tar." + codecExt(h.Compress)
	}
}

func codecExt(codec string) string {
	if codec == "zstd" {
		return "zst"
	}
	return codec
}

func (v *Volume) AppendFile(h record.Header, write func(w io.Writer) error) (int, error) {
	v.mu.Lock()
	pos := v.next
	v.next++
	v.mu.Unlock()

	base := stem(pos, h)
	payloadKey := v.store.Key(h.Slot, base+payloadExt(h))
	hdrKey := v.store.Key(h.Slot, base+".hdr")

	// Payload first (a clean archive), then the header sidecar — so an interrupted
	// write leaves a sidecar-less orphan that scan/rebuild ignores.
	if err := v.store.Write(payloadKey, write); err != nil {
		return 0, err
	}
	hb, err := json.Marshal(h)
	if err != nil {
		return 0, err
	}
	if err := v.store.WriteAll(hdrKey, append(hb, '\n')); err != nil {
		return 0, err
	}

	v.mu.Lock()
	v.idx[pos] = entry{hdr: hdrKey, payload: payloadKey, slot: h.Slot}
	v.mu.Unlock()
	return pos, nil
}

func (v *Volume) ReadFile(pos int) (record.Header, io.ReadCloser, error) {
	v.mu.Lock()
	e, ok := v.idx[pos]
	v.mu.Unlock()
	if !ok {
		return record.Header{}, nil, fmt.Errorf("no file at position %d", pos)
	}
	if e.incomplete {
		return record.Header{}, nil, fmt.Errorf("file at position %d is incomplete (interrupted append)", pos)
	}
	h, err := v.readHeader(e.hdr)
	if err != nil {
		return record.Header{}, nil, err
	}
	r, err := v.store.Open(e.payload)
	if err != nil {
		return record.Header{}, nil, err
	}
	return h, r, nil
}

func (v *Volume) Files() ([]record.FileInfo, error) {
	v.mu.Lock()
	entries := make(map[int]entry, len(v.idx))
	for pos, e := range v.idx {
		entries[pos] = e
	}
	v.mu.Unlock()

	out := make([]record.FileInfo, 0, len(entries))
	for pos, e := range entries {
		if e.incomplete {
			continue // orphan from an interrupted append; not a usable file
		}
		h, err := v.readHeader(e.hdr)
		if err != nil {
			// A present-but-unreadable header (a torn sidecar from a power-loss or
			// reordered write) is treated like a missing one: the file is not committed,
			// so skip it rather than abort the whole rebuild. Bit-rot on a file the seal
			// *does* commit is caught later by verify against the seal — enumeration is
			// best-effort, integrity is verify's job.
			continue
		}
		out = append(out, record.FileInfo{Pos: pos, Header: h})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pos < out[j].Pos })
	return out, nil
}

func (v *Volume) RemoveSlot(slot string) error {
	if err := v.store.RemoveTree(slot); err != nil {
		return err
	}
	v.mu.Lock()
	for pos, e := range v.idx {
		if e.slot == slot {
			delete(v.idx, pos)
		}
	}
	v.mu.Unlock()
	return nil
}

// scan builds the position index from the Store's file listing only — it does not
// read headers, so Open stays cheap. Each position has a .hdr and a payload paired
// by numeric filename prefix.
func (v *Volume) scan() error {
	objs, err := v.store.List()
	if err != nil {
		return err
	}
	max := -1
	for _, o := range objs {
		pos, err := strconv.Atoi(strings.SplitN(o.Base, "-", 2)[0])
		if err != nil {
			continue
		}
		e := v.idx[pos]
		e.slot = o.Slot
		if strings.HasSuffix(o.Base, ".hdr") {
			e.hdr = o.Key
		} else {
			e.payload = o.Key
		}
		v.idx[pos] = e
		if pos > max {
			max = pos
		}
	}
	// Mark incomplete positions: an interrupted append leaves a payload with no
	// header sidecar (or, rarer, a sidecar with no payload). Such an orphan is not a
	// usable file — it must never be read (readHeader on an empty key fails) — but it
	// stays indexed so pruning can reap it later. next still advances past max so a
	// new append never collides with an orphan payload still on disk.
	for pos, e := range v.idx {
		if e.hdr == "" || e.payload == "" {
			e.incomplete = true
			v.idx[pos] = e
		}
	}
	v.next = max + 1
	return nil
}

func (v *Volume) readHeader(key string) (record.Header, error) {
	data, err := v.store.ReadAll(key)
	if err != nil {
		return record.Header{}, fmt.Errorf("read header %s: %w", key, err)
	}
	var h record.Header
	if err := json.Unmarshal(data, &h); err != nil {
		return record.Header{}, fmt.Errorf("parse header %s: %w", key, err)
	}
	return h, nil
}
