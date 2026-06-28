package notify

import (
	"context"
	"fmt"
	"html"
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

// messageBoundary separates the MIME parts. It is static — the report body is
// machine-rendered columns and a label, never this literal — so the message stays
// deterministic (and Date is the only varying header).
const messageBoundary = "nbackup-alt-boundary-b9c1f0a3"

// smtpMessage assembles an RFC 5322 multipart/alternative message carrying the
// report body twice: once as text/plain (for plaintext-only clients) and once as
// text/html wrapping it in <pre>. Webmail clients render text/plain in a
// proportional font, which collapses the report's column alignment; the HTML
// alternative renders monospace so the columns line up again.
func smtpMessage(from string, to []string, subject, body string) []byte {
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", messageBoundary)
	b.WriteString("\r\n")

	// Plaintext alternative: the report verbatim.
	fmt.Fprintf(&b, "--%s\r\n", messageBoundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(crlf(body))
	b.WriteString("\r\n")

	// HTML alternative: the same text in a monospace <pre>, HTML-escaped so any
	// <, >, & in the report can't break the markup.
	fmt.Fprintf(&b, "--%s\r\n", messageBoundary)
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString(`<pre style="font:13px/1.4 monospace;white-space:pre">`)
	b.WriteString(crlf(html.EscapeString(body)))
	b.WriteString("</pre>\r\n")

	fmt.Fprintf(&b, "--%s--\r\n", messageBoundary)
	return []byte(b.String())
}
