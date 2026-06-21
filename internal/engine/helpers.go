package engine

import (
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/dle"
	"github.com/Niloen/nbackup/internal/xfer"
)

// newSink wraps a destination writer with the standard zstd+checksum filter.
func newSink(dst io.Writer) (*xfer.Sink, error) { return xfer.NewZstdSink(dst) }

// newSource wraps a compressed reader for decompression.
func newSource(src io.Reader) (io.ReadCloser, error) { return xfer.NewZstdSource(src) }

// hashReader returns the hex sha256 of everything read from r.
func hashReader(r io.Reader) (string, error) { return xfer.HashReader(r) }

// findDLE returns the configured DLE with the given name, or a bare DLE if not
// found (e.g. restoring from a slot whose source is no longer configured).
func (e *Engine) findDLE(name string) dle.DLE {
	for _, d := range e.cfg.DLEs() {
		if d.Name() == name {
			return d
		}
	}
	return dle.DLE{}
}

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
