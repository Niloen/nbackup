// Package fslike implements the media.Volume layout shared by the address-
// identified media (disk, cloud): each file is two artifacts under
// runs/<run>/ — a clean payload (<NNNNNN>-<dle>-L<n>.tar.<ext>, directly usable
// with stock tools) and a JSON header sidecar (<NNNNNN>-…-L<n>.hdr). Positions are
// volume-global file numbers paired by their numeric filename prefix. A split archive
// (one written under a part_size cap) appends a .pNNN part-index suffix to the payload
// (…-L<n>.tar.gz.p000, .p001, …) so the slices group and order by name and no fragment
// poses as a standalone artifact; the sidecar keeps the plain .hdr name (see payloadExt).
//
// The layout (stems, extensions, run subtrees, payload-first atomicity, the
// scan that indexes by filename prefix) lives here once; a backing Store supplies
// only the storage primitives (a local directory for disk, an object bucket for
// cloud). Because both media share this code, a run streams disk<->cloud byte-for-
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
// read/open, the run it belongs to, and its basename (whose numeric prefix gives
// the position).
type Object struct {
	Key  string
	Run  string
	Base string
}

// Store is the storage seam a fslike.Volume runs on. Keys are opaque handles the
// Store mints (via Key) and consumes; the fslike layer never parses them, so disk
// (filesystem paths) and cloud (object keys) supply whatever form suits the backend.
type Store interface {
	// Key builds the storage key for a file named name (base+ext) in run.
	Key(run, name string) string
	// Writer opens a streaming writer for key. Close commits the file; if ctx is canceled the file
	// must not commit (a cloud upload is abandoned; a local partial is left for the caller to discard),
	// so an aborted append never leaves a committed payload.
	Writer(ctx context.Context, key string) (io.WriteCloser, error)
	// Open opens the rng slice of the file at key for streaming (Range{} = the whole
	// file); the caller closes it. Both fslike backings serve genuine sub-ranges — a
	// disk file seeks, a cloud object issues a ranged GET — which is what makes every
	// fslike medium range-capable.
	Open(key string, rng media.Range) (io.ReadCloser, error)
	// List returns every stored file, for the catalog-rebuild scan.
	List() ([]Object, error)
	// RemoveTree deletes every file belonging to run.
	RemoveTree(run string) error
	// Remove deletes a single file by key (one archive's payload or sidecar). A
	// missing key is not an error (idempotent reclamation).
	Remove(key string) error
}

// writeAll writes b as the whole file at key (a header sidecar) through the
// Store's streaming Writer.
func writeAll(ctx context.Context, s Store, key string, b []byte) error {
	w, err := s.Writer(ctx, key)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		w.Close() //nolint:errcheck — the write error is the one to report
		return err
	}
	return w.Close()
}

