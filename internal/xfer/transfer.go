package xfer

import (
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/programs"
)

// A Transfer moves one byte stream from a Source, through a local Filters chain, to a
// Sink — NBackup's data-movement primitive (Amanda's Xfer). The three zones map onto the
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

// Produced is the raw-stream statistics a producing source reports after the chain drains.
type Produced struct {
	Uncompressed int64
	FileCount    int
	Members      []string
}

// SinkResult is what a sink measured about the bytes that reached it.
type SinkResult struct {
	Compressed int64
	SHA256     string
	Members    []string // e.g. a `tar -t` listing, when the sink lists
}

// Result is a completed transfer's measurements: the producer's raw-stream stats and the
// sink's landed-byte stats.
type Result struct {
	Produced
	SinkResult
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
	// Open begins producing and returns the output stream plus a reap that waits the
	// source's processes (and yields the source-zone error). Cleanup releases scratch.
	Open() (out io.ReadCloser, reap func() error, err error)
	// Produced reports the raw-stream stats once the chain has drained.
	Produced() (Produced, error)
	Cleanup()
}

// Sink consumes the transfer's output stream. progress, if non-nil, is called with the
// running count of bytes that have reached the sink.
type Sink interface {
	Drain(in io.Reader, progress func(compressed int64)) (SinkResult, error)
}

// Filters is the local middle: a chain of programs run on programs.Local(). It carries no
// executor by design — the middle is always the server, never a third remote host.
type Filters struct{ cmds []programs.Cmd }

// NewFilters builds a local filter chain.
func NewFilters(cmds ...programs.Cmd) Filters { return Filters{cmds: cmds} }

// Add appends a filter command, returning a new chain. An identity command (empty Name, e.g.
// codec "none") is dropped: a chain never carries a stage that is not a real program.
func (f Filters) Add(c programs.Cmd) Filters {
	if c.Name == "" {
		return f
	}
	return Filters{cmds: append(f.cmds[:len(f.cmds):len(f.cmds)], c)}
}

// Opts tunes a transfer.
type Opts struct {
	Progress func(compressed int64) // running bytes reaching the sink; nil = no live progress
}

// Transfer runs source → filters(local) → sink as one pipeline, returns the merged Result,
// and on failure a *Error tagged with the faulting zone (upstream cause first).
func Transfer(source Source, filters Filters, sink Sink, opts Opts) (Result, error) {
	out, srcReap, err := source.Open()
	if err != nil {
		source.Cleanup()
		return Result{}, &Error{RoleSource, err}
	}

	mid := out
	filtReap := func() error { return nil }
	filtered := false
	if len(filters.cmds) > 0 {
		fr, fw, ferr := programs.Local().RunPipe(out, filters.cmds...)
		if ferr != nil {
			out.Close()
			_ = srcReap()
			source.Cleanup()
			return Result{}, &Error{RoleFilters, ferr}
		}
		mid, filtReap, filtered = fr, fw, true
	}

	sinkRes, sinkErr := sink.Drain(mid, opts.Progress)

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
	srcErr := srcReap()
	produced, finErr := source.Produced()
	source.Cleanup()
	if srcErr == nil {
		srcErr = srcCloseErr
	}
	if filtErr == nil {
		filtErr = filtCloseErr
	}

	switch {
	case srcErr != nil:
		return Result{}, &Error{RoleSource, srcErr}
	case finErr != nil:
		return Result{}, &Error{RoleSource, finErr}
	case filtErr != nil:
		return Result{}, &Error{RoleFilters, filtErr}
	case sinkErr != nil:
		return Result{}, &Error{RoleSink, sinkErr}
	}
	return Result{Produced: produced, SinkResult: sinkRes}, nil
}

// --- generic sources ---

// Reader is an in-process source over a reader (the medium read, or a test stream). reap,
// when non-nil, waits/closes whatever backs rc and yields the source-zone error; produced
// reports any stats. The zero finish/produced make a plain reader a stat-less source.
func Reader(rc io.ReadCloser) Source { return &readerSource{rc: rc} }

