package notify

import (
	"context"
	"io"
	"net/http"
	"net/smtp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/report"
)

func failRun() report.Run {
	return report.Run{Command: report.CommandDump, Outcome: report.OutcomeFailure, ExitClass: "dump-failed", Error: "boom",
		StartedAt: time.Now(), EndedAt: time.Now()}
}

func TestRouteFor(t *testing.T) {
	all := config.NotifyConfig{Backends: map[string]config.NotifyBackend{"mail": {Type: "smtp"}, "chat": {Type: "webhook"}}}
	run := func(cmd report.Command, out report.Outcome) report.Run {
		return report.Run{Command: cmd, Outcome: out}
	}

	// Failure with no on_failure → every backend (failures must be loud).
	got := routeFor(all, run(report.CommandDump, report.OutcomeFailure))
	sort.Strings(got)
	if strings.Join(got, ",") != "chat,mail" {
		t.Errorf("failure routing = %v, want all backends", got)
	}
	// A successful dump notifies by default (the nightly "backups happened" signal).
	got = routeFor(all, run(report.CommandDump, report.OutcomeSuccess))
	sort.Strings(got)
	if strings.Join(got, ",") != "chat,mail" {
		t.Errorf("dump success routing = %v, want all backends by default", got)
	}
	// Other commands' success is opt-in: silent with no on_success.
	if got := routeFor(all, run(report.CommandSync, report.OutcomeSuccess)); len(got) != 0 {
		t.Errorf("sync success routing = %v, want none by default", got)
	}
	if got := routeFor(all, run(report.CommandDrill, report.OutcomeSuccess)); len(got) != 0 {
		t.Errorf("drill success routing = %v, want none by default", got)
	}

	// An explicit on_success overrides the default and applies to every command.
	withSuccess := all
	withSuccess.OnSuccess = []string{"mail"}
	if got := routeFor(withSuccess, run(report.CommandDump, report.OutcomeSuccess)); len(got) != 1 || got[0] != "mail" {
		t.Errorf("explicit dump success routing = %v, want [mail]", got)
	}
	if got := routeFor(withSuccess, run(report.CommandSync, report.OutcomeSuccess)); len(got) != 1 || got[0] != "mail" {
		t.Errorf("explicit sync success routing = %v, want [mail]", got)
	}

	// Explicit on_failure is honored.
	withFailure := all
	withFailure.OnFailure = []string{"chat"}
	if got := routeFor(withFailure, run(report.CommandDump, report.OutcomeFailure)); len(got) != 1 || got[0] != "chat" {
		t.Errorf("explicit failure routing = %v, want [chat]", got)
	}

	// A warned run (success + warnings) routes like a failure: it owes the
	// operator an action, so it must reach the loud channels.
	warned := run(report.CommandSync, report.OutcomeSuccess)
	warned.Warnings = []string{"landing c2 tripped mid-run"}
	got = routeFor(all, warned)
	sort.Strings(got)
	if strings.Join(got, ",") != "chat,mail" {
		t.Errorf("warned routing = %v, want all backends", got)
	}
	if got := routeFor(withFailure, warned); len(got) != 1 || got[0] != "chat" {
		t.Errorf("warned routing with on_failure = %v, want [chat]", got)
	}
}

// TestBuildEventSubject: the subject carries the three-state verdict and the run's
// date (Amanda's convention) — a dated subject per night, so mail clients never
// thread tonight's report as a reply to last night's, and a degraded run says
// WARNING instead of a false OK.
func TestBuildEventSubject(t *testing.T) {
	rec := report.Run{Command: report.CommandDump, Outcome: report.OutcomeSuccess,
		StartedAt: time.Date(2026, 7, 8, 1, 0, 0, 0, time.Local)}
	ev := buildEvent("box", rec)
	if !strings.Contains(ev.Subject, "dump OK on box") || !strings.Contains(ev.Subject, "2026-07-08") {
		t.Errorf("subject = %q, want verdict + date", ev.Subject)
	}
	rec.Warnings = []string{"landing c2 tripped mid-run"}
	ev = buildEvent("box", rec)
	if !strings.Contains(ev.Subject, "dump WARNING on box") || !ev.Warned || ev.Failed {
		t.Errorf("warned subject = %q (Warned=%v Failed=%v), want WARNING", ev.Subject, ev.Warned, ev.Failed)
	}
	if !strings.Contains(ev.Body, "landing c2 tripped mid-run") {
		t.Errorf("body = %q, want it to carry the warning", ev.Body)
	}
}

