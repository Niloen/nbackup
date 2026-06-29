// Package cloud implements media.Volume backed by a cloud object store via the Go
// CDK (gocloud.dev/blob). One medium type — "cloud" — covers S3, GCS, Azure Blob,
// and any S3-compatible store (MinIO, R2, B2, Wasabi); the backend is selected by
// the bucket URL scheme (s3://, gs://, azblob://), and file:// / mem:// drivers
// make the medium fully testable with no network or credentials.
//
// Like the disk medium, an object store is ADDRESS-IDENTIFIED: a bucket+key names
// a volume unambiguously, so it carries no on-medium label and runs none of the
// label-verify / changer / spanning machinery (it does not implement media.Labeled,
// Drive, Changer or Shelf). The slot layout — clean payloads plus JSON header
// sidecars under slots/<slot>/ — lives in package fslike, shared with the disk
// medium, so a slot streams disk<->cloud unchanged; this package supplies only the
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
	// Object-store drivers (URL schemes). Blank imports register them with gocloud.
	_ "gocloud.dev/blob/azureblob" // azblob://
	_ "gocloud.dev/blob/fileblob"  // file://  (local dir; also used in tests)
	_ "gocloud.dev/blob/gcsblob"   // gs://
	_ "gocloud.dev/blob/memblob"   // mem://   (in-memory; tests)
	_ "gocloud.dev/blob/s3blob"    // s3://    (and S3-compatible)

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/media/fslike"
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
		if err := media.RejectPartSize(opts, "cloud"); err != nil {
			return nil, err
		}
		return open(url, opts.Get("prefix"))
	})
	// Same capacity model as disk: a byte budget reclaimed per slot, oldest first.
	media.RegisterProfile("cloud", media.NewSizeProfile)
	media.RegisterParams("cloud", "url", "prefix", "part_size")
	// An object store accepts concurrent puts and deletes individual objects — eligible as a
	// holding disk (e.g. S3 buffering a tape landing).
	media.RegisterConcurrentWrite("cloud")
}

// slotsPrefix is the key prefix under which all slot files live, mirroring the
// disk medium's slots/ subdirectory.
const slotsPrefix = "slots/"

func open(url, prefix string) (media.Volume, error) {
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
	v, err := fslike.Open(blobStore{ctx: ctx, bucket: bucket})
	if err != nil {
		bucket.Close()
		return nil, err
	}
	return v, nil
}

// blobStore is a fslike.Store over a gocloud blob bucket. Keys are object keys
// under slots/.
type blobStore struct {
	ctx    context.Context
	bucket *blob.Bucket
}

func (s blobStore) Key(slot, name string) string { return path.Join(slotsPrefix, slot, name) }

// Writer opens a streaming writer bound to ctx: Close commits the object; canceling ctx before Close
// abandons the (possibly multipart) upload — gocloud discards any buffered parts — so the caller's
// abort is just a ctx cancel, no bespoke unwind here.
func (s blobStore) Writer(ctx context.Context, key string) (io.WriteCloser, error) {
	return s.bucket.NewWriter(ctx, key, nil)
}

func (s blobStore) WriteAll(ctx context.Context, key string, b []byte) error {
	return s.bucket.WriteAll(ctx, key, b, nil)
}

func (s blobStore) ReadAll(key string) ([]byte, error) { return s.bucket.ReadAll(s.ctx, key) }

func (s blobStore) Open(key string) (io.ReadCloser, error) {
	return s.bucket.NewReader(s.ctx, key, nil)
}

func (s blobStore) RemoveTree(slot string) error {
	prefix := path.Join(slotsPrefix, slot) + "/"
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
	iter := s.bucket.List(&blob.ListOptions{Prefix: slotsPrefix})
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
		// Keys are slots/<slot>/<base>; the slot is the element after slotsPrefix.
		rel := strings.TrimPrefix(obj.Key, slotsPrefix)
		slot := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			slot = rel[:i]
		}
		out = append(out, fslike.Object{Key: obj.Key, Slot: slot, Base: path.Base(obj.Key)})
	}
	return out, nil
}
