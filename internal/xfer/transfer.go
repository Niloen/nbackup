package xfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"

	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
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

// SourceStats is what a source reports about the raw stream once its producer has
// finished: the totals only the producer knows (uncompressed size, file count, member
// list). A non-producing source (a plain medium read) reports the zero value. It is the
// transfer's single out-of-band channel — a sink that needs to hand something back (e.g.
// a `tar -t` listing) does so through its own concrete type, not through the transfer
// result.
type SourceStats struct {
	Uncompressed int64
	FileCount    int
	Members      []record.Member
	Frames       []record.Frame // a framed source's decode-restart table (ChunkSource); nil = plain stream
	Units        []record.Unit  // the producer's content inventory (see record.Unit); nil = none reported
	Unreadable   []string       // source paths the producer could not read (a partial dump); empty = complete
}

// Role identifies which zone of a transfer faulted, so a caller can classify the failure.
type Role int

const (
	RoleSource Role = iota
	RoleFilters
	RoleSink
	// RoleCommit is the sink's finalize-time verdict (Commit) — distinct from RoleSink's
	// mid-copy fault. For a sink whose NextPart writer cannot itself fail (e.g. Hash, whose
	// hash.Hash.Write never errors), every mid-copy RoleSink is necessarily an upstream read
	// fault that surfaced while draining into that writer, while a RoleCommit is the sink's
	// own judgment (a checksum mismatch, a tar child's exit status) — a caller that needs to
	// tell "something failed feeding the sink" from "the sink pronounced a verdict" (like
	// VerifyChecksum) checks this Role specifically.
	RoleCommit
)

func (r Role) String() string {
	switch r {
	case RoleSource:
		return "source"
	case RoleFilters:
		return "filters"
	case RoleCommit:
		return "commit"
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
	// source-zone error. Cleanup releases scratch. ctx binds the producer's processes:
	// canceling it kills them, which is how a canceled run stops an in-flight dump.
	Open(ctx context.Context) (out io.ReadCloser, finish func() (SourceStats, error), err error)
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
	Commit(ctx context.Context, s SourceStats) error
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
func Transfer(ctx context.Context, source Source, filters Filters, sink Sink) (SourceStats, error) {
	out, finish, err := source.Open(ctx)
	if err != nil {
		source.Cleanup()
		return SourceStats{}, &Error{RoleSource, err}
	}

	mid := out
	filtReap := func() error { return nil }
	filtered := false
	if len(filters.cmds) > 0 {
		fr, fw, ferr := programs.Local().RunPipe(ctx, out, filters.cmds...)
		if ferr != nil {
			out.Close()
			_, _ = finish() // reap the source we already started
			source.Cleanup()
			return SourceStats{}, &Error{RoleFilters, ferr}
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
	stats, finErr := finish() // reap the source's procs and read their totals
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
		return SourceStats{}, &Error{RoleSource, finErr}
	case filtErr != nil:
		cancel()
		return SourceStats{}, &Error{RoleFilters, filtErr}
	case sinkErr != nil:
		cancel()
		return SourceStats{}, &Error{RoleSink, sinkErr}
	}

	// The transfer is clean and the source reaped, so stats are final: seal the sink against those
	// totals (writes the footer + records the placement, for a medium sink).
	if err := sink.Commit(actx, stats); err != nil {
		cancel()
		return SourceStats{}, &Error{RoleCommit, err}
	}
	cancel()
	return stats, nil
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
