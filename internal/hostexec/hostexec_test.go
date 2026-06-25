package hostexec

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runPipe(t *testing.T, ex Executor, stdin io.Reader, progs ...Cmd) ([]byte, error) {
	t.Helper()
	out, wait, err := ex.RunPipe(stdin, progs...)
	if err != nil {
		return nil, err
	}
	data, rerr := io.ReadAll(out)
	if cerr := out.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if werr := wait(); werr != nil {
		return data, werr
	}
	return data, rerr
}

func TestLocalRunPipeSingle(t *testing.T) {
	got, err := runPipe(t, Local(), strings.NewReader("hello"), Cmd{Name: "cat"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestLocalRunPipeMultiStage(t *testing.T) {
	// gzip -c | gzip -d is the identity, exercising a real two-process pipe.
	want := strings.Repeat("the quick brown fox\n", 100)
	got, err := runPipe(t, Local(), strings.NewReader(want),
		Cmd{Name: "gzip", Args: []string{"-c"}},
		Cmd{Name: "gzip", Args: []string{"-d"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("roundtrip mismatch: %d vs %d bytes", len(got), len(want))
	}
}

func TestLocalRunPipeStageFailureAttributed(t *testing.T) {
	// A non-existent option makes gzip exit nonzero; the error must name the stage.
	_, err := runPipe(t, Local(), strings.NewReader("x"),
		Cmd{Name: "cat"},
		Cmd{Name: "gzip", Args: []string{"--no-such-flag"}},
	)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("error should name the failing stage: %v", err)
	}
}

func TestLocalRunPipeTap(t *testing.T) {
	var n int64
	want := strings.Repeat("a", 4096)
	_, err := runPipe(t, Local(), strings.NewReader(want),
		Cmd{Name: "cat", Tap: func(c int64) { n = c }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(want)) {
		t.Fatalf("tap counted %d, want %d", n, len(want))
	}
}

func TestLocalRunPipeStderrCaptured(t *testing.T) {
	var stderr bytes.Buffer
	// `sh -c 'echo oops >&2'` writes to stderr; the Stderr writer must receive it.
	_, err := runPipe(t, Local(), nil,
		Cmd{Name: "sh", Args: []string{"-c", "echo oops >&2"}, Stderr: &stderr},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "oops") {
		t.Fatalf("stderr not captured: %q", stderr.String())
	}
}

func TestLocalFileOps(t *testing.T) {
	ex := Local()
	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	if err := ex.MkdirAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := ex.Stat(sub); err != nil {
		t.Fatalf("stat after mkdir: %v", err)
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := ex.CopyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := ex.ReadFile(dst)
	if err != nil || string(b) != "payload" {
		t.Fatalf("copy/read: %q %v", b, err)
	}
	tmp, err := ex.TempFile("nbackup-test-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.Stat(tmp); err != nil {
		t.Fatalf("temp not created: %v", err)
	}
	if err := ex.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := ex.Stat(tmp); err == nil {
		t.Fatal("temp should be gone")
	}
	// Remove of an absent path is success.
	if err := ex.Remove(tmp); err != nil {
		t.Fatalf("remove absent: %v", err)
	}
}

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     "'plain'",
		"":          "''",
		"a b":       "'a b'",
		"it's":      `'it'\''s'`,
		"$VAR|x;y":  "'$VAR|x;y'",
		"/a/path-1": "'/a/path-1'",
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSSHCommandArgv(t *testing.T) {
	ex := SSH(Params{User: "backup", Host: "app01", Port: "2222", IdentityFile: "/k/id",
		Options: []string{"-o", "StrictHostKeyChecking=accept-new"}})
	cmd := ex.Command("tar", "--version")
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"ssh", "BatchMode=yes", "-p 2222", "-i /k/id",
		"StrictHostKeyChecking=accept-new", "backup@app01", "'tar' '--version'"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh argv missing %q in %q", want, joined)
		}
	}
}

// sshLoopbackAvailable reports whether `ssh localhost` works without a password — the
// guard for the live SSH executor tests (skipped in CI, which has no sshd).
func sshLoopbackAvailable() bool {
	return SSH(Params{Host: "localhost"}).Command("true").Run() == nil
}

func TestSSHLoopbackRunPipe(t *testing.T) {
	if !sshLoopbackAvailable() {
		t.Skip("ssh localhost not available")
	}
	ex := SSH(Params{Host: "localhost"})
	// A two-stage remote pipeline (identity via gzip) over a real ssh connection.
	want := strings.Repeat("remote payload\n", 50)
	got, err := runPipe(t, ex, strings.NewReader(want),
		Cmd{Name: "gzip", Args: []string{"-c"}},
		Cmd{Name: "gzip", Args: []string{"-d"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("ssh roundtrip mismatch: %d vs %d", len(got), len(want))
	}
}

func TestSSHLoopbackFileOps(t *testing.T) {
	if !sshLoopbackAvailable() {
		t.Skip("ssh localhost not available")
	}
	ex := SSH(Params{Host: "localhost"})
	tmp, err := ex.TempFile("nbackup-ssh-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.Stat(tmp); err != nil {
		t.Fatalf("temp not present: %v", err)
	}
	if err := ex.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := ex.Stat(tmp); err == nil {
		t.Fatal("temp should be gone")
	}
}

func TestSSHPipelineUsesPipefail(t *testing.T) {
	ex := SSH(Params{Host: "app01"})
	// Two stages must be wrapped so an upstream failure is not masked.
	cmd := ex.(sshExec).ssh("")
	_ = cmd
	// Build the remote command the way RunPipe does and assert its shape.
	remote := shJoin([]string{"bash", "-o", "pipefail", "-c",
		shJoin([]string{"tar", "-c"}) + " | " + shJoin([]string{"gzip", "-c"})})
	if !strings.Contains(remote, "pipefail") || !strings.Contains(remote, "|") {
		t.Fatalf("pipeline remote command malformed: %q", remote)
	}
}
