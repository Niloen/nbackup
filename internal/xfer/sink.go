package xfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// SHA256 returns the hex sha256 of everything read from r. It is the one-shot,
// off-Transfer hashing used when a stream is summed outside a pipeline (the Hash sink
// is the in-Transfer counterpart).
func SHA256(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// nopWriteCloser adapts an io.Writer to a part writer whose Close is a no-op (the sink keeps no
// per-part medium state to finalize).
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// Hash consumes the stream, hashing it, and reports at Commit whether it matches sha (a mismatch is
// the sink-zone error that fails the transfer).
func Hash(sha string) Sink { return &hashSink{sha: sha, h: sha256.New()} }

type hashSink struct {
	sha string
	h   hash.Hash
}

func (s *hashSink) NextPart(_ context.Context) (io.WriteCloser, int64, error) {
	return nopWriteCloser{s.h}, -1, nil
}

func (s *hashSink) Commit(_ context.Context, _ SourceStats) error {
	if got := hex.EncodeToString(s.h.Sum(nil)); got != s.sha {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, s.sha)
	}
	return nil
}

// Drain is a sink that discards the stream (the recoverability proof's "did it decode").
func Drain() Sink { return drainSink{} }

type drainSink struct{}

func (drainSink) NextPart(_ context.Context) (io.WriteCloser, int64, error) {
	return nopWriteCloser{io.Discard}, -1, nil
}
func (drainSink) Commit(_ context.Context, _ SourceStats) error { return nil }

// Writer is a sink that copies the stream to w (a temp file, stdout).
func Writer(w io.Writer) Sink { return writerSink{w: w} }

type writerSink struct{ w io.Writer }

func (s writerSink) NextPart(_ context.Context) (io.WriteCloser, int64, error) {
	return nopWriteCloser{s.w}, -1, nil
}
func (s writerSink) Commit(_ context.Context, _ SourceStats) error { return nil }