// fakeNotifier records the events it is asked to send and can be made to fail.
type fakeNotifier struct {
	got *[]Event
	err error
}

func (f fakeNotifier) Notify(_ context.Context, ev Event) error {
	*f.got = append(*f.got, ev)
	return f.err
}

func TestDispatchRunRoutingAndBuild(t *testing.T) {
	var got []Event
	register(Spec{Name: "fake", build: func(b config.NotifyBackend) (Notifier, error) {
		return fakeNotifier{got: &got}, nil
	}})
	cfg := config.NotifyConfig{Backends: map[string]config.NotifyBackend{
		"a": {Type: "fake"}, "b": {Type: "fake"},
	}}
	DispatchRun(context.Background(), cfg, "box", failRun(), failWarn(t))
	if len(got) != 2 {
		t.Fatalf("failure with no routing reached %d backends, want 2 (all)", len(got))
	}
	ev := got[0]
	if !strings.Contains(ev.Subject, "dump FAILED on box") {
		t.Errorf("subject = %q, want it to name the command/outcome/host", ev.Subject)
	}
	if !strings.Contains(ev.Body, "boom") {
		t.Errorf("body = %q, want it to carry the error", ev.Body)
	}
}

func TestDispatchBackendErrorIsWarnedNotFatal(t *testing.T) {
	var got []Event
	register(Spec{Name: "fake-err", build: func(b config.NotifyBackend) (Notifier, error) {
		return fakeNotifier{got: &got, err: io.ErrUnexpectedEOF}, nil
	}})
	cfg := config.NotifyConfig{Backends: map[string]config.NotifyBackend{"x": {Type: "fake-err"}}}
	var warnings int
	DispatchRun(context.Background(), cfg, "", failRun(), func(string, ...any) { warnings++ })
	if len(got) != 1 {
		t.Errorf("backend should still be attempted, got %d sends", len(got))
	}
	if warnings == 0 {
		t.Errorf("a backend error should produce a warning")
	}
}

func TestNewSMTPMissingSecret(t *testing.T) {
	_, err := newSMTP(config.NotifyBackend{Type: "smtp", Host: "mail", From: "a@b", To: []string{"c@d"}, PasswordEnv: "NB_TEST_SMTP_UNSET"})
	if err == nil {
		t.Errorf("expected error when password env is unset")
	}
}

func TestNewWebhookMissingSecret(t *testing.T) {
	_, err := newWebhook(config.NotifyBackend{Type: "webhook", URLEnv: "NB_TEST_HOOK_UNSET"})
	if err == nil {
		t.Errorf("expected error when url env is unset")
	}
}

func TestSMTPNotifyBuildsMessage(t *testing.T) {
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg []byte
	n := &smtpNotifier{
		addr: "mail:587", from: "backups@x", to: []string{"ops@x"},
		send: func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
			gotAddr, gotFrom, gotTo, gotMsg = addr, from, to, msg
			return nil
		},
	}
	err := n.Notify(context.Background(), Event{Subject: "nbackup dump FAILED", Body: "details\nhere"})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotAddr != "mail:587" || gotFrom != "backups@x" || len(gotTo) != 1 {
		t.Errorf("send args = %q %q %v", gotAddr, gotFrom, gotTo)
	}
	msg := string(gotMsg)
	for _, want := range []string{"Subject: nbackup dump FAILED", "To: ops@x", "details\r\nhere"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\n%s", want, msg)
		}
	}
}

func TestSMTPMessageIsMultipartWithMonospaceHTML(t *testing.T) {
	msg := string(smtpMessage("backups@x", []string{"ops@x"}, "nbackup dump OK", "DLE      level\nroot/etc 1 <ok>"))
	for _, want := range []string{
		"Content-Type: multipart/alternative;",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Type: text/html; charset=utf-8",
		"monospace",             // the HTML part renders the columns monospace
		"<pre",                  // wrapped so column alignment survives
		"root/etc 1 &lt;ok&gt;", // body HTML-escaped in the HTML part
		"root/etc 1 <ok>",       // body verbatim in the plaintext part
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\n%s", want, msg)
		}
	}
}

