package xfer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"syscall"

	"github.com/Niloen/nbackup/internal/programs"
)

// isBrokenPipe reports whether err is a SIGPIPE/EPIPE — the symptom a producer shows
// when its consumer stops reading and closes the pipe (e.g. tar killed by SIGPIPE
// because the medium sink failed first). It is a downstream symptom, never a root cause.
func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EPIPE) {
		return true
	}
	return strings.Contains(err.Error(), "broken pipe") // covers "signal: broken pipe"
}

// A Transfer moves one byte stream from a Source, through a local Filters chain, to a
// Sink — NBackup's data-movement primitive. The three zones map onto the
// three hosts that can ever be involved: the source's host (a client's tar), the local
// server (the compress/encrypt or decrypt/decompress filters), and the sink's host (the
// medium, or a target's tar). Filters are pinned to the local server on purpose: a
// transform never runs on a third *remote* host; client-side transforms ride in the
// Source, target-side ones in the Sink.
//
// A fault is tagged with the zone it came from (Source/Filters/Sink), with the upstream
// cause winning — so a decode failure (Filters) is reported as such rather than as the
// "truncated input" symptom tar shows downstream. That role is exactly the Pipeline vs
// Chain/Structural split the drill and verify layers classify on.

// Produced is what a source reports about the raw stream once its producer has finished:
// the totals only the producer knows (uncompressed size, file count, member list). A
// non-producing source (a plain medium read) reports the zero value. It is the transfer's
// single out-of-band channel — a sink that needs to hand something back (e.g. a `tar -t`
// listing) does so through its own concrete type, not through the transfer result.
type Produced struct {
	Uncompressed int64
	FileCount    int
	Members      []string
}

// Role identifies which zone of a transfer faulted, so a caller can classify the failure.
type Role int

const (
	RoleSource Role = iota
	RoleFilters
	RoleSink
)

func (r Role) String() string {
	switch r {
	case RoleSource:
		return "source"
	case RoleFilters:
		return "filters"
	default:
		return "sink"
	}
}

