// Package xfer holds the light, in-process pieces of the backup stream pipeline:
// checksumming and byte counting. The heavy part — compression — is run as an
// external child process (see package filter), so nb stays a thin orchestrator.
package xfer

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"sync/atomic"
)

// Meter is a write filter that passes bytes through to a destination while
// computing their sha256 and counting them. Wrap the destination object with a
// Meter and write the compressed stream to it; after the writes complete, SHA256
// and Bytes describe exactly what reached the destination. The byte count is
// updated atomically, so Bytes may be polled from another goroutine for live
// progress while Write runs.
type Meter struct {
	dst io.Writer
	h   hash.Hash
	n   atomic.Int64
}

// NewMeter wraps dst with sha256 + byte counting.
func NewMeter(dst io.Writer) *Meter {
	return &Meter{dst: dst, h: sha256.New()}
}

func (m *Meter) Write(p []byte) (int, error) {
	n, err := m.dst.Write(p)
	if n > 0 {
		m.h.Write(p[:n])
		m.n.Add(int64(n))
	}
	return n, err
}

// SHA256 returns the hex checksum of everything written so far. Call only after
// the writes complete; the hash is not safe to read concurrently with Write.
func (m *Meter) SHA256() string { return hex.EncodeToString(m.h.Sum(nil)) }

// Bytes returns the number of bytes written so far. Safe to call concurrently
// with Write.
func (m *Meter) Bytes() int64 { return m.n.Load() }

// Counter is a write filter that passes bytes through while counting them and
// invoking an optional callback with the running total — used to meter the
// uncompressed source stream for live progress. The callback runs inline on the
// writing goroutine, so it must be cheap.
type Counter struct {
	dst    io.Writer
	n      int64
	report func(total int64)
}

// NewCounter wraps dst, calling report (if non-nil) with the cumulative byte
// count after each successful write.
func NewCounter(dst io.Writer, report func(total int64)) *Counter {
	return &Counter{dst: dst, report: report}
}

func (c *Counter) Write(p []byte) (int, error) {
	n, err := c.dst.Write(p)
	if n > 0 {
		c.n += int64(n)
		if c.report != nil {
			c.report(c.n)
		}
	}
	return n, err
}

// Bytes returns the number of bytes written so far.
func (c *Counter) Bytes() int64 { return c.n }

// HashReader returns the hex sha256 of everything read from r.
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
