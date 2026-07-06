package notify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Niloen/nbackup/internal/config"
)

// commandRun is the transport seam: exec.CommandContext in production, a recorder
// in tests (so the suite spawns no process from the notify package's own tests;
// the command backend's own test still exercises a real script).
type commandRun func(ctx context.Context, path string, args []string, env []string, stdin []byte) error

// commandNotifier execs an operator-supplied script or binary on a notify event,
// passing event data as environment variables and the rendered body on stdin —
// mirroring how the sendmail backend shells out to a local MTA. The command and
// its args come from config verbatim and are execed directly (no shell), so
// nothing in the event (a DLE path, an error message) is ever interpreted as
// shell syntax.
type commandNotifier struct {
	path string
	args []string
	run  commandRun
}

func newCommand(b config.NotifyBackend) (Notifier, error) {
	return &commandNotifier{path: b.Command, args: b.Args, run: runCommand}, nil
}

func (n *commandNotifier) Notify(ctx context.Context, ev Event) error {
	status := "OK"
	if ev.Failed {
		status = "FAILED"
	}
	env := []string{
		"NB_COMMAND=" + ev.Command,
		"NB_STATUS=" + status,
		"NB_SUBJECT=" + ev.Subject,
	}
	return n.run(ctx, n.path, n.args, env, []byte(ev.Body))
}

// runCommand execs path with args, the process environment extended by env, and
// stdin piped in. exec.CommandContext kills the process if the dispatch deadline
// fires, so a wedged script can't block a run's notifications indefinitely.
func runCommand(ctx context.Context, path string, args []string, env []string, stdin []byte) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(cmd.Environ(), env...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return fmt.Errorf("%s: %v: %s", path, err, s)
		}
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}
