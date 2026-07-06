package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/config"
)

// writeRecorderScript writes a script that dumps its NB_* env vars and stdin to
// outFile, so the test can assert on what the command backend actually passed —
// without a shell in the production path (runCommand execs the interpreter
// directly, same as any other configured command).
func writeRecorderScript(t *testing.T, outFile string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "notify.sh")
	body := "#!/bin/sh\n" +
		"{ echo \"NB_COMMAND=$NB_COMMAND\"; echo \"NB_STATUS=$NB_STATUS\"; echo \"NB_SUBJECT=$NB_SUBJECT\"; echo \"---\"; cat; } > " + outFile + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return script
}

func TestCommandNotifyPassesEnvAndStdin(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "out.txt")
	script := writeRecorderScript(t, outFile)

	n, err := newCommand(config.NotifyBackend{Type: "command", Command: script})
	if err != nil {
		t.Fatalf("newCommand: %v", err)
	}
	ev := Event{Subject: "nbackup dump FAILED", Body: "1 archive failed", Command: "dump", Failed: true}
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read script output: %v", err)
	}
	out := string(got)
	for _, want := range []string{
		"NB_COMMAND=dump",
		"NB_STATUS=FAILED",
		"NB_SUBJECT=nbackup dump FAILED",
		"1 archive failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("script output missing %q\n%s", want, out)
		}
	}
}

func TestCommandNotifySuccessStatus(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "out.txt")
	script := writeRecorderScript(t, outFile)

	n, err := newCommand(config.NotifyBackend{Type: "command", Command: script})
	if err != nil {
		t.Fatalf("newCommand: %v", err)
	}
	if err := n.Notify(context.Background(), Event{Command: "dump", Failed: false}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	got, _ := os.ReadFile(outFile)
	if !strings.Contains(string(got), "NB_STATUS=OK") {
		t.Errorf("script output missing NB_STATUS=OK\n%s", got)
	}
}

func TestCommandNotifyNonZeroExitIsError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	n, err := newCommand(config.NotifyBackend{Type: "command", Command: script})
	if err != nil {
		t.Fatalf("newCommand: %v", err)
	}
	err = n.Notify(context.Background(), Event{})
	if err == nil {
		t.Fatalf("expected error from a nonzero exit")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to carry the script's stderr", err)
	}
}

func TestCommandNotifyPassesArgs(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "args.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+outFile+"\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	n, err := newCommand(config.NotifyBackend{Type: "command", Command: script, Args: []string{"--quiet", "run"}})
	if err != nil {
		t.Fatalf("newCommand: %v", err)
	}
	if err := n.Notify(context.Background(), Event{}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	got, _ := os.ReadFile(outFile)
	if strings.TrimSpace(string(got)) != "--quiet run" {
		t.Errorf("args = %q, want %q", strings.TrimSpace(string(got)), "--quiet run")
	}
}
