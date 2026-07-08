package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/notify"
	"github.com/Niloen/nbackup/internal/report"
)

// dispatchNotify delivers a finished run to the backends routed for its outcome.
// It is best-effort: nothing here ever fails or blocks the run (the caller has
// already returned the run's own error); a notification problem is a stderr warning,
// matching the progress/reporting stance.
func (a *app) dispatchNotify(cfg *config.Config, rec report.Run) {
	if len(cfg.Notify.Backends) == 0 {
		return
	}
	notify.DispatchRun(context.Background(), cfg.Notify, hostname(), rec, notifyWarn)
}

// dispatchNotifyStart pings every configured healthcheck backend that a covered
// run (cmd) has begun, before the run's own work starts — the runReported seam's
// only hook into the dead-man's-switch "/start" beacon (see notify.DispatchStart).
func (a *app) dispatchNotifyStart(cfg *config.Config, cmd report.Command) {
	if len(cfg.Notify.Backends) == 0 {
		return
	}
	notify.DispatchStart(context.Background(), cfg.Notify, cmd, notifyWarn)
}

// dispatchNotifyFinish pings every configured healthcheck backend to close out a
// run that runReported found to be a skip (a no-op or argument-validation
// failure): it never became a report.Run record or reached a report channel, but
// it already got a /start ping and would otherwise dangle (see
// notify.DispatchFinish). failed selects the success vs. /fail ping.
func (a *app) dispatchNotifyFinish(cfg *config.Config, failed bool) {
	if len(cfg.Notify.Backends) == 0 {
		return
	}
	notify.DispatchFinish(context.Background(), cfg.Notify, failed, notifyWarn)
}

// dispatchDigest sends an `nb report --notify` digest through the config's
// notify.digest backends. The body is the same digest `nb report` prints.
func (a *app) dispatchDigest(cfg *config.Config, runs []report.Run) {
	if len(cfg.Notify.Digest) == 0 {
		fmt.Fprintln(os.Stderr, "warning: --notify: no notify.digest backends configured")
		return
	}
	var body bytes.Buffer
	report.Render(&body, runs, time.Now())
	renderDrillLedger(&body, cfg, time.Now())
	renderStaleness(&body, cfg, time.Now())
	// The subject carries the date (Amanda's mail-report convention) so each
	// digest gets its own subject instead of threading as a reply to the last one.
	subject := "nbackup report"
	if host := hostname(); host != "" {
		subject += " on " + host
	}
	subject += " — " + time.Now().Local().Format("2006-01-02")
	notify.DispatchDigest(context.Background(), cfg.Notify, subject, body.String(), notifyWarn)
}

// notifyWarn logs a non-fatal notification problem to stderr.
func notifyWarn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...)
}

// hostname returns the local host name for notification subjects, or "" if unknown.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}
