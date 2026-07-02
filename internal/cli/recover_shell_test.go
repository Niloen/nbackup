package cli

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
)

// newShellFixture builds a real engine over a disk medium with one committed run
// (a single file f.txt), the smallest world the recover shell can browse.
func newShellFixture(t *testing.T) (eng *engine.Engine, dle string, runID string) {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("recover me"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	return eng, "localhost:" + src, s.ID
}

// runShellScript feeds a scripted session to the recover shell (stdin is not a
// terminal in tests, so the shell runs in its piped mode) and returns everything
// it printed. It fails the test if the shell errors.
func runShellScript(t *testing.T, eng *engine.Engine, dle, script string) string {
	t.Helper()
	oldIn := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(script))
	defer func() { stdinReader = oldIn }()

	oldOut := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	outCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		outCh <- string(b)
	}()
	shErr := runRecoverShell(eng, dle, "", "", "")
	w.Close()
	os.Stdout = oldOut
	out := <-outCh
	if shErr != nil {
		t.Fatalf("shell: %v\noutput:\n%s", shErr, out)
	}
	return out
}

// TestRecoverShellExtractNoDestPipedErrors: with piped (non-tty) input, `extract`
// with no destination set must error instead of prompting — a prompt would
// swallow the next command as the destination (the literal "quit" became ./quit).
func TestRecoverShellExtractNoDestPipedErrors(t *testing.T) {
	eng, dle, _ := newShellFixture(t)
	// Run from a scratch cwd: the pre-fix behavior extracted into ./quit, and the
	// assertion below must not be fooled by leftovers in the package directory.
	t.Chdir(t.TempDir())
	script := "setdisk " + dle + "\nadd f.txt\nextract\nquit\n"
	out := runShellScript(t, eng, "", script)

	if !strings.Contains(out, "no destination set (use 'dest <dir>' or 'extract <dir>')") {
		t.Fatalf("want a no-destination error, got:\n%s", out)
	}
	if strings.Contains(out, "destination directory:") {
		t.Fatalf("piped extract must not prompt for a destination:\n%s", out)
	}
	// The old behavior consumed "quit" as the destination and extracted into ./quit.
	if _, err := os.Stat("quit"); !os.IsNotExist(err) {
		t.Fatalf("a directory named %q was created (stat err=%v) — the prompt swallowed the next command", "quit", err)
	}
	if strings.Contains(out, "recovered ") {
		t.Fatalf("nothing should have been extracted:\n%s", out)
	}
}

// TestRecoverShellExitsOnEOF: a script that ends without `quit` must end the
// shell cleanly (EOF), not hang or error.
func TestRecoverShellExitsOnEOF(t *testing.T) {
	eng, dle, _ := newShellFixture(t)
	oldIn := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader("pwd\n")) // no quit: ends at EOF
	defer func() { stdinReader = oldIn }()
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	go io.Copy(io.Discard, r)
	defer func() { w.Close(); os.Stdout = oldOut }()

	errCh := make(chan error, 1)
	go func() { errCh <- runRecoverShell(eng, dle, "", "", "") }()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("shell errored on EOF: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("shell did not exit on EOF")
	}
}

// TestRecoverShellSetdateNoBackupKeepsState: setdate to a date with no backup
// must say what state it kept, not just note the miss and silently stay put.
func TestRecoverShellSetdateNoBackupKeepsState(t *testing.T) {
	eng, dle, runID := newShellFixture(t)
	out := runShellScript(t, eng, dle, "setdate 2026-01-01\nquit\n")
	if !strings.Contains(out, "no backup") {
		t.Fatalf("want the no-backup note, got:\n%s", out)
	}
	if !strings.Contains(out, "— keeping ") || !strings.Contains(out, "("+runID+")") {
		t.Fatalf("the note must restate the kept as-of state (run %s):\n%s", runID, out)
	}
}

// TestRecoverShellSetdiskTrailingSlash: a tab-completed "host:/path/" must match
// the catalog's "host:/path" — in the shell's setdisk and (through the same
// ResolveDLE) the --dle flag.
func TestRecoverShellSetdiskTrailingSlash(t *testing.T) {
	eng, dle, _ := newShellFixture(t)
	out := runShellScript(t, eng, "", "setdisk "+dle+"/\nquit\n")
	if strings.Contains(out, "unknown disk") {
		t.Fatalf("trailing slash must not fail DLE matching:\n%s", out)
	}
	if !strings.Contains(out, "disk \"") {
		t.Fatalf("expected the disk banner after setdisk:\n%s", out)
	}
}
