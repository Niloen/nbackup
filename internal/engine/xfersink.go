package engine

import (
	"io"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/slotio"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
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

// listSink consumes the decoded stream by listing its members (`tar -t`) — the verify
// path's structural check. A bad stream (truncated decode, not-a-tar) fails the archiver's
// List; the members feed the seal comparison.
type listSink struct{ arch archiver.Archiver }

func (s listSink) Drain(in io.Reader, _ func(int64)) (xfer.SinkResult, error) {
	members, err := s.arch.List(in)
	return xfer.SinkResult{Members: members}, err
}

// decodeFilters returns the local-server decrypt and decompress commands that reverse an
// archive's recorded transforms (server-side decode, keyed by the engine's default
// dcopts). A none scheme yields an empty Cmd, which the transfer skips.
func (e *Engine) decodeFilters(codec, encrypt string) (decrypt, decompress programs.Cmd, err error) {
	cf, err := compress.Filter(codec, e.fopts)
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	ef, err := crypt.Filter(encrypt, e.dcopts)
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	return ef.Reverse, cf.Reverse, nil
}

// localDecode builds a local Filters chain (decrypt then decompress) from decodeFilters'
// commands, skipping the none/identity ones.
func localDecode(decrypt, decompress programs.Cmd) xfer.Filters {
	f := xfer.NewFilters()
	if decrypt.Name != "" {
		f = f.Add(decrypt)
	}
	if decompress.Name != "" {
		f = f.Add(decompress)
	}
	return f
}

// copySink is the xfer.Sink for a copy/sync: it re-splits the source copy's already-
// compressed bytes onto the target's volumes without recompressing, verifying the stream
// against the seal's checksum (slotio.Writer.CopyArchive). It is the raw-passthrough peer
// of mediumSink.
type copySink struct {
	w    *slotio.Writer
	meta record.Archive
}

func (s *copySink) Drain(in io.Reader, _ func(int64)) (xfer.SinkResult, error) {
	_, err := s.w.CopyArchive(s.meta, in)
	return xfer.SinkResult{}, err
}
