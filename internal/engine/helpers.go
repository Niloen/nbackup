package engine

import (
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/xfer"
)

// newSink wraps a destination writer with the standard zstd+checksum filter.
func newSink(dst io.Writer) (*xfer.Sink, error) { return xfer.NewZstdSink(dst) }

// newSource wraps a compressed reader for decompression.
func newSource(src io.Reader) (io.ReadCloser, error) { return xfer.NewZstdSource(src) }

// hashReader returns the hex sha256 of everything read from r.
func hashReader(r io.Reader) (string, error) { return xfer.HashReader(r) }

func humanBytes(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}