// readAll reads the whole file at key (a header sidecar) through the Store's
// streaming Open.
func readAll(s Store, key string) ([]byte, error) {
	r, err := s.Open(key, media.Range{})
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// entry pairs a file's header sidecar and payload keys with its run. incomplete
// marks a position missing one of the two — an interrupted append leaves a payload
// with no header sidecar; such an orphan is indexed but skipped, never read. It also
// marks a position reserved by an in-flight AppendFile (both keys still empty): the
// reservation keeps the index from lagging the directory so a concurrent reclaim sees
// the run is still in use (see AppendFile).
type entry struct {
	hdr        string
	payload    string
	run        string
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
// capacity model and the concurrent-write capability are consequences of the run layout
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
	// The base extension is the archiver's own (recorded per-archive, like the
	// schemes) — ".tar" for gnutar, ".raw" or the operator's for pipe. An archive
	// written before the field existed records ""; every archiver then was gnutar.
	ext := h.Ext
	if ext == "" {
		ext = ".tar"
	}
	switch h.Compress {
	case "gzip":
		ext += ".gz"
	case "none", "":
	default: // zstd and any future compressor named after its extension
		ext += "." + compressExt(h.Compress)
	}
	// An encrypted payload is ciphertext, not a readable tar/gz: append the scheme
	// (gpg) so the name says "decrypt first" and a stock `tar`/`gzip` is not reached
	// for on a gpg blob. Only the payload carries it — the commit footer and member
	// index stay plaintext (and keep their .json/.json.gz names).
	if h.Encrypt != "" && h.Encrypt != "none" {
		ext += "." + h.Encrypt
	}
	// The part-index suffix position IS the shape (docs/design/archive-shapes.md):
	//
	// An ATOM is a complete valid file of its type (one whole gpg message), so its
	// part index goes BEFORE the extensions — …-L0.p000.tar.zst.gpg — keeping the
	// name honest (tools recognize it, the file-loop stock recovery works per file)
	// and making the shape operator-visible at a bare bucket listing.
	if h.Shape.StandaloneParts() {
		return fmt.Sprintf(".p%03d", h.Part) + ext
	}
	// A split archive's payload is one SLICE of a multi-part whole, not a standalone
	// file: append a .pNNN part-index suffix AFTER the .tar/.gz/.gpg extension so the
	// name no longer claims to be a directly-openable artifact (a stock `tar` on a lone
	// part fails fast instead of yielding garbage) and the siblings group and order by
	// name — `cat <stem>.tar.gz.p* | tar xz` reconstructs. The header sidecar keeps the
	// plain <stem>.hdr (each part already has its own position-prefixed sidecar). Set
	// for every part including a sole one, since the total is unknown when the name is
	// minted (see record.Header.Split). The position prefix remains the authoritative
	// order; .pNNN is the human-facing within-archive index.
	if h.Split {
		ext += fmt.Sprintf(".p%03d", h.Part)
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
	// reservation a concurrent RemoveFile reclaiming the run's last file would judge
	// the run empty from the lagging index and RemoveTree the subtree — taking the
	// freshly written, not-yet-indexed payload with it. The reservation makes that
	// RemoveFile see the in-flight append and leave the directory alone. Finalized on
	// the writer's Close once both artifacts have landed; dropped there on abort/error.
	v.idx[pos] = entry{run: h.Run, incomplete: true}
	v.mu.Unlock()

	base := stem(pos, h)
	payloadKey := v.store.Key(h.Run, base+payloadExt(h))
	hdrKey := v.store.Key(h.Run, base+".hdr")

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
	return &fileWriter{v: v, ctx: ctx, pw: pw, pos: pos, run: h.Run, hdrKey: hdrKey, payloadKey: payloadKey, hdr: append(hb, '\n')}, nil
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
	run        string
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
	if err := writeAll(f.ctx, f.v.store, f.hdrKey, f.hdr); err != nil {
		f.v.drop(f.pos)
		return err
	}
	f.v.mu.Lock()
	f.v.idx[f.pos] = entry{hdr: f.hdrKey, payload: f.payloadKey, run: f.run}
	f.v.mu.Unlock()
	return nil
}

// ReadFile opens the rng slice of the file at pos. The header always comes whole from
// the sidecar (asserting identity costs nothing here); only the payload is opened
// ranged through the Store. A payload here is pure archive bytes (the header lives in
// the sidecar, not inline), so offset 0 is the payload's first byte.
func (v *Volume) ReadFile(pos int, rng media.Range) (record.Header, io.ReadCloser, error) {
	v.mu.Lock()
	e, ok := v.idx[pos]
	v.mu.Unlock()
	if !ok {
		return record.Header{}, nil, fmt.Errorf("no file at position %d", pos)
	}
	if e.incomplete {
		switch {
		case e.payload == "" && e.hdr != "":
			return record.Header{}, nil, fmt.Errorf("file at position %d is missing its payload (deleted, or never landed)", pos)
		case e.hdr == "" && e.payload != "":
			return record.Header{}, nil, fmt.Errorf("file at position %d is incomplete (interrupted append: payload with no header sidecar)", pos)
		default:
			return record.Header{}, nil, fmt.Errorf("file at position %d is missing (no payload or header)", pos)
		}
	}
	h, err := v.readHeader(e.hdr)
	if err != nil {
		return record.Header{}, nil, err
	}
	r, err := v.store.Open(e.payload, rng)
	if err != nil {
		return record.Header{}, nil, err
	}
	return h, r, nil
}

func (v *Volume) Files() ([]record.FileInfo, error) { return v.FilesExcept(nil) }

// FilesExcept is Files() restricted to positions the caller does not already know: it
// reads (and returns) the header only for entries whose position is absent from known.
// The orphan sweep passes the catalog's placement positions as known, so on a healthy
// store it opens almost nothing — every object read on a cloud medium is a round trip.
// known may be nil (a read from a nil map is false), in which case this is exactly
// Files(). Skipped positions are simply omitted from the result.
func (v *Volume) FilesExcept(known map[int]bool) ([]record.FileInfo, error) {
	v.mu.Lock()
	entries := make(map[int]entry, len(v.idx))
	for pos, e := range v.idx {
		if known[pos] {
			continue // the caller already accounts for this file; don't read its header
		}
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

// IncompleteFiles reports the positions of files an interrupted append left
// half-written — exactly one of the payload/header pair present. scan() marks these
// incomplete (and Files() skips them), so they belong to no archive and a prune sweep
// reaps them. A both-empty entry is an in-flight AppendFile reservation, not a stored
// fragment, so it is excluded (it also cannot survive a crash: scan() only ever
// reconstructs entries from objects actually on the store).
func (v *Volume) IncompleteFiles() ([]int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []int
	for pos, e := range v.idx {
		if (e.hdr == "") != (e.payload == "") { // exactly one half present
			out = append(out, pos)
		}
	}
	sort.Ints(out)
	return out, nil
}

// RemoveFile deletes the payload and header sidecar at pos and drops them from the
// index. The run is read from the index entry; when removing this file empties the
// run's subtree, the now-empty directory is reclaimed too, leaving no stray
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
		if other.run == e.run {
			return nil // the run still has files; keep its subtree
		}
	}
	return v.store.RemoveTree(e.run) // last file in the run: reclaim the empty directory
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
		e.run = o.Run
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
	data, err := readAll(v.store, key)
	if err != nil {
		return record.Header{}, fmt.Errorf("read header %s: %w", key, err)
	}
	var h record.Header
	if err := json.Unmarshal(data, &h); err != nil {
		return record.Header{}, fmt.Errorf("parse header %s: %w", key, err)
	}
	return h, nil
}
