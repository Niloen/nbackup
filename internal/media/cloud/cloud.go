// Package cloud implements media.Volume backed by a cloud object store via the Go
// CDK (gocloud.dev/blob). One medium type — "cloud" — covers S3, GCS, Azure Blob,
// and any S3-compatible store (MinIO, R2, B2, Wasabi); the backend is selected by
// the bucket URL scheme (s3://, gs://, azblob://), and file:// / mem:// drivers
// make the medium fully testable with no network or credentials.
//
// Like the disk medium, an object store is ADDRESS-IDENTIFIED: a bucket+key names
// a volume unambiguously, so it carries no on-medium label and runs none of the
// label-verify / changer / spanning machinery (it does not implement media.Labeled,
// Drive, Changer or Shelf). The run layout — clean payloads plus JSON header
// sidecars under runs/<run>/ — lives in package fslike, shared with the disk
// medium, so a run streams disk<->cloud unchanged; this package supplies only the
// object-store storage primitives.
package cloud

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/media/bucket"
	"github.com/Niloen/nbackup/internal/media/fslike"
)

func init() {
	// An object store is fslike-backed like disk, so it inherits the size profile and the
	// concurrent-write capability (eligible as a holding disk, e.g. S3 buffering a tape
	// landing) from the shared layout. What is cloud-specific: its url/prefix params, a
	// part-size policy (it splits a large archive into part-objects), and pricing (newCost).
	s := fslike.Spec()
	s.Type = "cloud"
	s.Params = []string{"url", "prefix", "part_size"}
	// Split each archive into <= part_size part-objects, defaulting to 10 GiB. The cap
	// keeps one object's multipart upload well under the 10000-part limit: at the
	// default 5 MiB upload buffer that ceiling is ~48.8 GiB, so an object above ~40 GiB
	// risks the original "exceeded total allowed MaxUploadParts" failure. Memory stays
	// flat — the 5 MiB streaming buffer is unchanged regardless of part_size. Sizes are
	// binary (powers of 1024) so they read as round numbers in object-store tooling.
	s.PartSize = media.PartSizePolicy{
		Default: 10 << 30, // 10 GiB
		Max:     40 << 30, // 40 GiB
		MaxNote: "an object store caps a single object's multipart upload at 10000 parts (~48.8 GiB at the 5 MiB buffer); use a smaller part_size so each part-object stays well under it",
	}
	s.Cost = newCost
	s.New = func(opts media.Options) (media.Volume, error) {
		url := opts.Get("url")
		if url == "" {
			return nil, fmt.Errorf("cloud medium requires a url (e.g. s3://bucket?region=…, gs://bucket, azblob://container)")
		}
		// part_size is honored here: a large archive is split into several part-objects
		// of <= part_size rather than one giant object, so a multi-GB archive never hits
		// the object store's multipart-upload part-count ceiling (S3 caps a single
		// object's upload at 10000 parts). Splitting is per archive on the one logical
		// volume; it stays fully concurrent (unlike a serial tape drive). The default and
		// upper bound live in the part-size policy above; the engine applies them. The
		// factory itself is part-size-agnostic — the writer drives the split.
		return open(url, opts.Get("prefix"))
	}
	media.Register(s)
}

// runsPrefix is the key prefix under which all run files live, mirroring the
// disk medium's runs/ subdirectory.
const runsPrefix = "runs/"

func open(url, prefix string) (media.Volume, error) {
	ctx := context.Background()
	b, err := bucket.Open(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open cloud bucket %q: %w", url, err)
	}
	if prefix != "" {
		// Confine every key to the configured prefix within the bucket. A trailing
		// slash makes it a clean folder boundary regardless of how it was written.
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		b = blob.PrefixedBucket(b, prefix)
	}
	v, err := fslike.Open(blobStore{ctx: ctx, bucket: b})
	if err != nil {
		b.Close()
		return nil, err
	}
	return v, nil
}

// blobStore is a fslike.Store over a gocloud blob bucket. Keys are object keys
// under runs/.
type blobStore struct {
	// ctx in a struct is accepted debt, forced by media.Volume's ctx-less read path
	// (revisit if Volume ever grows a ctx).
	ctx    context.Context
	bucket *blob.Bucket
}

func (s blobStore) Key(run, name string) string { return path.Join(runsPrefix, run, name) }

// Writer opens a streaming writer bound to ctx: Close commits the object; canceling ctx before Close
// abandons the (possibly multipart) upload — gocloud discards any buffered parts — so the caller's
// abort is just a ctx cancel, no bespoke unwind here.
func (s blobStore) Writer(ctx context.Context, key string) (io.WriteCloser, error) {
	return s.bucket.NewWriter(ctx, key, nil)
}

// Open opens the rng slice of the object at key. A sub-range is the object store's
// ranged GET — the whole point of the framed shape on cloud: selective restore pays
// for the covering frames' bytes, not the object's.
func (s blobStore) Open(key string, rng media.Range) (io.ReadCloser, error) {
	if rng.IsWhole() {
		return s.bucket.NewReader(s.ctx, key, nil)
	}
	length := rng.Len
	if length <= 0 {
		length = -1 // gocloud's "to the end"
	}
	return s.bucket.NewRangeReader(s.ctx, key, rng.Off, length, nil)
}

func (s blobStore) RemoveTree(run string) error {
	prefix := path.Join(runsPrefix, run) + "/"
	iter := s.bucket.List(&blob.ListOptions{Prefix: prefix})
	for {
		obj, err := iter.Next(s.ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if obj.IsDir {
			continue
		}
		if err := s.bucket.Delete(s.ctx, obj.Key); err != nil {
			return err
		}
	}
	return nil
}

func (s blobStore) Remove(key string) error {
	if err := s.bucket.Delete(s.ctx, key); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		return err
	}
	return nil
}

func (s blobStore) List() ([]fslike.Object, error) {
	iter := s.bucket.List(&blob.ListOptions{Prefix: runsPrefix})
	var out []fslike.Object
	for {
		obj, err := iter.Next(s.ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if obj.IsDir {
			continue
		}
		// Keys are runs/<run>/<base>; the run is the element after runsPrefix.
		rel := strings.TrimPrefix(obj.Key, runsPrefix)
		run := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			run = rel[:i]
		}
		out = append(out, fslike.Object{Key: obj.Key, Run: run, Base: path.Base(obj.Key)})
	}
	return out, nil
}
