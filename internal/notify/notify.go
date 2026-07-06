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

// Event is what a backend delivers: a pre-rendered subject and body, so every
// backend sends the same message (and a run's body matches `nb report`'s
// rendering). Rendering happens once, in the dispatch layer — backends only carry.
// Command and Failed are structured companions to Subject/Body for backends that
// need to branch or export the outcome machine-readably (healthcheck's
// success-vs-/fail URL, command's NB_COMMAND/NB_STATUS env vars) rather than parse
// the rendered text; a digest has no single command or pass/fail verdict, so it
// leaves both zero.
type Event struct {
	Subject string
	Body    string
	Command string
	Failed  bool
}

// Notifier delivers one Event over one channel.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

// Starter is implemented by backends that need a liveness signal when a covered
// run *begins*, before its outcome is known — currently only healthcheck (a
// dead-man's-switch ping needs a "run started" beacon, not just pass/fail, so a
// wedged or killed process is still detected as missing its next ping). It is a
// separate interface rather than part of Notifier because every other backend has
// no such event.
type Starter interface {
	Start(ctx context.Context) error
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
	register(Spec{Name: "healthcheck", build: newHealthcheck})
	register(Spec{Name: "command", build: newCommand})
}

// DispatchRun delivers a finished run to the backends routed for its command and
// outcome (see routeFor), plus every healthcheck backend unconditionally. host
// frames the subject ("… on <host>"; "" omits it). It builds the Event once and is
// best-effort per backend.
//
// A healthcheck backend is a dead-man's switch, not a report: its whole value is
// pinging on every covered run so a *missing* ping (not a delivered failure
// message) is the alarm. Gating it behind on_failure/on_success like a report
// channel would silently blind it to whichever commands/outcomes the operator
// didn't list, so it always fires — see DispatchStart for the matching start ping.
func DispatchRun(ctx context.Context, cfg config.NotifyConfig, host string, rec report.Run, warn Warnf) {
	names := withHealthchecks(cfg, routeFor(cfg, rec.Command, rec.Outcome))
	if len(names) == 0 {
		return
	}
	deliver(ctx, cfg, names, buildEvent(host, rec), warn)
}

// DispatchStart pings every configured healthcheck backend that a covered run has
// begun (the healthchecks.io "/start" convention), independent of routing — see
// DispatchRun. Other backend types don't implement Starter and are skipped; this
// is the seam a caller (runReported) hits once, before the run's own work starts.
func DispatchStart(ctx context.Context, cfg config.NotifyConfig, cmd report.Command, warn Warnf) {
	forEachStarter(cfg, warn, func(name string, s Starter, _ Notifier) {
		sendCtx, cancel := context.WithTimeout(ctx, dispatchTimeout)
		defer cancel()
		if err := s.Start(sendCtx); err != nil {
			warnf(warn, "notify: backend %q: start ping: %v", name, err)
		}
	})
}

// DispatchFinish pings every configured healthcheck backend to close out a covered
// run that runReported *skipped* (a no-op or an argument-validation error): such a
// run writes no report.Run record and reaches no report channel (smtp/webhook/
// command), by design — but DispatchStart already sent it a /start ping, since a
// skip is only discovered after build() returns. Leaving that ping unanswered
// would make healthchecks.io flag a started-but-unfinished check once its grace
// period lapses: a false alarm on what was actually a healthy no-op. failed
// distinguishes a clean no-op (success ping) from an argument-validation error
// (fail ping); it never touches routing or report channels, matching skipRun's
// "this never happened" semantics for everything but the liveness beacon.
func DispatchFinish(ctx context.Context, cfg config.NotifyConfig, failed bool, warn Warnf) {
	forEachStarter(cfg, warn, func(name string, _ Starter, n Notifier) {
		sendCtx, cancel := context.WithTimeout(ctx, dispatchTimeout)
		defer cancel()
		if err := n.Notify(sendCtx, Event{Failed: failed}); err != nil {
			warnf(warn, "notify: backend %q: finish ping: %v", name, err)
		}
	})
}

// forEachStarter builds every configured backend and invokes fn for the ones that
// implement Starter (currently only healthcheck) — the shared iteration behind
// DispatchStart and DispatchFinish, both of which act on that same subset
// independent of on_failure/on_success routing.
func forEachStarter(cfg config.NotifyConfig, warn Warnf, fn func(name string, s Starter, n Notifier)) {
	for name, b := range cfg.Backends {
		spec, ok := registry[b.Type]
		if !ok {
			continue
		}
		n, err := spec.build(b)
		if err != nil {
			warnf(warn, "notify: backend %q: %v", name, err)
			continue
		}
		s, ok := n.(Starter)
		if !ok {
			continue
		}
		fn(name, s, n)
	}
}

// withHealthchecks unions every configured healthcheck backend into names, so a
// dead-man's-switch backend fires regardless of on_failure/on_success routing.
func withHealthchecks(cfg config.NotifyConfig, names []string) []string {
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	for name, b := range cfg.Backends {
		if b.Type == "healthcheck" && !have[name] {
			names = append(names, name)
			have[name] = true
		}
	}
	return names
}

// DispatchDigest delivers an `nb report --notify` digest to the configured `digest`
// backends. body is the already-rendered digest text.
func DispatchDigest(ctx context.Context, cfg config.NotifyConfig, subject, body string, warn Warnf) {
	deliver(ctx, cfg, cfg.Digest, Event{Subject: subject, Body: body}, warn)
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
func buildEvent(host string, rec report.Run) Event {
	state := "OK"
	if rec.Failed() {
		state = "FAILED"
	}
	if host != "" {
		host = " on " + host
	}
	var sb strings.Builder
	report.RenderRun(&sb, rec)
	return Event{
		Subject: "nbackup " + string(rec.Command) + " " + state + host,
		Body:    sb.String(),
		Command: string(rec.Command),
		Failed:  rec.Failed(),
	}
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
