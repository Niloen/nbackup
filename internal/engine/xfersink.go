package engine

import (
	"io"

	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/slotio"
	"github.com/Niloen/nbackup/internal/xfer"
)

// mediumSink is the xfer.Sink that lands an archive's bytes onto a slot's volumes via the
// open slotio.Writer: it meters (sha256 + size) and splits the stream into parts, keeping
// the measured archive + part positions for the caller to record once the producer's
// stats are merged in. It is the data path's one slotio-coupled endpoint.
type mediumSink struct {
	w    *slotio.Writer
	meta record.Archive

	arch  record.Archive // measured (Compressed/SHA256/Parts), filled by Drain
	parts []record.FilePos
}

func (m *mediumSink) Drain(in io.Reader, progress func(int64)) (xfer.SinkResult, error) {
	arch, parts, err := m.w.WriteArchive(m.meta, in, progress)
	if err != nil {
		return xfer.SinkResult{}, err
	}
	m.arch, m.parts = arch, parts
	return xfer.SinkResult{Compressed: arch.Compressed, SHA256: arch.SHA256}, nil
}
