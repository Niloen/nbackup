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

// programChain is the command list shared by ProgramSource and ProgramSink: a
// chain of programs on one executor, appended with identity commands dropped.
type programChain struct {
	exec programs.Executor
	cmds []programs.Cmd
}

// add appends commands, dropping identity commands (empty Name) so a "none"
// compress or encrypt scheme leaves no stage behind.
func (p *programChain) add(cmds []programs.Cmd) {
	for _, c := range cmds {
		if c.Name != "" {
			p.cmds = append(p.cmds, c)
		}
	}
}

// ProgramSource is a producing chain of programs on one executor (tar -c and any
// client-fused transforms): its first command runs with no stdin, and the chain's
// stdout is the source stream.
type ProgramSource struct {
	programChain
	finish  func() (SourceStats, error) // producer totals (e.g. tar --totals)
	cleanup func()
}

// NewProgramSource starts a producing program chain on ex.
func NewProgramSource(ex programs.Executor) *ProgramSource {
	return &ProgramSource{programChain: programChain{exec: ex}}
}

// Add appends commands to the chain, dropping identity commands (empty Name).
// Variadic so a caller can splice in a placed slice.
func (p *ProgramSource) Add(cmds ...programs.Cmd) *ProgramSource {
	p.add(cmds)
	return p
}

// Finishing sets the producer's stat hook.
func (p *ProgramSource) Finishing(fn func() (SourceStats, error)) *ProgramSource {
	p.finish = fn
	return p
}

// OnCleanup sets a scratch-cleanup hook.
func (p *ProgramSource) OnCleanup(fn func()) *ProgramSource { p.cleanup = fn; return p }

// Open starts the chain (tar -c, no stdin). The returned finish waits the chain's
// processes and then reads the producer's totals — one event, since the totals are
// only readable once the producer has exited; a reap failure wins over the totals
// it would have reported.
func (p *ProgramSource) Open(ctx context.Context) (io.ReadCloser, func() (SourceStats, error), error) {
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

func (p *ProgramSource) Cleanup() {
	if p.cleanup != nil {
		p.cleanup()
	}
}

// ProgramSink is a consuming chain of programs on one executor (decrypt →
// decompress → tar -x): it consumes the whole stream as stdin — one unbounded
// part. NextPart starts the chain over a pipe (the returned writer is its stdin)
// and drains the chain's (empty, for tar -x) output on a goroutine; Commit waits
// for the chain and the drain. It writes the filesystem, not a stored archive, so
// there is no footer/placement to seal.
type ProgramSink struct {
	programChain
	wait      func() error
	drainDone chan error
}

// NewProgramSink starts a consuming program chain on ex.
func NewProgramSink(ex programs.Executor) *ProgramSink {
	return &ProgramSink{programChain: programChain{exec: ex}}
}

// Add appends commands to the chain, dropping identity commands (empty Name).
// Variadic so a caller can splice in a placed slice.
func (p *ProgramSink) Add(cmds ...programs.Cmd) *ProgramSink {
	p.add(cmds)
	return p
}

func (p *ProgramSink) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
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

func (p *ProgramSink) Commit(_ context.Context, _ SourceStats) error {
	werr := p.wait()
	derr := <-p.drainDone
	if werr == nil {
		werr = derr
	}
	return werr
}
