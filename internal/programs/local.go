package programs

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
)

// localExec runs everything on the local machine — the default and the behavior NBackup
// had before remote sources: os/exec children, os.* scratch files.
type localExec struct{}

// Local returns the executor for the local machine.
func Local() Executor { return localExec{} }

func (localExec) Host() string { return "local" }

func (localExec) Command(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

func (localExec) Stat(path string) error {
	_, err := os.Stat(path)
	return err
}

func (localExec) Size(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (localExec) MkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func (localExec) Remove(path string) error { return os.RemoveAll(path) }

func (localExec) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

func (localExec) WriteFile(path string, data []byte) error { return os.WriteFile(path, data, 0o644) }

func (localExec) CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (localExec) TempFile(pattern string) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	return name, f.Close()
}

func (localExec) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// RunPipe chains the stages with in-process os/exec pipes: each stage's stdout feeds the
// next stage's stdin, optionally tapped for a byte counter. Zero stages is the identity:
// it returns stdin unchanged.
func (localExec) RunPipe(ctx context.Context, stdin io.Reader, progs ...Cmd) (io.ReadCloser, func() error, error) {
	if len(progs) == 0 {
		return io.NopCloser(stdin), func() error { return nil }, nil
	}
	cmds := make([]*exec.Cmd, len(progs))
	bufs := make([]*bufferedStderr, len(progs))
	cur := stdin
	for i, p := range progs {
		argv := p.argv()
		c := exec.CommandContext(ctx, argv[0], argv[1:]...)
		c.Stdin = cur
		w, buf := captureBuf(p)
		c.Stderr = w
		bufs[i] = &bufferedStderr{name: p.Name, buf: buf}
		if i < len(progs)-1 {
			out, err := c.StdoutPipe()
			if err != nil {
				return nil, nil, err
			}
			var r io.Reader = out
			if p.Tap != nil {
				r = &countReader{r: out, f: p.Tap}
			}
			cur = r
		}
		cmds[i] = c
	}
	last := cmds[len(cmds)-1]
	finalOut, err := last.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	var final io.Reader = finalOut
	if tap := progs[len(progs)-1].Tap; tap != nil {
		final = &countReader{r: finalOut, f: tap}
	}
	for _, c := range cmds {
		if err := c.Start(); err != nil {
			// Kill any already-started children so this does not leak.
			for _, k := range cmds {
				if k.Process != nil {
					_ = k.Process.Kill()
				}
			}
			return nil, nil, stageError(c.Path, err, "")
		}
	}
	wait := func() error {
		var firstErr error
		for i, c := range cmds {
			if e := c.Wait(); !isOK(e, progs[i].OKExit) && firstErr == nil {
				firstErr = stageError(bufs[i].name, e, bufs[i].buf.String())
			}
		}
		return firstErr
	}
	return pipeReader{Reader: final, closers: []io.Closer{finalOut}}, wait, nil
}

type bufferedStderr struct {
	name string
	buf  *bytes.Buffer
}
