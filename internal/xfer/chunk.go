package xfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
)

// ChunkSource wraps a producing source with the (server-side) encode filter chain,
// respawning the filter children every frameSize bytes of raw input — the write-side
// framing mechanism of the FRAMED-INVISIBLE archive shape (docs/design/archive-shapes.md).
// Each child respawn is a decode-restart point: for Concat=Full schemes (gzip, zstd) the
// concatenated per-chunk outputs decode as ONE stream, byte-identical in meaning to an
// unframed encode, so every whole-stream reader and the stock one-liner are untouched.
// The chunk boundaries are recorded as a Frame table and merged into the producer's
// SourceStats at finish, riding the existing stats channel to the archive's index.
//
// The filters are ABSORBED into the source: pass empty Filters to Transfer. An encode
// fault therefore surfaces as RoleSource (not RoleFilters); the failing program is still
// named by the programs layer, and the frame it died in is added here.
//
// With no real filter stages (an all-none pipeline) or a non-positive frameSize there is
// nothing to restart — the inner source is returned unchanged and no frames are recorded
// (raw and encoded offsets coincide; readers treat that identity case directly).
func ChunkSource(inner Source, filters Filters, frameSize int64) Source {
	if len(filters.cmds) == 0 || frameSize <= 0 {
		return inner
	}
	return &chunkSource{inner: inner, cmds: filters.cmds, frameSize: frameSize}
}

type chunkSource struct {
	inner     Source
	cmds      []programs.Cmd
	frameSize int64

	frames []record.Frame
	err    error         // the chunk loop's fault (an encode-stage death); nil on a clean drain
	done   chan struct{} // closed when the chunk loop has exited (frames + err final)
}

func (c *chunkSource) Open(ctx context.Context) (io.ReadCloser, func() (SourceStats, error), error) {
	out, innerFinish, err := c.inner.Open(ctx)
	if err != nil {
		return nil, nil, err
	}
	pr, pw := io.Pipe()
	c.done = make(chan struct{})
	go c.encode(ctx, out, pw)

	finish := func() (SourceStats, error) {
		// The chunk loop owns `out`, c.frames, and c.err; it has exited by the time
		// Transfer calls finish (the stream is drained or the pipe torn down), but wait
		// for it so those are final and the inner reap races nothing.
		<-c.done
		stats, err := innerFinish()
		if c.err != nil {
			// The encode chain died: that is the source-zone cause. Tearing it down
			// closed the producer's pipe, so the producer's own reap typically shows
			// only the SIGPIPE symptom — the encode fault wins either way, keeping the
			// failing encode program named in the RoleSource error.
			return stats, c.err
		}
		if err != nil {
			return stats, err
		}
		stats.Frames = append([]record.Frame(nil), c.frames...)
		return stats, nil
	}
	return pr, finish, nil
}

// encode drives the chunk loop: it slices the raw stream into frameSize chunks and runs
// one filter-chain child pipeline per chunk, concatenating their outputs into pw. Each
// chunk's start is recorded as a Frame before its bytes flow. Any fault tears the pipe
// down with the cause (naming the frame), which the transfer surfaces as RoleSource.
func (c *chunkSource) encode(ctx context.Context, out io.ReadCloser, pw *io.PipeWriter) {
	// The inner source's stream is owned here (Transfer only ever sees the pipe), so
	// close it on every exit: after a clean drain it is already at EOF; on a fault the
	// close pushes SIGPIPE upstream to stop the producer, exactly as Transfer's own
	// reap ordering does for an unwrapped source.
	defer close(c.done)
	defer out.Close()
	fail := func(frame int, err error) {
		// A closed pipe means the DOWNSTREAM side tore the transfer down first (a sink
		// fault: Transfer cancels and closes its reader) — that is not an encode fault,
		// so don't claim the source zone with it; the sink's real error must win.
		if errors.Is(err, io.ErrClosedPipe) {
			pw.CloseWithError(err)
			return
		}
		c.err = frameErr(frame, err)
		pw.CloseWithError(c.err)
	}
	var rawN, encN int64
	br := bufio.NewReader(out)
	for {
		if _, err := br.Peek(1); err == io.EOF {
			break // raw stream exhausted; no empty trailing frame
		} else if err != nil {
			c.err = err
			pw.CloseWithError(err)
			return
		}
		c.frames = append(c.frames, record.Frame{Raw: rawN, Enc: encN})
		chunk := &countingReader{r: io.LimitReader(br, c.frameSize), n: &rawN}
		enc, wait, err := programs.Local().RunPipe(ctx, chunk, c.cmds...)
		if err != nil {
			fail(len(c.frames)-1, err)
			return
		}
		n, cerr := io.Copy(pw, enc)
		encN += n
		enc.Close()
		werr := wait() // reap the chunk's children; their exit is part of this frame
		if cerr == nil {
			cerr = werr
		}
		if cerr != nil {
			fail(len(c.frames)-1, cerr)
			return
		}
	}
	pw.Close()
}

func (c *chunkSource) Cleanup() { c.inner.Cleanup() }

// frameErr names the frame an encode fault happened in; the wrapped error already names
// the failing program (the programs layer's per-stage attribution).
func frameErr(frame int, err error) error {
	return fmt.Errorf("encode frame %d: %w", frame, err)
}

// countingReader counts the bytes a chunk's child pipeline actually consumed, so frame
// boundaries record the exact raw offset the next chunk starts at.
type countingReader struct {
	r io.Reader
	n *int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	*cr.n += int64(n)
	return n, err
}