// Error wraps a transfer failure with the zone it came from.
type Error struct {
	Role Role
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %v", e.Role, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// Source produces the transfer's input stream.
type Source interface {
	// Open begins producing and returns the output stream plus a finish that, once the
	// chain has drained, reaps the source's processes and reports their raw-stream stats
	// (one event: "the producer is done, here is what it produced"). Its error is the
	// source-zone error. Cleanup releases scratch.
	Open() (out io.ReadCloser, finish func() (Produced, error), err error)
	Cleanup()
}

// Sink consumes the transfer's output stream by handing out part writers: Transfer calls NextPart for
// a writer (and a byte cap; max < 0 = unbounded), copies up to that cap of the stream into it, closes
// it, and asks for the next until the stream is exhausted — then Commit finalizes against the
// producer's totals. A medium sink rolls volumes between parts; a plain consumer (hash, discard,
// restore tar) returns one unbounded part. NextPart/Commit take the transfer's ctx: canceling it
// aborts the in-flight part (no committed file), which is how a faulted transfer unwinds.
type Sink interface {
	NextPart(ctx context.Context) (io.WriteCloser, int64, error)
	Commit(ctx context.Context, p Produced) error
}

// Filters is the local middle: a chain of programs run on programs.Local(). It carries no
// executor by design — the middle is always the server, never a third remote host.
type Filters struct{ cmds []programs.Cmd }

// NewFilters builds a local filter chain.
func NewFilters(cmds ...programs.Cmd) Filters { return Filters{cmds: cmds} }

// Add appends a filter command, returning a new chain. An identity command (empty Name, e.g.
// scheme "none") is dropped: a chain never carries a stage that is not a real program.
func (f Filters) Add(c programs.Cmd) Filters {
	if c.Name == "" {
		return f
	}
	return Filters{cmds: append(f.cmds[:len(f.cmds):len(f.cmds)], c)}
}

// Transfer runs source → filters(local) → sink as one pipeline, returns the producer's
// raw-stream stats, and on failure a *Error tagged with the faulting zone (upstream cause
// first). ctx threads to the sink's part writers; on a fault Transfer cancels it so the
// in-flight part aborts (no committed file) rather than committing a partial.
func Transfer(ctx context.Context, source Source, filters Filters, sink Sink) (Produced, error) {
	out, finish, err := source.Open()
	if err != nil {
		source.Cleanup()
		return Produced{}, &Error{RoleSource, err}
	}

	mid := out
	filtReap := func() error { return nil }
	filtered := false
	if len(filters.cmds) > 0 {
		fr, fw, ferr := programs.Local().RunPipe(out, filters.cmds...)
		if ferr != nil {
			out.Close()
			_, _ = finish() // reap the source we already started
			source.Cleanup()
			return Produced{}, &Error{RoleFilters, ferr}
		}
		mid, filtReap, filtered = fr, fw, true
	}

	actx, cancel := context.WithCancel(ctx)
	sinkErr := drive(actx, cancel, sink, mid) // pull parts, copy the stream in, close each

	// Reap from the consumer back to the producer, closing each reader to push EOF/SIGPIPE
	// upstream. A reader's Close surfaces a media/process fault (e.g. an unreadable part on
	// the source), so its error is folded into that reader's zone. The upstream cause wins:
	// a source or filter that died makes the sink see truncated input, so we surface the
	// source/filter error over the sink's symptom.
	var srcCloseErr, filtCloseErr, filtErr error
	if filtered {
		filtCloseErr = mid.Close() // the filter chain's output
		// Reap the filters BEFORE closing the source reader: the filters' stdin-copy
		// goroutine is still reading `out`, so closing it first would race that read.
		filtErr = filtReap()
		srcCloseErr = out.Close() // the source's output (a media-read fault lands here)
	} else {
		srcCloseErr = mid.Close() // mid is the source's output; the sink already reaped its procs
	}
	produced, finErr := finish() // reap the source's procs and read their totals
	source.Cleanup()
	if finErr == nil {
		finErr = srcCloseErr // a clean reap still leaves a media-read close fault to surface
	}
	if filtErr == nil {
		filtErr = filtCloseErr
	}

	// When the SINK fails first it stops reading, so the source (and filters) get
	// SIGPIPE/EPIPE writing into the now-closed pipe — a downstream symptom, not the
	// cause. Drop those broken-pipe symptoms so the sink's real error (e.g. "medium is
	// full; load a fresh volume") surfaces instead of a baffling "tar: broken pipe". A
	// genuine, non-pipe source/filter fault (e.g. a media-read error) is not a broken
	// pipe, so it still wins as the upstream cause.
	if sinkErr != nil {
		if isBrokenPipe(finErr) {
			finErr = nil
		}
		if isBrokenPipe(filtErr) {
			filtErr = nil
		}
	}

	switch {
	case finErr != nil:
		cancel() // abort any in-flight part rather than commit a partial
		return Produced{}, &Error{RoleSource, finErr}
	case filtErr != nil:
		cancel()
		return Produced{}, &Error{RoleFilters, filtErr}
	case sinkErr != nil:
		cancel()
		return Produced{}, &Error{RoleSink, sinkErr}
	}

	// The transfer is clean and the source reaped, so produced is final: seal the sink against those
	// totals (writes the footer + records the placement, for a medium sink).
	if err := sink.Commit(actx, produced); err != nil {
		cancel()
		return Produced{}, &Error{RoleSink, err}
	}
	cancel()
	return produced, nil
}

// drive pulls parts from sink and copies the (filtered) stream mid into each until exhausted, closing
// each part to commit it. On a copy/close fault it cancels ctx (so the in-flight part's Close
// discards rather than commits) and returns the fault as the sink-zone error — the reap's role
// precedence then lets a genuine upstream (source/filter) cause win over this symptom.
func drive(ctx context.Context, cancel context.CancelFunc, sink Sink, mid io.Reader) error {
	r := bufio.NewReader(mid)
	for {
		w, max, err := sink.NextPart(ctx)
		if err != nil {
			return err
		}
		eof, copyErr := copyPart(w, r, max)
		if copyErr != nil {
			cancel() // abort the in-flight part: its Close must discard, not commit
			_ = w.Close()
			return copyErr
		}
		if err := w.Close(); err != nil { // commit this part
			return err
		}
		if eof {
			return nil
		}
	}
}

// copyPart copies up to max bytes (all, when max < 0) from r into w, reporting whether the stream is
// exhausted. For a bounded part it peeks one byte past a full part to tell "exactly done" from "more
// to come" without leaving an empty trailing part.
func copyPart(w io.Writer, r *bufio.Reader, max int64) (eof bool, err error) {
	if max < 0 {
		_, e := io.Copy(w, r)
		return true, e
	}
	_, e := io.CopyN(w, r, max)
	if e == io.EOF { // stream ended within this part (n < max)
		return true, nil
	}
	if e != nil {
		return false, e
	}
	if _, pe := r.Peek(1); pe == io.EOF { // filled exactly at the cap and nothing follows
		return true, nil
	} else if pe != nil {
		return false, pe
	}
	return false, nil
}

// --- generic sources ---

// Reader is an in-process source over a reader (the medium read, or a test stream). It has
// no producer of its own, so it reports no stats — a stat-less source; Transfer closes rc.
func Reader(rc io.ReadCloser) Source { return &readerSource{rc: rc} }

type readerSource struct{ rc io.ReadCloser }

func (s *readerSource) Open() (io.ReadCloser, func() (Produced, error), error) {
	return s.rc, func() (Produced, error) { return Produced{}, nil }, nil
}
func (s *readerSource) Cleanup() {}

// Programs is a chain of programs on one executor. As a Source its first command produces
// (tar -c, no stdin); as a Sink it consumes the incoming stream as stdin (tar -x). It
// satisfies both Source and Sink, so it serves whichever end the operation places it in.
type Programs struct {
	exec    programs.Executor
	cmds    []programs.Cmd
	finish  func() (Produced, error) // source: producer totals (e.g. tar --totals)
	cleanup func()

	// sink side (set by NextPart): the chain consumes the part stream as stdin (tar -x).
	wait      func() error
	drainDone chan error
}

// NewPrograms starts a program chain on ex.
func NewPrograms(ex programs.Executor) *Programs { return &Programs{exec: ex} }

// Add appends commands to the chain, dropping identity commands (empty Name) so a "none"
// compress or encrypt scheme leaves no stage behind. Variadic so a caller can splice in a placed slice.
func (p *Programs) Add(cmds ...programs.Cmd) *Programs {
	for _, c := range cmds {
		if c.Name != "" {
			p.cmds = append(p.cmds, c)
		}
	}
	return p
}

// Finishing sets the producer's stat hook (source use).
func (p *Programs) Finishing(fn func() (Produced, error)) *Programs { p.finish = fn; return p }

// OnCleanup sets a scratch-cleanup hook (source use).
func (p *Programs) OnCleanup(fn func()) *Programs { p.cleanup = fn; return p }

// Source side: the chain produces (tar -c, no stdin). finish waits the chain's processes
// and then reads the producer's totals — one event, since the totals are only readable once
// the producer has exited; a reap failure wins over the totals it would have reported.
func (p *Programs) Open() (io.ReadCloser, func() (Produced, error), error) {
	out, wait, err := p.exec.RunPipe(nil, p.cmds...)
	if err != nil {
		return nil, nil, err
	}
	finish := func() (Produced, error) {
		if werr := wait(); werr != nil {
			return Produced{}, werr
		}
		if p.finish != nil {
			return p.finish()
		}
		return Produced{}, nil
	}
	return out, finish, nil
}
func (p *Programs) Cleanup() {
	if p.cleanup != nil {
		p.cleanup()
	}
}

// Sink side: a program sink (tar -x) consumes the whole stream as stdin — one unbounded part.
// NextPart starts the chain over a pipe (the returned writer is its stdin) and drains the chain's
// (empty, for tar -x) output on a goroutine; Commit waits for the chain and the drain. It writes the
// filesystem, not a stored archive, so there is no footer/placement to seal.
func (p *Programs) NextPart(_ context.Context) (io.WriteCloser, int64, error) {
	pr, pw := io.Pipe()
	out, wait, err := p.exec.RunPipe(pr, p.cmds...)
	if err != nil {
		pr.Close()
		pw.Close()
		return nil, 0, err
	}
	p.wait = wait
	p.drainDone = make(chan error, 1)
	go func() {
		_, e := io.Copy(io.Discard, out)
		out.Close()
		p.drainDone <- e
	}()
	return pw, -1, nil
}

func (p *Programs) Commit(_ context.Context, _ Produced) error {
	werr := p.wait()
	derr := <-p.drainDone
	if werr == nil {
		werr = derr
	}
	return werr
}

// --- generic sinks ---

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

func (s *hashSink) Commit(_ context.Context, _ Produced) error {
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
func (drainSink) Commit(_ context.Context, _ Produced) error { return nil }

// Writer is a sink that copies the stream to w (a temp file, stdout).
func Writer(w io.Writer) Sink { return writerSink{w: w} }

type writerSink struct{ w io.Writer }

func (s writerSink) NextPart(_ context.Context) (io.WriteCloser, int64, error) {
	return nopWriteCloser{s.w}, -1, nil
}
func (s writerSink) Commit(_ context.Context, _ Produced) error { return nil }
