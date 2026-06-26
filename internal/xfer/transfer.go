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

// Sink consumes the transfer's output stream. A sink that measures something the caller
// needs (a checksum, a member listing, a live byte count) does its own metering and hands
// it back through its own concrete type — the transfer carries no progress channel, since
// the byte count any sink would report is one it already computes for its own purposes.
type Sink interface {
	Drain(in io.Reader) error
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

// Transfer runs source → filters(local) → sink as one pipeline, returns the producer's
// raw-stream stats, and on failure a *Error tagged with the faulting zone (upstream cause
// first).
func Transfer(source Source, filters Filters, sink Sink) (Produced, error) {
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

	sinkErr := sink.Drain(mid)

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

	switch {
	case finErr != nil:
		return Produced{}, &Error{RoleSource, finErr}
	case filtErr != nil:
		return Produced{}, &Error{RoleFilters, filtErr}
	case sinkErr != nil:
		return Produced{}, &Error{RoleSink, sinkErr}
	}
	return produced, nil
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

// Sink side: feed `in` as stdin to the chain, then drain its (empty, for tar -x) output.
func (p *Programs) Drain(in io.Reader) error {
	out, wait, err := p.exec.RunPipe(in, p.cmds...)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(io.Discard, out) // a program sink (tar -x) writes the fs; drain the rest
	out.Close()
	werr := wait()
	if werr == nil {
		werr = copyErr
	}
	return werr
}

// --- generic sinks ---

// Hash drains the stream, hashing it, and reports whether it matches sha (mismatch is an
// error so the transfer fails).
func Hash(sha string) Sink { return hashSink{sha: sha} }

type hashSink struct{ sha string }

func (s hashSink) Drain(in io.Reader) error {
	got, err := HashReader(in)
	if err != nil {
		return err
	}
	if got != s.sha {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, s.sha)
	}
	return nil
}

// Drain is a sink that discards the stream (the recoverability proof's "did it decode").
func Drain() Sink { return drainSink{} }

type drainSink struct{}

func (drainSink) Drain(in io.Reader) error {
	_, err := io.Copy(io.Discard, in)
	return err
}

// Writer is a sink that copies the stream to w (a temp file, stdout).
func Writer(w io.Writer) Sink { return writerSink{w: w} }

type writerSink struct{ w io.Writer }

func (s writerSink) Drain(in io.Reader) error {
	_, err := io.Copy(s.w, in)
	return err
}
