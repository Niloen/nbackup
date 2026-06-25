// Package streamproc runs a stream transform as an external child process: bytes
// read out (ReadThrough) flow through the child's stdin->stdout. It is the shared
// plumbing behind package filter (decompressors) and package crypt (decryptors),
// which differ only in the argv they build and the registry they expose — Amanda
// drives its compress and encrypt programs the same way. A nil argv is the identity
// transform (the `none` codec/scheme), so callers don't special-case it. The write
// side is driven separately as a pipeline stage via hostexec (see filter.CompressCmd
// / crypt.EncryptCmd). The package also hosts the registry helpers shared by both:
// ProgramOr and SortedNames.
package streamproc

import (
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// ProgramOr returns program when it is non-empty, else def. It backs the codec /
// scheme "override the default binary" Option shared by filter and crypt.
func ProgramOr(program, def string) string {
	if program != "" {
		return program
	}
	return def
}

// SortedNames returns the keys of a registry map sorted, for stable "known: …"
// error messages. Shared by the filter codec and crypt scheme registries.
func SortedNames[V any](registry map[string]V) []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Command builds the exec.Cmd for argv, prefixing `nice -n N` for CPU politeness
// when nice != 0.
func Command(argv []string, nice int) *exec.Cmd {
	if nice != 0 {
		argv = append([]string{"nice", "-n", strconv.Itoa(nice)}, argv...)
	}
	return exec.Command(argv[0], argv[1:]...)
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
