// Package cloud implements media.Volume backed by a cloud object store via the Go
// CDK (gocloud.dev/blob). One medium type — "cloud" — covers S3, GCS, Azure Blob,
// and any S3-compatible store (MinIO, R2, B2, Wasabi); the backend is selected by
// the bucket URL scheme (s3://, gs://, azblob://), and file:// / mem:// drivers
// make the medium fully testable with no network or credentials.
//
// Like the disk medium, an object store is ADDRESS-IDENTIFIED: a bucket+key names
// a volume unambiguously, so it carries no on-medium label and runs none of the
// label-verify / changer / spanning machinery (it does not implement media.Labeled,
// Drive, Changer or Shelf). Each file is stored as two objects under
// slots/<slot>/: a clean payload (<NNNNNN>-<dle>-L<n>.tar.<ext>, directly usable
// with stock tools after a plain GET) and a JSON header sidecar (<NNNNNN>-…-L<n>.hdr).
// Positions are volume-global file numbers paired by their numeric key prefix —
// the same layout as the disk medium, so a slot streams disk<->cloud unchanged.
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gocloud.dev/blob"
	// Object-store drivers (URL schemes). Blank imports register them with gocloud.
	_ "gocloud.dev/blob/azureblob" // azblob://
	_ "gocloud.dev/blob/fileblob"  // file://  (local dir; also used in tests)
	_ "gocloud.dev/blob/gcsblob"   // gs://
	_ "gocloud.dev/blob/memblob"   // mem://   (in-memory; tests)
	_ "gocloud.dev/blob/s3blob"    // s3://    (and S3-compatible)

	"github.com/Niloen/nbackup/internal/media"
)

func init() {
	media.RegisterVolume("cloud", func(opts media.Options) (media.Volume, error) {
		url := opts.Get("url")
		if url == "" {
			return nil, fmt.Errorf("cloud medium requires a url (e.g. s3://bucket?region=…, gs://bucket, azblob://container)")
		}
		// An object store is unbounded per object, so an archive is always a single
		// part; part_size (the tape-spanning chunk bound) is meaningless here and is
		// refused so the knob is never silently ignored — mirroring the disk medium.
		if opts.Get("part_size") != "" {
			return nil, fmt.Errorf("cloud medium does not support part_size (it is unbounded and never splits archives)")
		}
		return open(url, opts.Get("prefix"))
	})
	// Same capacity model as disk: a byte budget reclaimed per slot, oldest first.
	media.RegisterProfile("cloud", media.NewSizeProfile)
	media.RegisterParams("cloud", "url", "prefix", "part_size")
}

// slotsPrefix is the key prefix under which all slot files live, mirroring the
// disk medium's slots/ subdirectory.
const slotsPrefix = "slots/"

// entry pairs a file's header sidecar and payload object keys.
type entry struct {
	hdr     string
	payload string
}

type volume struct {
	ctx    context.Context
	bucket *blob.Bucket
	mu     sync.Mutex
	next   int
	idx    map[int]entry
}

func open(url, prefix string) (*volume, error) {
	ctx := context.Background()
	bucket, err := blob.OpenBucket(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open cloud bucket %q: %w", url, err)
	}
	if prefix != "" {
		// Confine every key to the configured prefix within the bucket. A trailing
		// slash makes it a clean folder boundary regardless of how it was written.
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		bucket = blob.PrefixedBucket(bucket, prefix)
	}
	v := &volume{ctx: ctx, bucket: bucket, idx: map[int]entry{}}
	if err := v.scan(); err != nil {
		bucket.Close()
		return nil, err
	}
	return v, nil
}

var slug = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// stem is the friendly key base (without extension) for a file — identical to the
// disk medium so payloads are interchangeable and recognizable.
func stem(pos int, h media.Header) string {
	if h.Kind == media.KindSeal {
		return fmt.Sprintf("%06d-seal", pos)
	}
	return fmt.Sprintf("%06d-%s-L%d", pos, slug.ReplaceAllString(h.DLE, "_"), h.Level)
}

