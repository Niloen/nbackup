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

// atomic.go is the FRAMED-ATOMIC write path (docs/design/archive-shapes.md): an
// encrypted archive's parts are indivisible sealed atoms — each ONE complete encrypted
// message. The inner Full stages (compress) frame at frame_size exactly as the
// invisible shape does; whole inner frames pack up to the ATOM bound (the dumptype's
// part_size, in the compressed domain — the same place today's splitter cuts, so
// object count is preserved); each bundle is fed to one seal (gpg) child, and each
// child's output is one atom. TransferAtoms is the matching drive mode: one sink part
// per atom, never cut at the sink's cap.

// AtomSource produces its stream as a sequence of indivisible atoms. next returns each
// atom's reader in order — the caller drains it and Closes it (which reaps the atom's
// children) before asking for the next — and (nil, io.EOF) past the last. finish and
// Cleanup keep Source's exact semantics: finish reaps the producer once the stream is
// done and reports its stats (Frames carrying each atom's raw/encrypted start offsets,
// which the writer folds into the per-part seals' RawSize).
type AtomSource interface {
	OpenAtoms(ctx context.Context) (next func() (io.ReadCloser, error), finish func() (SourceStats, error), err error)
	Cleanup()
}

// AtomicSource wraps a producing source with the atomic encode pipeline: innerFilters
// (the Full stages — compress) restart every frameSize of raw input; whole inner
// frames pack into bundles of at most atomBound encoded bytes; seal (the PerFrame
// stage — gpg) seals each bundle as one atom. All encode work is absorbed here, so
// the transfer runs with no filter zone and every encode fault is RoleSource.
func AtomicSource(inner Source, innerFilters Filters, frameSize int64, seal programs.Cmd, atomBound int64) AtomSource {
	return &atomicSource{inner: inner, cmds: innerFilters.cmds, seal: seal, frameSize: frameSize, atomBound: atomBound}
}

type atomicSource struct {
	inner     Source
	cmds      []programs.Cmd // inner Full stages; empty = identity (compress none)
	seal      programs.Cmd
	frameSize int64
	atomBound int64

	br     *bufio.Reader
	out    io.ReadCloser
	rawN   int64          // raw bytes consumed from the producer
	encN   int64          // sealed (atom-stream) bytes produced
	frames []record.Frame // per-atom start offsets {Raw, Enc}
	err    error          // first encode fault (source zone)
}

// maxInnerFrameEnc is the worst-case encoded size of one inner frame — the packing
// margin that lets the bundle cut BEFORE producing a frame (a frame's exact encoded
// size is only known after its child exits, but the bundle must never exceed the atom
// bound). Compressors expand incompressible input by a hair (gzip stored blocks:
// +5 bytes/32 KiB), so frameSize plus a small proportional margin bounds it.
func (a *atomicSource) maxInnerFrameEnc() int64 {
	return a.frameSize + a.frameSize/64 + 4096
}

func (a *atomicSource) OpenAtoms(ctx context.Context) (func() (io.ReadCloser, error), func() (SourceStats, error), error) {
	out, innerFinish, err := a.inner.Open(ctx)
	if err != nil {
		return nil, nil, err
	}
	a.out = out
	a.br = bufio.NewReader(out)

	next := func() (io.ReadCloser, error) {
		if a.err != nil {
			return nil, a.err
		}
		if _, err := a.br.Peek(1); err == io.EOF {
			a.out.Close() // raw stream drained; push EOF upstream as Transfer's reap would
			return nil, io.EOF
		} else if err != nil {
			a.fail(err)
			return nil, a.err
		}
		return a.openAtom(ctx)
	}
	finish := func() (SourceStats, error) {
		stats, ferr := innerFinish()
		if a.err != nil {
			// The encode chain died; the producer's own reap typically shows only the
			// SIGPIPE symptom of our teardown — the encode fault is the cause.
			return stats, a.err
		}
		if ferr != nil {
			return stats, ferr
		}
		stats.Frames = append([]record.Frame(nil), a.frames...)
		if stats.Uncompressed == 0 {
			// A stat-less inner source (a raw copy) reports no totals; the chunk loop
			// counted the raw bytes itself, and the last atom's RawSize needs the total.
			stats.Uncompressed = a.rawN
		}
		return stats, nil
	}
	return next, finish, nil
}

// openAtom starts one atom: a seal child whose stdin a goroutine feeds with whole
// inner frames up to the atom bound. The returned reader is the child's stdout; its
// Close reaps the feeder and the child, recording any fault as the source-zone error.
func (a *atomicSource) openAtom(ctx context.Context) (io.ReadCloser, error) {
	a.frames = append(a.frames, record.Frame{Raw: a.rawN, Enc: a.encN})
	pr, pw := io.Pipe()
	sealOut, sealWait, err := programs.Local().RunPipe(ctx, pr, a.seal)
	if err != nil {
		a.fail(err)
		return nil, a.err
	}
	feedDone := make(chan error, 1)
	go func() {
		feedDone <- a.feedBundle(ctx, pw)
	}()
	return &atomReader{a: a, rc: sealOut, feedIn: pw, wait: sealWait, feedDone: feedDone}, nil
}

