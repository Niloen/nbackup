// Package xfer composes backup stream pipelines, analogous to Amanda's Xfer
// API (source -> filter -> dest). It keeps compression and checksumming
// separate from both the dump method (which produces a raw stream) and the
// medium (which stores bytes). Compression is in process, so no external zstd
// binary is required.
package xfer

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Sink is a write filter that zstd-compresses incoming data, checksums the
// compressed bytes, and counts them before passing them to a destination
// writer. Write the raw (uncompressed) stream to it; after Close, the checksum
// and compressed size describe what reached the destination.
type Sink struct {
	zw      *zstd.Encoder
	hasher  hash.Hash
	counter *countWriter
}

// NewZstdSink wraps dst with zstd compression, sha256, and byte counting.
func NewZstdSink(dst io.Writer) (*Sink, error) {
	hasher := sha256.New()
	counter := &countWriter{}
	zw, err := zstd.NewWriter(io.MultiWriter(dst, hasher, counter))
	if err != nil {
		return nil, err
	}
	return &Sink{zw: zw, hasher: hasher, counter: counter}, nil
}

// Write compresses p toward the destination.
func (s *Sink) Write(p []byte) (int, error) { return s.zw.Write(p) }

// Close flushes and finishes the compressed stream. It does not close the
// underlying destination writer (the caller owns that).
func (s *Sink) Close() error { return s.zw.Close() }

// SHA256 returns the hex checksum of the compressed bytes (valid after Close).
func (s *Sink) SHA256() string { return hex.EncodeToString(s.hasher.Sum(nil)) }

// Compressed returns the number of compressed bytes written (valid after Close).
func (s *Sink) Compressed() int64 { return s.counter.n }

// NewZstdSource wraps a compressed reader, decompressing on read. The returned
// ReadCloser must be closed by the caller.
func NewZstdSource(src io.Reader) (io.ReadCloser, error) {
	zr, err := zstd.NewReader(src)
	if err != nil {
		return nil, err
	}
	return zr.IOReadCloser(), nil
}

// HashReader returns the hex sha256 of everything read from r.
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
