// Package bucket opens gocloud blob buckets from one location string, shared by
// every medium that stores objects: a bucket URL (s3://, gs://, azblob://,
// file://, mem://) opens the matching driver, and a plain filesystem path opens
// fileblob on that directory — created on demand, with no .attrs metadata
// sidecars, so the on-disk layout is exactly the files a caller writes. That
// keeps a directory and a cloud bucket interchangeable behind *blob.Bucket.
package bucket

import (
	"context"
	"path/filepath"
	"strings"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	// Object-store drivers (URL schemes). Blank imports register them with gocloud.
	_ "gocloud.dev/blob/azureblob" // azblob://
	_ "gocloud.dev/blob/gcsblob"   // gs://
	_ "gocloud.dev/blob/memblob"   // mem://   (in-memory; tests)
	_ "gocloud.dev/blob/s3blob"    // s3://    (and S3-compatible)
)

// Open opens loc as a gocloud bucket: a URL by its scheme's driver, a plain
// path as a metadata-free fileblob directory.
func Open(ctx context.Context, loc string) (*blob.Bucket, error) {
	if !strings.Contains(loc, "://") {
		abs, err := filepath.Abs(loc)
		if err != nil {
			return nil, err
		}
		return fileblob.OpenBucket(abs, &fileblob.Options{
			CreateDir: true,
			Metadata:  fileblob.MetadataDontWrite,
		})
	}
	return blob.OpenBucket(ctx, loc)
}
