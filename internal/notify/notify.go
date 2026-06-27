// Package notify is NBackup's unattended-alerting layer: it turns a finished run
// (report.Run) into a notification delivered over a configured channel, so a
// cron-driven backup with no operator watching still makes a failure loud. It is the
// channel half of the "0 errors" story — verified recoverability only matters if a
// non-zero result reaches a human.
//
// Like filter/crypt/media, a channel is a registered, named backend (smtp, sendmail, webhook),
// so adding one is a registry registration, not a core conditional. Like
// progress.NewFileSink, notification is best-effort: a backend error, a missing
// secret, or a hung endpoint is a warning the caller logs — it never fails or blocks
// the backup. Secrets (SMTP password, webhook URL) are read from the environment by
// name at send time and never stored in config (crypt's orchestrate-don't-hoard
// stance). The package depends only on config (for the declarative backend
// definitions) and report (for the record + its rendering); nothing imports it but
// the CLI.
package notify

import (
	"context"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/report"
)

// dispatchTimeout bounds a single backend send, so a hung SMTP server or webhook
// endpoint can never wedge a cron run waiting to report.
const dispatchTimeout = 30 * time.Second

// Event is what a backend renders and delivers: the run plus the host/config
// identity that frames it, with a pre-rendered subject and body so every backend
// sends the same message (and the body matches `nb report`'s rendering).
type Event struct {
	Outcome report.Outcome
	Command report.Command
	Host    string
	Config  string
	Run     report.Run
	Subject string
	Body    string
}

// Notifier delivers one Event over one channel.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

// Warnf logs a non-fatal notification problem (a backend error, a missing secret).
// The CLI passes a function that writes a stderr warning; notification problems are
// never fatal.
type Warnf func(format string, args ...any)

// Spec builds a Notifier of one registered type from its backend config.
type Spec struct {
	Name  string
	build func(b config.NotifyBackend) (Notifier, error)
}

var registry = map[string]Spec{}

func register(s Spec) { registry[s.Name] = s }

func init() {
	register(Spec{Name: "smtp", build: newSMTP})
	register(Spec{Name: "sendmail", build: newSendmail})
	register(Spec{Name: "webhook", build: newWebhook})
}

// DispatchRun delivers a finished run to the backends routed for its command and
// outcome (see routeFor). It builds the Event once and is best-effort per backend.
func DispatchRun(ctx context.Context, cfg config.NotifyConfig, id Event, rec report.Run, warn Warnf) {
	names := routeFor(cfg, rec.Command, rec.Outcome)
	if len(names) == 0 {
		return
	}
	deliver(ctx, cfg, names, buildEvent(id, rec), warn)
}

// DispatchDigest delivers an `nb report --notify` digest to the configured `digest`
// backends. body is the already-rendered digest text.
func DispatchDigest(ctx context.Context, cfg config.NotifyConfig, id Event, subject, body string, warn Warnf) {
	ev := id
	ev.Subject, ev.Body = subject, body
	deliver(ctx, cfg, cfg.Digest, ev, warn)
}

// routeFor resolves which backends fire for a command's outcome.
//
//   - Failure: every configured backend by default (failures must be loud), or the
//     explicit on_failure list.
//   - Success: a successful dump notifies by default — the nightly "backups happened"
//     signal, so an empty inbox reads as a problem (cron didn't run) rather than a
//     healthy run. The other commands' success is opt-in. An explicit on_success list
//     overrides both, applying to every command's success.
func routeFor(cfg config.NotifyConfig, cmd report.Command, out report.Outcome) []string {
	switch out {
	case report.OutcomeFailure:
		if len(cfg.OnFailure) == 0 {
			return backendNames(cfg)
		}
		return cfg.OnFailure
	case report.OutcomeSuccess:
		if len(cfg.OnSuccess) > 0 {
			return cfg.OnSuccess
		}
		if cmd == report.CommandDump {
			return backendNames(cfg)
		}
		return nil
	}
	return nil
}

// deliver builds and calls each named backend, isolating failures: a build error
// (e.g. an unset secret env var) or a send error is warned and skipped, never
// propagated. Each send runs under dispatchTimeout.
func deliver(ctx context.Context, cfg config.NotifyConfig, names []string, ev Event, warn Warnf) {
	for _, name := range names {
		b, ok := cfg.Backends[name]
		if !ok {
			warnf(warn, "notify: no backend %q", name)
			continue
		}
		spec, ok := registry[b.Type]
		if !ok {
			warnf(warn, "notify: backend %q: unknown type %q", name, b.Type)
			continue
		}
		n, err := spec.build(b)
		if err != nil {
			warnf(warn, "notify: backend %q: %v", name, err)
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, dispatchTimeout)
		err = n.Notify(sendCtx, ev)
		cancel()
		if err != nil {
			warnf(warn, "notify: backend %q: %v", name, err)
		}
	}
}

// buildEvent renders the subject and body for a run, reusing report.RenderRun so the
// notification body matches what `nb report` shows.
func buildEvent(id Event, rec report.Run) Event {
	ev := id
	ev.Outcome, ev.Command, ev.Run = rec.Outcome, rec.Command, rec
	state := "OK"
	if rec.Failed() {
		state = "FAILED"
	}
	host := id.Host
	if host != "" {
		host = " on " + host
	}
	ev.Subject = "nbackup " + string(rec.Command) + " " + state + host
	var sb strings.Builder
	report.RenderRun(&sb, rec)
	ev.Body = sb.String()
	return ev
}

func backendNames(cfg config.NotifyConfig) []string {
	names := make([]string, 0, len(cfg.Backends))
	for name := range cfg.Backends {
		names = append(names, name)
	}
	return names
}

func warnf(warn Warnf, format string, args ...any) {
	if warn != nil {
		warn(format, args...)
	}
}