// payloadExt is the extension for a file's payload object, so a plain GET yields a
// directly usable archive (no header to skip). Kept local so the medium does not
// depend on package filter.
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
	payloadKey := path.Join(slotsPrefix, h.Slot, base+payloadExt(h))
	hdrKey := path.Join(slotsPrefix, h.Slot, base+".hdr")

	// Payload first (a clean archive), then the header sidecar — so an interrupted
	// upload leaves a sidecar-less orphan that scan()/rebuild ignores, the same
	// atomicity the disk and tape media rely on.
	if err := v.writeObject(payloadKey, write); err != nil {
		return 0, err
	}
	hb, err := json.Marshal(h)
	if err != nil {
		return 0, err
	}
	if err := v.bucket.WriteAll(v.ctx, hdrKey, append(hb, '\n'), nil); err != nil {
		return 0, err
	}

	v.mu.Lock()
	v.idx[pos] = entry{hdr: hdrKey, payload: payloadKey}
	v.mu.Unlock()
	return pos, nil
}

// writeObject streams write's output to key, aborting the upload (rather than
// committing a partial object) if write fails.
func (v *volume) writeObject(key string, write func(w io.Writer) error) error {
	ctx, cancel := context.WithCancel(v.ctx)
	defer cancel()
	w, err := v.bucket.NewWriter(ctx, key, nil)
	if err != nil {
		return err
	}
	if err := write(w); err != nil {
		cancel()  // abort the (possibly multipart) upload
		w.Close() // best-effort; the canceled context discards any buffered parts
		return err
	}
	// Close commits the object; its error is the authoritative write result.
	return w.Close()
}

func (v *volume) ReadFile(pos int) (media.Header, io.ReadCloser, error) {
	v.mu.Lock()
	e, ok := v.idx[pos]
	v.mu.Unlock()
	if !ok {
		return media.Header{}, nil, fmt.Errorf("no file at position %d", pos)
	}
	h, err := v.readHeader(e.hdr)
	if err != nil {
		return media.Header{}, nil, err
	}
	r, err := v.bucket.NewReader(v.ctx, e.payload, nil)
	if err != nil {
		return media.Header{}, nil, err
	}
	return h, r, nil
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
		h, err := v.readHeader(e.hdr)
		if err != nil {
			return nil, err
		}
		out = append(out, media.FileInfo{Pos: pos, Header: h})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pos < out[j].Pos })
	return out, nil
}

func (v *volume) RemoveSlot(slot string) error {
	prefix := path.Join(slotsPrefix, slot) + "/"
	iter := v.bucket.List(&blob.ListOptions{Prefix: prefix})
	for {
		obj, err := iter.Next(v.ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if obj.IsDir {
			continue
		}
		if err := v.bucket.Delete(v.ctx, obj.Key); err != nil {
			return err
		}
	}
	v.mu.Lock()
	for pos, e := range v.idx {
		if strings.HasPrefix(e.payload, prefix) {
			delete(v.idx, pos)
		}
	}
	v.mu.Unlock()
	return nil
}

// scan builds the position index from object keys only — it does not read header
// objects, so open stays cheap (one paginated List of the slots/ prefix). Each
// position has a .hdr sidecar and a payload paired by numeric key prefix. This is
// the catalog-rebuild path; normal ops resolve positions from the catalog.
func (v *volume) scan() error {
	iter := v.bucket.List(&blob.ListOptions{Prefix: slotsPrefix})
	max := -1
	for {
		obj, err := iter.Next(v.ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if obj.IsDir {
			continue
		}
		name := path.Base(obj.Key)
		pos, err := strconv.Atoi(strings.SplitN(name, "-", 2)[0])
		if err != nil {
			continue
		}
		e := v.idx[pos]
		if strings.HasSuffix(name, ".hdr") {
			e.hdr = obj.Key
		} else {
			e.payload = obj.Key
		}
		v.idx[pos] = e
		if pos > max {
			max = pos
		}
	}
	v.next = max + 1
	return nil
}

func (v *volume) readHeader(key string) (media.Header, error) {
	data, err := v.bucket.ReadAll(v.ctx, key)
	if err != nil {
		return media.Header{}, fmt.Errorf("read header %s: %w", key, err)
	}
	var h media.Header
	if err := json.Unmarshal(data, &h); err != nil {
		return media.Header{}, fmt.Errorf("parse header %s: %w", key, err)
	}
	return h, nil
}
