package notify

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// smtpSend is the transport seam: net/smtp.SendMail in production, a recorder in
// tests (so the suite opens no socket).
type smtpSend func(addr string, a smtp.Auth, from string, to []string, msg []byte) error

// smtpNotifier emails an Event via an SMTP relay. Auth is configured only when a
// password env var is set; an open relay sends unauthenticated.
type smtpNotifier struct {
	addr string
	auth smtp.Auth
	from string
	to   []string
	send smtpSend
}

// newSMTP builds an SMTP notifier from its backend config, resolving the password
// from the named environment variable (never from config). A configured-but-unset
// password env var is an error — the channel is skipped with a warning rather than
// silently sending unauthenticated.
func newSMTP(b config.NotifyBackend) (Notifier, error) {
	port := b.Port
	if port == 0 {
		port = 587
	}
	n := &smtpNotifier{
		addr: net.JoinHostPort(b.Host, strconv.Itoa(port)),
		from: b.From,
		to:   b.To,
		send: smtp.SendMail,
	}
	if b.PasswordEnv != "" {
		pw := os.Getenv(b.PasswordEnv)
		if pw == "" {
			return nil, fmt.Errorf("SMTP password env %q is unset or empty", b.PasswordEnv)
		}
		user := b.Username
		if user == "" {
			user = b.From
		}
		n.auth = smtp.PlainAuth("", user, pw, b.Host)
	}
	return n, nil
}

func (n *smtpNotifier) Notify(ctx context.Context, ev Event) error {
	msg := smtpMessage(n.from, n.to, ev.Subject, ev.Body)
	// net/smtp.SendMail is blocking and context-unaware; run it off-goroutine so the
	// dispatch timeout can abandon a hung relay (the send goroutine is left to finish
	// or fail on its own — it never blocks the caller past the deadline).
	done := make(chan error, 1)
	go func() { done <- n.send(n.addr, n.auth, n.from, n.to, msg) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// smtpMessage assembles a minimal RFC 5322 plaintext message.
func smtpMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}
