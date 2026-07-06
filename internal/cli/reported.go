package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/report"
)

// runReported executes a run-producing command body, records its outcome to the run
// history, and dispatches notifications — in one place, so each command
// neither re-implements recording nor risks letting it change the exit code.
//
// build does the actual work and returns a partially-populated report.Run (its
// command-specific fields) plus the run's error. runReported stamps the timing and
// outcome, persists the record, fires notifications, and then returns build's error
// verbatim: a failure to write the summary or send a notification is a stderr
// warning, never the cause — nor a suppressor — of the run's own exit code (the
// progress.NewFileSink contract).
// skipRun, returned by a runReported build as its error, marks the command as not a
// recordable run — an argument-validation failure or a no-op never "ran" in the recovery
// sense, so it must not appear in nb report or fire a notification. runReported returns the
// wrapped error (nil for a clean no-op) without writing a run record. Use it via skip(err).
type skipRun struct{ err error }

func (s skipRun) Error() string {
	if s.err == nil {
		return "no-op"
	}
	return s.err.Error()
}
func (s skipRun) Unwrap() error { return s.err }

// skip marks a build's result as not worth recording (a no-op or arg-validation error).
func skip(err error) error { return skipRun{err} }

func (a *app) runReported(cfg *config.Config, seed report.Run, build func() (report.Run, error)) error {
	start := time.Now().UTC()
	a.dispatchNotifyStart(cfg, seed.Command)
	rec, runErr := build()
	// A no-op or argument-validation result is not a run: surface its error (nil for a clean
	// no-op) but write no run record and fire no report-channel notification. A healthcheck
	// backend already got a /start ping above, though (the skip is only known now, after
	// build() returns), so it still needs a matching completion ping or it's left dangling.
	var sr skipRun
	if errors.As(runErr, &sr) {
		a.dispatchNotifyFinish(cfg, sr.err != nil)
		return sr.err
	}
	if rec.Command == "" {
		rec.Command = seed.Command
	}
	rec.StartedAt, rec.EndedAt = start, time.Now().UTC()
	if runErr != nil {
		rec.Outcome = report.OutcomeFailure
		rec.Error = runErr.Error()
		// Prefer a class the body's record already set, else the seed's
		// command-specific class (for a body that failed early with a zero record),
		// else a generic fallback.
		switch {
		case rec.ExitClass != "":
		case seed.ExitClass != "":
			rec.ExitClass = seed.ExitClass
		default:
			rec.ExitClass = "error"
		}
	} else {
		// A successful run carries no exit class — the seed's failure class must never
		// leak onto a passing record.
		rec.Outcome = report.OutcomeSuccess
		rec.ExitClass = ""
	}

	if err := report.Append(cfg.WorkdirPath(), rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: write run summary: %v\n", err)
	}
	a.dispatchNotify(cfg, rec)
	return runErr
}
