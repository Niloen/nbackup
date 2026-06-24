package xfer

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// Limiter is a shared, token-bucket bandwidth cap — NBackup's network politeness,
// the read/write analogue of the CPU politeness `nice` already provides. One
// Limiter wraps any number of streams that should draw from a single budget: the
// part writes of several concurrent workers to one medium (so the aggregate stays
// under the cap, like Amanda's netusage), or the parts of one read. It throttles
// the medium-facing stream, so a slow consumer/producer back-pressures the
// one-pass pipeline through its pipe rather than buffering — no holding disk, no
// deadlock (the wait is a timer sleep, never a lock).
//
// A nil *Limiter is the uncapped case and is the common one: its wrappers return
// the stream unchanged, so behavior is byte-for-byte identical to having no
// limiter at all. Every method is therefore safe to call on a nil receiver.
type Limiter struct {
	lim   *rate.Limiter
	burst int
}

// Burst bounds, in bytes:
//   - minBurst keeps the bucket at least one io.Copy buffer (32 KiB) deep so a
//     typical write is admitted in a single WaitN rather than split.
//   - maxBurst caps how much may pass in an idle-bucket instant, so a freshly
//     started stream cannot briefly exceed the cap by more than ~1 MiB — negligible
//     over a real multi-gigabyte dump, while keeping the sustained rate honest.
const (
	minBurst = 32 << 10
	maxBurst = 1 << 20
)

// NewLimiter returns a Limiter capping aggregate throughput to bytesPerSec, or nil
// when bytesPerSec <= 0 (uncapped — the default). The returned Limiter is safe for
// concurrent use, so one instance is shared across a medium's concurrent streams.
func NewLimiter(bytesPerSec int64) *Limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	burst := bytesPerSec
	if burst < minBurst {
		burst = minBurst
	}
	if burst > maxBurst {
		burst = maxBurst
	}
	return &Limiter{lim: rate.NewLimiter(rate.Limit(bytesPerSec), int(burst)), burst: int(burst)}
}

// Writer wraps w so writes are paced to the cap, blocking the calling goroutine
// for the bytes it is about to write. On the write path the caller is the consumer
// draining the pipe into the volume, so the sleep back-pressures the producer (tar
// → compress → encrypt) through the pipe.
func (l *Limiter) Writer(w io.Writer) io.Writer {
	if l == nil {
		return w
	}
	return &limitWriter{dst: w, l: l}
}

// Reader wraps r so reads are paced to the cap, blocking for the bytes just read —
// the restore/un-vault/drill download analogue of Writer.
func (l *Limiter) Reader(r io.Reader) io.Reader {
	if l == nil {
		return r
	}
	return &limitReader{src: r, l: l}
}

// ReadCloser is Reader for a ReadCloser, preserving Close — what the engine wraps
// around each part stream a medium hands back.
func (l *Limiter) ReadCloser(rc io.ReadCloser) io.ReadCloser {
	if l == nil {
		return rc
	}
	return readCloser{Reader: l.Reader(rc), Closer: rc}
}

type limitWriter struct {
	dst io.Writer
	l   *Limiter
}

func (w *limitWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > w.l.burst {
			n = w.l.burst // never request more than the bucket can hold
		}
		// Reserve tokens (sleeping until they are available) before writing, so the
		// rate bounds the bytes about to land. WaitN with a background context and
		// n <= burst never errors; the timer sleep is the whole mechanism.
		if err := w.l.lim.WaitN(context.Background(), n); err != nil {
			return written, err
		}
		m, err := w.dst.Write(p[:n])
		written += m
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

type limitReader struct {
	src io.Reader
	l   *Limiter
}

func (r *limitReader) Read(p []byte) (int, error) {
	if len(p) > r.l.burst {
		p = p[:r.l.burst] // bound the read so the WaitN below fits the bucket
	}
	n, err := r.src.Read(p)
	if n > 0 {
		// Pace on the bytes actually read; a read can only report its size after the
		// fact, so the wait trails by one chunk — exact on average over the stream.
		_ = r.l.lim.WaitN(context.Background(), n)
	}
	return n, err
}

type readCloser struct {
	io.Reader
	io.Closer
}
