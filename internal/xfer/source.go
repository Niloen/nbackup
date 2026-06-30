package xfer

import (
	"context"
	"io"

	"github.com/Niloen/nbackup/internal/programs"
)

// Reader is an in-process source over a reader (the medium read, or a test stream). It has
// no producer of its own, so it reports no stats — a stat-less source; Transfer closes rc.
func Reader(rc io.ReadCloser) Source { return &readerSource{rc: rc} }

type readerSource struct{ rc io.ReadCloser }

func (s *readerSource) Open(context.Context) (io.ReadCloser, func() (SourceStats, error), error) {
	return s.rc, func() (SourceStats, error) { return SourceStats{}, nil }, nil
}
func (s *readerSource) Cleanup() {}

// ProgramChain is a chain of programs on one executor. As a Source its first command produces
// (tar -c, no stdin); as a Sink it consumes the incoming stream as stdin (tar -x). It
// satisfies both Source and Sink, so it serves whichever end the operation places it in.
//
// The struct lives two lives that never overlap: the source-side fields (finish, cleanup) are
// used only when it is opened as a Source, and the sink-side fields (wait, drainDone, set by
// NextPart) only when it is driven as a Sink. A given instance is one or the other, never both.
type ProgramChain struct {
	exec    programs.Executor
	cmds    []programs.Cmd
	finish  func() (SourceStats, error) // source: producer totals (e.g. tar --totals)
	cleanup func()

	// sink side (set by NextPart): the chain consumes the part stream as stdin (tar -x).
	wait      func() error
	drainDone chan error
}

// NewProgramChain starts a program chain on ex.
func NewProgramChain(ex programs.Executor) *ProgramChain { return &ProgramChain{exec: ex} }

// Add appends commands to the chain, dropping identity commands (empty Name) so a "none"
// compress or encrypt scheme leaves no stage behind. Variadic so a caller can splice in a placed slice.
func (p *ProgramChain) Add(cmds ...programs.Cmd) *ProgramChain {
	for _, c := range cmds {
		if c.Name != "" {
			p.cmds = append(p.cmds, c)
		}
	}
	return p
}

// Finishing sets the producer's stat hook (source use).
func (p *ProgramChain) Finishing(fn func() (SourceStats, error)) *ProgramChain {
	p.finish = fn
	return p
}

// OnCleanup sets a scratch-cleanup hook (source use).
func (p *ProgramChain) OnCleanup(fn func()) *ProgramChain { p.cleanup = fn; return p }

// Source side: the chain produces (tar -c, no stdin). finish waits the chain's processes
// and then reads the producer's totals — one event, since the totals are only readable once
// the producer has exited; a reap failure wins over the totals it would have reported.
func (p *ProgramChain) Open(ctx context.Context) (io.ReadCloser, func() (SourceStats, error), error) {
	out, wait, err := p.exec.RunPipe(ctx, nil, p.cmds...)
	if err != nil {
		return nil, nil, err
	}
	finish := func() (SourceStats, error) {
		if werr := wait(); werr != nil {
			return SourceStats{}, werr
		}
		if p.finish != nil {
			return p.finish()
		}
		return SourceStats{}, nil
	}
	return out, finish, nil
}
func (p *ProgramChain) Cleanup() {
	if p.cleanup != nil {
		p.cleanup()
	}
}

// Sink side: a program sink (tar -x) consumes the whole stream as stdin — one unbounded part.
// NextPart starts the chain over a pipe (the returned writer is its stdin) and drains the chain's
// (empty, for tar -x) output on a goroutine; Commit waits for the chain and the drain. It writes the
// filesystem, not a stored archive, so there is no footer/placement to seal.
func (p *ProgramChain) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	pr, pw := io.Pipe()
	out, wait, err := p.exec.RunPipe(ctx, pr, p.cmds...)
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

func (p *ProgramChain) Commit(_ context.Context, _ SourceStats) error {
	werr := p.wait()
	derr := <-p.drainDone
	if werr == nil {
		werr = derr
	}
	return werr
}
