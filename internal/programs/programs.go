// Package programs is NBackup's execution transport: it runs external programs —
// and stages their scratch files — on a host that is transparently either the local
// machine (Local) or a remote one over SSH. It is injected into every external-tool
// stage (archivers, and the compressor/encryptor stages), so "runs here" versus "runs
// on the client" is invisible to them: a new archiver gets remote execution for free as
// long as its binaries are on the client. SSH therefore lives in exactly one place, not
// inside any archiver.
//
// The load-bearing primitive is RunPipe: it runs progs[0] | progs[1] | ... as one
// host-local pipeline, so when a dump's tar+compress+encrypt share one executor the
// intermediate bytes never leave that host (plaintext stays on the client). The xfer
// layer composes these single-host pipelines into a source → filters → sink transfer,
// crossing the wire only between zones; the same model runs in reverse for restore
// (decrypt | decompress | tar).
package programs

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Cmd is one program stage in a pipeline.
type Cmd struct {
	Name string
	Args []string
	Nice int // prepend `nice -n Nice` for CPU politeness when nonzero

	// OKExit lists non-zero exit codes to treat as success (besides 0). GNU tar's
	// exit 1 ("some files changed as we read them") is a warning, not a failure, so the
	// tar stage sets OKExit=[1].
	OKExit []int

	// Tap, when set, is called with the cumulative byte count flowing OUT of this
	// stage. Only the Local executor honors it (the bytes are visible in-process); the
	// SSH executor cannot tap a stage buried inside the remote pipe and ignores it, so
	// callers must tolerate it never being called (progress falls back to the meter).
	Tap func(n int64)

	// Stderr, when set, receives this stage's standard error (e.g. tar's `--totals`
	// line and diagnostics). For the SSH executor a pipeline shares one remote shell, so
	// the whole pipe's stderr is delivered to the first stage that sets Stderr.
	Stderr io.Writer
}

// argv returns the full argument vector, with an optional nice prefix.
func (c Cmd) argv() []string {
	base := append([]string{c.Name}, c.Args...)
	if c.Nice != 0 {
		return append([]string{"nice", "-n", strconv.Itoa(c.Nice)}, base...)
	}
	return base
}

// Executor runs commands and manipulates scratch files on one host (local or remote).
// The filesystem operations exist because a stage's scratch state — a snapshot library,
// an index temp file — lives wherever its program runs.
type Executor interface {
	// Host is a stable identity for the host these commands run on ("local", or
	// "ssh:user@host:port"). The pipeline builder groups adjacent stages by it.
	Host() string

	// RunPipe runs progs[0] | progs[1] | ... on this host. stdin feeds progs[0]
	// (nil = no input); the returned reader is progs[last]'s stdout. wait() blocks for
	// every stage and reports the first failure (with its stderr). The caller must drain
	// (and Close) the reader, then call wait.
	RunPipe(stdin io.Reader, progs ...Cmd) (stdout io.ReadCloser, wait func() error, err error)

	// Command builds a single command on this host (for one-shot probes like
	// `tar --version` or the `/dev/null` estimate). The returned *exec.Cmd already
	// targets the right host (local process, or an ssh invocation), so callers use the
	// familiar StdoutPipe/Run/Output API.
	Command(name string, args ...string) *exec.Cmd

	// Stat returns nil iff path exists on this host.
	Stat(path string) error
	MkdirAll(dir string) error
	// Remove deletes path, treating "not present" as success.
	Remove(path string) error
	CopyFile(src, dst string) error
	// TempFile creates an empty scratch file on this host and returns its path.
	TempFile(pattern string) (string, error)
	ReadFile(path string) ([]byte, error)
}

// pipeResult wraps the final stdout so Close drains/closes it; wait (returned
// separately) reaps the children.
type pipeReader struct {
	io.Reader
	closers []io.Closer
}

func (p pipeReader) Close() error {
	var err error
	for _, c := range p.closers {
		if e := c.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// countReader counts bytes read and reports the running total via f.
type countReader struct {
	r io.Reader
	n int64
	f func(int64)
}

func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 {
		c.n += int64(n)
		c.f(c.n)
	}
	return n, err
}

// Filter is a reversible stream transform: a named scheme with a forward (encode) and
// reverse (decode) child command. A zero Forward/Reverse (Cmd.Name == "") is the
// identity — the "none" scheme — which contributes no stage to a pipeline. It is the
// shared shape compress and crypt expose so the transform layer can chain and reverse
// them uniformly.
type Filter struct {
	Name    string
	Forward Cmd
	Reverse Cmd
}

// isOK reports whether an exit error is success — exit 0, or a code in okExit.
func isOK(err error, okExit []int) bool {
	if err == nil {
		return true
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		return false
	}
	for _, c := range okExit {
		if ee.ExitCode() == c {
			return true
		}
	}
	return false
}

// stageError formats a stage failure with the program name and its captured stderr.
func stageError(name string, err error, stderr string) error {
	if s := strings.TrimSpace(stderr); s != "" {
		return fmt.Errorf("%s: %w\n%s", name, err, s)
	}
	return fmt.Errorf("%s: %w", name, err)
}

// captureBuf returns the writer for a stage's stderr plus the buffer behind it: the
// caller's Stderr when set, otherwise a fresh buffer used for error reporting.
func captureBuf(c Cmd) (io.Writer, *bytes.Buffer) {
	if c.Stderr != nil {
		// Tee into a buffer too so a failure still reports diagnostics.
		buf := &bytes.Buffer{}
		return io.MultiWriter(c.Stderr, buf), buf
	}
	buf := &bytes.Buffer{}
	return buf, buf
}
