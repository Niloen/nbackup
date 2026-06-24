// Package streamproc runs a stream transform as an external child process: bytes
// written in (WriteThrough) or read out (ReadThrough) flow through the child's
// stdin->stdout. It is the shared plumbing behind package filter (compressors) and
// package crypt (encryptors), which differ only in the argv they build and the
// registry they expose — Amanda drives its compress and encrypt programs the same
// way. A nil argv is the identity transform (the `none` codec/scheme), so callers
// don't special-case it.
package streamproc

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Command builds the exec.Cmd for argv, prefixing `nice -n N` for CPU politeness
// when nice != 0.
func Command(argv []string, nice int) *exec.Cmd {
	if nice != 0 {
		argv = append([]string{"nice", "-n", strconv.Itoa(nice)}, argv...)
	}
	return exec.Command(argv[0], argv[1:]...)
}

// WriteThrough returns a WriteCloser that pipes everything written to it through the
// child built from argv and on to dst; Close finishes and waits the child. A nil argv
// is the identity transform: dst is returned with a no-op Close.
func WriteThrough(argv []string, nice int, dst io.Writer) (io.WriteCloser, error) {
	if argv == nil {
		return nopWriteCloser{dst}, nil
	}
	cmd := Command(argv, nice)
	cmd.Stdout = dst
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	return &procWriter{cmd: cmd, stdin: stdin, stderr: &stderr}, nil
}

// ReadThrough returns a ReadCloser that yields src piped through the child built from
// argv; Close waits the child. A nil argv is the identity transform: src is returned
// with a no-op Close.
func ReadThrough(argv []string, nice int, src io.Reader) (io.ReadCloser, error) {
	if argv == nil {
		return io.NopCloser(src), nil
	}
	cmd := Command(argv, nice)
	cmd.Stdin = src
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	return &procReader{cmd: cmd, stdout: stdout, stderr: &stderr}, nil
}

type procWriter struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *strings.Builder
}

func (p *procWriter) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (p *procWriter) Close() error {
	stdinErr := p.stdin.Close()
	waitErr := p.cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("%s: %w\n%s", p.cmd.Path, waitErr, strings.TrimSpace(p.stderr.String()))
	}
	return stdinErr
}

type procReader struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *strings.Builder
}

func (p *procReader) Read(b []byte) (int, error) { return p.stdout.Read(b) }

func (p *procReader) Close() error {
	p.stdout.Close()
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("%s: %w\n%s", p.cmd.Path, err, strings.TrimSpace(p.stderr.String()))
	}
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