// feedBundle pipes whole inner frames into the seal child's stdin until the next
// frame could exceed the atom bound (or the raw stream ends), then closes it — the
// seal child finishing its message is what ends the atom.
func (a *atomicSource) feedBundle(ctx context.Context, pw *io.PipeWriter) error {
	var bundle int64
	for {
		if bundle > 0 && bundle+a.maxInnerFrameEnc() > a.atomBound {
			break // the next frame might not fit; seal what we have
		}
		if _, err := a.br.Peek(1); err == io.EOF {
			break
		} else if err != nil {
			pw.CloseWithError(err)
			return err
		}
		n, err := a.emitInnerFrame(ctx, pw)
		bundle += n
		if err != nil {
			pw.CloseWithError(err)
			return err
		}
	}
	return pw.Close()
}

// emitInnerFrame produces one inner frame into w: one compress child over the next
// frameSize of raw input — or, for an identity inner pipeline, the raw chunk itself.
// It returns the frame's encoded size.
func (a *atomicSource) emitInnerFrame(ctx context.Context, w io.Writer) (int64, error) {
	chunk := &countingReader{r: io.LimitReader(a.br, a.frameSize), n: &a.rawN}
	if len(a.cmds) == 0 {
		return io.Copy(w, chunk)
	}
	enc, wait, err := programs.Local().RunPipe(ctx, chunk, a.cmds...)
	if err != nil {
		return 0, err
	}
	n, cerr := io.Copy(w, enc)
	enc.Close()
	werr := wait()
	if cerr == nil {
		cerr = werr
	}
	return n, cerr
}

// fail records the first encode fault (naming the atom) and tears the producer down.
func (a *atomicSource) fail(err error) {
	if a.err == nil && !errors.Is(err, io.ErrClosedPipe) {
		a.err = fmt.Errorf("encode atom %d: %w", len(a.frames)-1, err)
	}
	a.out.Close()
}

func (a *atomicSource) Cleanup() { a.inner.Cleanup() }

// atomReader is one atom's output stream: the seal child's stdout, counting the
// atom's encrypted bytes. Close reaps the feeder goroutine and the seal child,
// recording any fault on the source.
type atomReader struct {
	a        *atomicSource
	rc       io.ReadCloser
	feedIn   *io.PipeWriter // the seal child's stdin pipe; aborted on Close to unstick a feeder
	wait     func() error
	feedDone chan error
}

func (r *atomReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	r.a.encN += int64(n)
	return n, err
}

func (r *atomReader) Close() error {
	r.rc.Close()
	// A dead seal child stops consuming its stdin, leaving the feeder blocked mid-
	// write; abort the pipe so it unsticks (a no-op after the feeder's own clean
	// Close — CloseWithError never overwrites a previous close).
	r.feedIn.CloseWithError(io.ErrClosedPipe)
	ferr := <-r.feedDone
	werr := r.wait()
	// The feeder's closed-pipe symptom is our own teardown; the child's exit status
	// is the real story then.
	if ferr == nil || errors.Is(ferr, io.ErrClosedPipe) {
		ferr = werr
	}
	if ferr != nil {
		r.a.fail(ferr)
		return r.a.err
	}
	return nil
}

// TransferAtoms is the atomic shape's drive mode — Transfer's sibling for an
// AtomSource: each atom becomes exactly ONE sink part (the sink's cap is never
// consulted; an atomic writer places each part whole against the atom bound). The
// fault taxonomy is Transfer's: a producer/encode fault is RoleSource, a part
// write/allocation fault RoleSink, the sink's finalize verdict RoleCommit; the
// upstream cause wins over downstream symptoms.
func TransferAtoms(ctx context.Context, source AtomSource, sink Sink) (SourceStats, error) {
	next, finish, err := source.OpenAtoms(ctx)
	if err != nil {
		source.Cleanup()
		return SourceStats{}, &Error{RoleSource, err}
	}
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sinkErr error
	for sinkErr == nil {
		rc, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break // the source fault is finish's to report (role precedence below)
		}
		w, _, err := sink.NextPart(actx)
		if err != nil {
			rc.Close()
			sinkErr = err
			cancel() // abort the in-flight part: its Close must discard, not commit
			break
		}
		srcErr, wErr := copyAtom(w, rc)
		closeErr := rc.Close() // reaps the atom's children; a fault lands on the source
		if srcErr != nil || closeErr != nil || wErr != nil {
			if wErr != nil {
				sinkErr = wErr
			}
			cancel()
			_ = w.Close()
			break
		}
		if err := w.Close(); err != nil { // commit this part (one atom)
			sinkErr = err
			cancel()
			break
		}
	}
	stats, finErr := finish()
	source.Cleanup()
	// When the sink failed first, the source's teardown symptoms (broken pipes) must
	// not outrank the sink's real error — mirror Transfer's suppression.
	if sinkErr != nil && isBrokenPipe(finErr) {
		finErr = nil
	}
	switch {
	case finErr != nil:
		return SourceStats{}, &Error{RoleSource, finErr}
	case sinkErr != nil:
		return SourceStats{}, &Error{RoleSink, sinkErr}
	}
	if err := sink.Commit(actx, stats); err != nil {
		return SourceStats{}, &Error{RoleCommit, err}
	}
	return stats, nil
}

// copyAtom copies one atom into its part writer, attributing a fault to the side that
// raised it: a read fault is the source's (a dying seal child), a write fault the
// sink's (a full or failing medium).
func copyAtom(w io.Writer, r io.Reader) (srcErr, sinkErr error) {
	buf := make([]byte, 256*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil, werr
			}
		}
		if rerr == io.EOF {
			return nil, nil
		}
		if rerr != nil {
			return rerr, nil
		}
	}
}
