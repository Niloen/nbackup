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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Niloen/nbackup/internal/media"
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
	// Writer opens a streaming writer for key. Close commits the file; if ctx is canceled the file
	// must not commit (a cloud upload is abandoned; a local partial is left for the caller to discard),
	// so an aborted append never leaves a committed payload.
	Writer(ctx context.Context, key string) (io.WriteCloser, error)
	// WriteAll writes b to key (the header sidecar).
	WriteAll(ctx context.Context, key string, b []byte) error
	// ReadAll reads the whole file at key (the header sidecar).
	ReadAll(key string) ([]byte, error)
	// Open opens the file at key for streaming; the caller closes it.
	Open(key string) (io.ReadCloser, error)
	// List returns every stored file, for the catalog-rebuild scan.
	List() ([]Object, error)
	// RemoveTree deletes every file belonging to slot.
	RemoveTree(slot string) error
	// Remove deletes a single file by key (one archive's payload or sidecar). A
	// missing key is not an error (idempotent reclamation).
	Remove(key string) error
}

// entry pairs a file's header sidecar and payload keys with its slot. incomplete
// marks a position missing one of the two — an interrupted append leaves a payload
// with no header sidecar; such an orphan is indexed but skipped, never read. It also
// marks a position reserved by an in-flight AppendFile (both keys still empty): the
// reservation keeps the index from lagging the directory so a concurrent reclaim sees
// the slot is still in use (see AppendFile).
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

// Spec returns the registration facts every fslike-backed medium shares: the byte-budget
// capacity model and the concurrent-write capability are consequences of the slot layout
// itself, not of the backing Store. A medium (disk, cloud) starts from this and fills in
// Type, New, Params, and any distinctive PartSize/Cost before calling media.Register, so
// it cannot silently omit the size profile or the concurrent-write flag the layout gives.
func Spec() media.Spec {
	return media.Spec{
		Profile:         media.NewSizeProfile,
		ConcurrentWrite: true,
	}
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
	switch h.Kind {
	case record.KindCommit:
		return fmt.Sprintf("%06d-%s-L%d-commit", pos, slug.ReplaceAllString(h.DLE, "_"), h.Level)
	case record.KindIndex:
		return fmt.Sprintf("%06d-%s-L%d-index", pos, slug.ReplaceAllString(h.DLE, "_"), h.Level)
	}
	return fmt.Sprintf("%06d-%s-L%d", pos, slug.ReplaceAllString(h.DLE, "_"), h.Level)
}

// payloadExt is the extension for a file's payload, so it is recognizable and
// directly usable with stock tools. Kept here so the media don't depend on package
// compress.
func payloadExt(h record.Header) string {
	switch h.Kind {
	case record.KindCommit:
		return ".json"
	case record.KindIndex:
		return ".json.gz"
	}
	var ext string
	switch h.Compress {
	case "gzip":
		ext = ".tar.gz"
	case "none", "":
		ext = ".tar"
	default: // zstd and any future compressor named after its extension
		ext = ".tar." + compressExt(h.Compress)
	}
	// An encrypted payload is ciphertext, not a readable tar/gz: append the scheme
	// (gpg) so the name says "decrypt first" and a stock `tar`/`gzip` is not reached
	// for on a gpg blob. Only the payload carries it — the commit footer and member
	// index stay plaintext (and keep their .json/.json.gz names).
	if h.Encrypt != "" && h.Encrypt != "none" {
		ext += "." + h.Encrypt
	}
	return ext
}

func compressExt(compress string) string {
	if compress == "zstd" {
		return "zst"
	}
	return compress
}

func (v *Volume) AppendFile(ctx context.Context, h record.Header) (media.FileWriter, error) {
	v.mu.Lock()
	pos := v.next
	v.next++
	// Reserve the position before writing the files, so the index never lags the
	// directory. A dumper creates the payload on disk before the position is indexed,
	// and does the I/O outside the lock so appends run in parallel; without this
	// reservation a concurrent RemoveFile reclaiming the slot's last file would judge
	// the slot empty from the lagging index and RemoveTree the subtree — taking the
	// freshly written, not-yet-indexed payload with it. The reservation makes that
	// RemoveFile see the in-flight append and leave the directory alone. Finalized on
	// the writer's Close once both artifacts have landed; dropped there on abort/error.
	v.idx[pos] = entry{slot: h.Slot, incomplete: true}
	v.mu.Unlock()

	base := stem(pos, h)
	payloadKey := v.store.Key(h.Slot, base+payloadExt(h))
	hdrKey := v.store.Key(h.Slot, base+".hdr")

	hb, err := json.Marshal(h)
	if err != nil {
		v.drop(pos)
		return nil, err
	}
	pw, err := v.store.Writer(ctx, payloadKey)
	if err != nil {
		v.drop(pos)
		return nil, err
	}
	return &fileWriter{v: v, ctx: ctx, pw: pw, pos: pos, slot: h.Slot, hdrKey: hdrKey, payloadKey: payloadKey, hdr: append(hb, '\n')}, nil
}

// drop releases an in-flight position's reservation (an aborted or failed append).
func (v *Volume) drop(pos int) {
	v.mu.Lock()
	delete(v.idx, pos)
	v.mu.Unlock()
}

// fileWriter is the payload writer for one in-flight AppendFile. Write streams to the payload; Close
// commits — payload first, then the header sidecar (so an interrupted write leaves a sidecar-less
// orphan a scan ignores) — or, if ctx was canceled, abandons the file and drops the reservation.
type fileWriter struct {
	v          *Volume
	ctx        context.Context
	pw         io.WriteCloser
	pos        int
	slot       string
	hdrKey     string
	payloadKey string
	hdr        []byte
}

func (f *fileWriter) Pos() int                    { return f.pos }
func (f *fileWriter) Write(p []byte) (int, error) { return f.pw.Write(p) }

func (f *fileWriter) Close() error {
	cerr := f.pw.Close() // commits (cloud) or, on a canceled ctx, abandons the upload
	if f.ctx.Err() != nil {
		// Aborted: a local partial is left as a sidecar-less orphan (no header); the reservation goes.
		f.v.drop(f.pos)
		return f.ctx.Err()
	}
	if cerr != nil {
		f.v.drop(f.pos)
		return cerr
	}
	if err := f.v.store.WriteAll(f.ctx, f.hdrKey, f.hdr); err != nil {
		f.v.drop(f.pos)
		return err
	}
	f.v.mu.Lock()
	f.v.idx[f.pos] = entry{hdr: f.hdrKey, payload: f.payloadKey, slot: f.slot}
	f.v.mu.Unlock()
	return nil
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

// RemoveFile deletes the payload and header sidecar at pos and drops them from the
// index. The slot is read from the index entry; when removing this file empties the
// slot's subtree, the now-empty directory is reclaimed too, leaving no stray
// directory behind. A position already gone is a no-op (idempotent).
func (v *Volume) RemoveFile(pos int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.idx[pos]
	if !ok {
		return nil // already gone (idempotent)
	}
	if e.payload != "" {
		if err := v.store.Remove(e.payload); err != nil {
			return err
		}
	}
	if e.hdr != "" {
		if err := v.store.Remove(e.hdr); err != nil {
			return err
		}
	}
	delete(v.idx, pos)
	for _, other := range v.idx {
		if other.slot == e.slot {
			return nil // the slot still has files; keep its subtree
		}
	}
	return v.store.RemoveTree(e.slot) // last file in the slot: reclaim the empty directory
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
