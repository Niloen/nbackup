package programs

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Params describes how to reach a client over SSH. Secrets are never here: the key
// comes from the operator's ssh config/agent (IdentityFile is a path, not a key),
// consistent with how NBackup handles cloud and gpg credentials.
type Params struct {
	User         string
	Host         string
	Port         string
	IdentityFile string
	Options      []string // extra raw ssh options, e.g. ["-o", "StrictHostKeyChecking=accept-new"]
}

// sshExec runs programs on a remote host via ssh. The remote command is built by
// shell-quoting every token so paths and exclude patterns with spaces or metacharacters
// survive the remote shell. A multi-stage pipeline runs under `bash -o pipefail` so any
// failing stage (notably tar) fails the whole run — a single stage needs no shell.
type sshExec struct{ p Params }

// SSH returns an executor that runs on the host described by p.
func SSH(p Params) Executor { return sshExec{p: p} }

func (s sshExec) Host() string {
	h := "ssh:" + s.target()
	if s.p.Port != "" {
		h += ":" + s.p.Port
	}
	return h
}

func (s sshExec) target() string {
	if s.p.User != "" {
		return s.p.User + "@" + s.p.Host
	}
	return s.p.Host
}

// sshFlags are the connection flags shared by every invocation.
func (s sshExec) sshFlags() []string {
	flags := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if s.p.Port != "" {
		flags = append(flags, "-p", s.p.Port)
	}
	if s.p.IdentityFile != "" {
		flags = append(flags, "-i", s.p.IdentityFile)
	}
	flags = append(flags, s.p.Options...)
	return flags
}

// ssh builds an ssh *exec.Cmd whose single trailing argument is remoteCmd (a string the
// remote login shell parses), bound to ctx: canceling it kills the local ssh process,
// which drops the connection so the remote pipeline (tar et al.) sees EOF/SIGHUP and exits
// — the remote arm of canceling an in-flight dump.
func (s sshExec) ssh(ctx context.Context, remoteCmd string) *exec.Cmd {
	args := append(s.sshFlags(), s.target(), remoteCmd)
	return exec.CommandContext(ctx, "ssh", args...)
}

// Command runs one program on the client. The argv is shell-quoted into a single remote
// command string so the caller's StdoutPipe/Run/Output behave as if local. One-shot probes
// have no cancellation scope of their own, so they run under context.Background.
func (s sshExec) Command(name string, args ...string) *exec.Cmd {
	return s.ssh(context.Background(), shJoin(append([]string{name}, args...)))
}

func (s sshExec) Stat(path string) error    { return s.Command("test", "-e", path).Run() }
func (s sshExec) MkdirAll(dir string) error { return s.Command("mkdir", "-p", dir).Run() }
func (s sshExec) Remove(path string) error  { return s.Command("rm", "-rf", "--", path).Run() }
func (s sshExec) CopyFile(src, dst string) error {
	return s.Command("cp", "--", src, dst).Run()
}

func (s sshExec) Size(path string) (int64, error) {
	out, err := s.Command("stat", "-c", "%s", "--", path).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

func (s sshExec) Rename(oldpath, newpath string) error {
	return s.Command("mv", "-f", "--", oldpath, newpath).Run()
}

// TempFile creates the scratch file with mktemp under the remote $TMPDIR,
// honoring the pattern the way os.CreateTemp does — the last "*" (or, absent
// one, the end of the pattern) becomes the random part — so remote temp files
// are as identifiable as local ones.
func (s sshExec) TempFile(pattern string) (string, error) {
	args := []string{"mktemp"}
	if pattern != "" {
		template := pattern + "XXXXXX"
		if i := strings.LastIndex(pattern, "*"); i >= 0 {
			template = pattern[:i] + "XXXXXX" + pattern[i+1:]
		}
		args = append(args, "-t", template)
	}
	out, err := s.Command(args[0], args[1:]...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (s sshExec) ReadFile(path string) ([]byte, error) {
	return s.Command("cat", "--", path).Output()
}

func (s sshExec) WriteFile(path string, data []byte) error {
	cmd := s.Command("sh", "-c", "cat > "+shQuote(path))
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}

// RunPipe runs the stages as one remote pipeline so intermediate bytes never leave the
// client. A single stage runs bare; two or more run under `bash -o pipefail -c` so a
// failing upstream stage (e.g. tar) is not masked by a succeeding downstream one. Tap is
// not honored (the bytes are inside the remote pipe); the first stage's Stderr receives
// the whole pipeline's standard error (where tar's `--totals` lives).
func (s sshExec) RunPipe(ctx context.Context, stdin io.Reader, progs ...Cmd) (io.ReadCloser, func() error, error) {
	if len(progs) == 0 {
		return io.NopCloser(stdin), func() error { return nil }, nil
	}
	var remote string
	if len(progs) == 1 {
		remote = shJoin(progs[0].argv())
	} else {
		parts := make([]string, len(progs))
		for i, p := range progs {
			parts[i] = shJoin(p.argv())
		}
		remote = shJoin([]string{"bash", "-o", "pipefail", "-c", strings.Join(parts, " | ")})
	}
	cmd := s.ssh(ctx, remote)
	cmd.Stdin = stdin
	stderrW, stderrBuf := captureBuf(firstWithStderr(progs))
	cmd.Stderr = stderrW
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, stageError("ssh", err, "")
	}
	// The remote pipeline reports a single exit code (under pipefail, the rightmost
	// failing stage's). Treat any stage's accepted code as success — in practice this
	// lets tar's exit-1 warning through, the same leniency the local path applies
	// per-stage.
	var okExit []int
	for _, p := range progs {
		okExit = append(okExit, p.OKExit...)
	}
	wait := func() error {
		if err := cmd.Wait(); !isOK(err, okExit) {
			return stageError("ssh "+s.target(), err, stderrBuf.String())
		}
		return nil
	}
	return pipeReader{Reader: stdout, closers: []io.Closer{stdout}}, wait, nil
}

// firstWithStderr returns a Cmd whose Stderr is the first non-nil one among the stages,
// so the remote pipeline's combined stderr is delivered where the caller wants tar's
// `--totals`.
func firstWithStderr(progs []Cmd) Cmd {
	for _, p := range progs {
		if p.Stderr != nil {
			return p
		}
	}
	return Cmd{}
}

// shJoin shell-quotes each token and joins with spaces, producing a string a remote
// shell re-parses into the same argv.
func shJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = shQuote(a)
	}
	return strings.Join(out, " ")
}

// shQuote single-quotes s, escaping embedded single quotes via the '\” idiom, so any
// content (spaces, $, |, nested quotes) is passed literally.
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
