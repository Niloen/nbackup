package notify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/Niloen/nbackup/internal/config"
)

// defaultSendmailPath is the conventional location of the local MTA's sendmail
// interface — postfix, sendmail, and exim all install a compatible binary here.
const defaultSendmailPath = "/usr/sbin/sendmail"

// sendmailRun is the transport seam: it pipes msg to the sendmail binary in
// production, a recorder in tests (so the suite spawns no process).
type sendmailRun func(ctx context.Context, path string, args []string, msg []byte) error

// sendmailNotifier emails an Event by piping an RFC 5322 message to a local
// sendmail binary (postfix/sendmail/exim). Unlike the smtp backend it needs no
// host, port, auth, or secret — delivery is the local MTA's job.
type sendmailNotifier struct {
	path string
	from string
	to   []string
	run  sendmailRun
}

// newSendmail builds a sendmail notifier from its backend config, defaulting the
// binary path to /usr/sbin/sendmail.
func newSendmail(b config.NotifyBackend) (Notifier, error) {
	path := b.SendmailPath
	if path == "" {
		path = defaultSendmailPath
	}
	return &sendmailNotifier{
		path: path,
		from: b.From,
		to:   b.To,
		run:  runSendmail,
	}, nil
}

func (n *sendmailNotifier) Notify(ctx context.Context, ev Event) error {
	msg := smtpMessage(n.from, n.to, ev.Subject, ev.Body)
	// -i: don't treat a lone "." as end-of-input. -f: set the envelope sender.
	// Recipients are passed explicitly after "--" rather than parsed from headers
	// (-t), so a stray address in the body can never redirect the mail.
	args := []string{"-i", "-f", n.from, "--"}
	args = append(args, n.to...)
	return n.run(ctx, n.path, args, msg)
}

// runSendmail execs the sendmail binary with msg on stdin. exec.CommandContext
// kills the process if the dispatch deadline fires, so a wedged MTA can't block.
func runSendmail(ctx context.Context, path string, args []string, msg []byte) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = bytes.NewReader(msg)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if s := bytes.TrimSpace(stderr.Bytes()); len(s) > 0 {
			return fmt.Errorf("%s: %v: %s", path, err, s)
		}
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}