type readerSource struct{ rc io.ReadCloser }

func (s *readerSource) Open() (io.ReadCloser, func() error, error) {
	return s.rc, func() error { return nil }, nil
}
func (s *readerSource) Produced() (Produced, error) { return Produced{}, nil }
func (s *readerSource) Cleanup()                    {}

// Programs is a chain of programs on one executor. As a Source its first command produces
// (tar -c, no stdin); as a Sink it consumes the incoming stream as stdin (tar -x). It
// satisfies both Source and Sink, so it serves whichever end the operation places it in.
type Programs struct {
	exec    programs.Executor
	cmds    []programs.Cmd
	finish  func() (Produced, error) // source: producer totals (e.g. tar --totals)
	cleanup func()
}

// NewPrograms starts a program chain on ex.
func NewPrograms(ex programs.Executor) *Programs { return &Programs{exec: ex} }

// Add appends commands to the chain, dropping identity commands (empty Name) so a "none"
// codec or scheme leaves no stage behind. Variadic so a caller can splice in a placed slice.
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

// Source side.
func (p *Programs) Open() (io.ReadCloser, func() error, error) { return p.exec.RunPipe(nil, p.cmds...) }
func (p *Programs) Produced() (Produced, error) {
	if p.finish != nil {
		return p.finish()
	}
	return Produced{}, nil
}
func (p *Programs) Cleanup() {
	if p.cleanup != nil {
		p.cleanup()
	}
}

// Sink side: feed `in` as stdin to the chain, then drain its (empty, for tar -x) output.
func (p *Programs) Drain(in io.Reader, progress func(int64)) (SinkResult, error) {
	out, wait, err := p.exec.RunPipe(meterReader(in, progress), p.cmds...)
	if err != nil {
		return SinkResult{}, err
	}
	_, copyErr := io.Copy(io.Discard, out) // a program sink (tar -x) writes the fs; drain the rest
	out.Close()
	werr := wait()
	if werr == nil {
		werr = copyErr
	}
	return SinkResult{}, werr
}

// --- generic sinks ---

// Hash drains the stream, hashing it, and reports whether it matches sha (mismatch is an
// error so the transfer fails). It also returns the bytes/hash it saw.
func Hash(sha string) Sink { return hashSink{sha: sha} }

type hashSink struct{ sha string }

func (s hashSink) Drain(in io.Reader, progress func(int64)) (SinkResult, error) {
	got, err := HashReader(meterReader(in, progress))
	if err != nil {
		return SinkResult{}, err
	}
	if got != s.sha {
		return SinkResult{SHA256: got}, fmt.Errorf("checksum mismatch: got %s, want %s", got, s.sha)
	}
	return SinkResult{SHA256: got}, nil
}

// Drain is a sink that discards the stream (the recoverability proof's "did it decode").
func Drain() Sink { return drainSink{} }

type drainSink struct{}

func (drainSink) Drain(in io.Reader, progress func(int64)) (SinkResult, error) {
	_, err := io.Copy(io.Discard, meterReader(in, progress))
	return SinkResult{}, err
}

// Writer is a sink that copies the stream to w (a temp file, stdout).
func Writer(w io.Writer) Sink { return writerSink{w: w} }

type writerSink struct{ w io.Writer }

func (s writerSink) Drain(in io.Reader, progress func(int64)) (SinkResult, error) {
	_, err := io.Copy(s.w, meterReader(in, progress))
	return SinkResult{}, err
}

// meterReader wraps r to report the running byte count to progress (nil = passthrough).
func meterReader(r io.Reader, progress func(int64)) io.Reader {
	if progress == nil {
		return r
	}
	return &progressReader{r: r, f: progress}
}

type progressReader struct {
	r io.Reader
	n int64
	f func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.n += int64(n)
		p.f(p.n)
	}
	return n, err
}