func TestSMTPNotifyContextTimeout(t *testing.T) {
	n := &smtpNotifier{
		addr: "mail:587", from: "a@x", to: []string{"b@x"},
		send: func(string, smtp.Auth, string, []string, []byte) error {
			time.Sleep(time.Second)
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := n.Notify(ctx, Event{}); err == nil {
		t.Errorf("expected a context deadline error from a hung send")
	}
}

func TestSendmailNotifyPipesMessage(t *testing.T) {
	var gotPath string
	var gotArgs []string
	var gotMsg []byte
	n := &sendmailNotifier{
		path: "/usr/sbin/sendmail", from: "backups@x", to: []string{"ops@x", "boss@x"},
		run: func(_ context.Context, path string, args []string, msg []byte) error {
			gotPath, gotArgs, gotMsg = path, args, msg
			return nil
		},
	}
	err := n.Notify(context.Background(), Event{Subject: "nbackup dump FAILED", Body: "details\nhere"})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotPath != "/usr/sbin/sendmail" {
		t.Errorf("path = %q", gotPath)
	}
	// Recipients are passed explicitly after "--", and the envelope sender via -f.
	if strings.Join(gotArgs, " ") != "-i -f backups@x -- ops@x boss@x" {
		t.Errorf("args = %v", gotArgs)
	}
	msg := string(gotMsg)
	for _, want := range []string{"Subject: nbackup dump FAILED", "To: ops@x, boss@x", "details\r\nhere"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\n%s", want, msg)
		}
	}
}

func TestNewSendmailDefaultsPath(t *testing.T) {
	n, err := newSendmail(config.NotifyBackend{Type: "sendmail", From: "a@b", To: []string{"c@d"}})
	if err != nil {
		t.Fatalf("newSendmail: %v", err)
	}
	if got := n.(*sendmailNotifier).path; got != defaultSendmailPath {
		t.Errorf("path = %q, want default %q", got, defaultSendmailPath)
	}
}

// stubDoer records the request and returns a canned response.
type stubDoer struct {
	gotURL  string
	gotBody string
	status  int
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	s.gotURL = req.URL.String()
	b, _ := io.ReadAll(req.Body)
	s.gotBody = string(b)
	st := s.status
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Status: http.StatusText(st), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func TestWebhookNotifyPostsJSON(t *testing.T) {
	doer := &stubDoer{}
	n := &webhookNotifier{url: "https://hooks.example/x", field: "text", client: doer}
	err := n.Notify(context.Background(), Event{Subject: "nbackup drill FAILED", Body: "1 failure"})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if doer.gotURL != "https://hooks.example/x" {
		t.Errorf("posted to %q", doer.gotURL)
	}
	if !strings.Contains(doer.gotBody, `"text"`) || !strings.Contains(doer.gotBody, "drill FAILED") {
		t.Errorf("payload = %q, want a Slack-style text field with the message", doer.gotBody)
	}
}

func TestWebhookNotifyNon2xxIsError(t *testing.T) {
	n := &webhookNotifier{url: "https://x", field: "text", client: &stubDoer{status: 500}}
	if err := n.Notify(context.Background(), Event{Subject: "s", Body: "b"}); err == nil {
		t.Errorf("expected error on a 500 response")
	}
}

func TestNewWebhookFromEnv(t *testing.T) {
	t.Setenv("NB_TEST_HOOK", "https://hooks.example/secret")
	n, err := newWebhook(config.NotifyBackend{Type: "webhook", URLEnv: "NB_TEST_HOOK"})
	if err != nil {
		t.Fatalf("newWebhook: %v", err)
	}
	if got := n.(*webhookNotifier).url; got != "https://hooks.example/secret" {
		t.Errorf("resolved url = %q, want the env value", got)
	}
}

func failWarn(t *testing.T) Warnf {
	return func(format string, args ...any) { t.Errorf("unexpected warning: "+format, args...) }
}
