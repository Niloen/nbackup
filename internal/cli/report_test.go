package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/report"
)

// runReported is the single seam every run-producing command flows through, so its
// contract — record the outcome, but never let recording change the exit code — is
// what these tests pin down. No engine is needed: the seam only touches the record.

func TestRunReportedSuccessRecordsRun(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Workdir: dir}
	a := &app{quiet: true}

	// The seed carries the failure ExitClass to use only if the body fails.
	err := a.runReported(cfg, report.Run{Command: report.CommandDump, ExitClass: "dump-failed"}, func() (report.Run, error) {
		return report.Run{Command: report.CommandDump, RunID: "run-x", Archives: 2, BytesMoved: 100}, nil
	})
	if err != nil {
		t.Fatalf("runReported returned error on success: %v", err)
	}
	runs, lerr := report.Load(dir)
	if lerr != nil || len(runs) != 1 {
		t.Fatalf("Load = %v, %v; want 1 run", runs, lerr)
	}
	if runs[0].Outcome != report.OutcomeSuccess || runs[0].RunID != "run-x" {
		t.Errorf("recorded run = %+v, want success/run-x", runs[0])
	}
	if runs[0].ExitClass != "" {
		t.Errorf("success record carries exit_class %q; the seed's failure class must not leak", runs[0].ExitClass)
	}
	if runs[0].StartedAt.IsZero() || runs[0].EndedAt.IsZero() {
		t.Errorf("seam did not stamp timing: %+v", runs[0])
	}
}

func TestRunReportedFailurePreservesErrorAndExitClass(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Workdir: dir}
	a := &app{quiet: true}
	sentinel := errors.New("archivers failed")

	err := a.runReported(cfg, report.Run{Command: report.CommandDump, ExitClass: "dump-failed"}, func() (report.Run, error) {
		return report.Run{}, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("runReported returned %v, want the build error verbatim", err)
	}
	runs, _ := report.Load(dir)
	if len(runs) != 1 {
		t.Fatalf("expected 1 recorded run, got %d", len(runs))
	}
	r := runs[0]
	if r.Outcome != report.OutcomeFailure || r.Error != sentinel.Error() {
		t.Errorf("recorded failure = %+v, want failure with the error message", r)
	}
	if r.Command != report.CommandDump || r.ExitClass != "dump-failed" {
		t.Errorf("seed fields not preserved on early failure: %+v", r)
	}
}

func TestRunReportedRecordingErrorDoesNotChangeExit(t *testing.T) {
	// Make the workdir unwritable as a directory by occupying its path with a file,
	// so report.Append fails — the seam must warn, not fail the run.
	base := t.TempDir()
	asFile := filepath.Join(base, "workdir-is-a-file")
	if err := os.WriteFile(asFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	cfg := &config.Config{Workdir: asFile}
	a := &app{quiet: true}

	sentinel := errors.New("real failure")
	err := a.runReported(cfg, report.Run{Command: report.CommandVerify}, func() (report.Run, error) {
		return report.Run{Command: report.CommandVerify, Failures: 1}, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("a recording error masked the run error: got %v, want %v", err, sentinel)
	}
}

// TestRunReportedSkipStillClosesOutHealthcheck pins the no-op dead-man's-switch
// fix: a build that returns skip(nil) (a clean no-op, e.g. nothing to sync) writes
// no run record and reaches no report channel, but a configured healthcheck
// backend already got a /start ping before build() ran — it must also get a
// matching completion ping, or healthchecks.io flags it as started-but-unfinished
// once its grace period lapses.
func TestRunReportedSkipStillClosesOutHealthcheck(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{Workdir: dir, Notify: config.NotifyConfig{
		Backends: map[string]config.NotifyBackend{"hc": {Type: "healthcheck", URL: srv.URL}},
	}}
	a := &app{quiet: true}

	err := a.runReported(cfg, report.Run{Command: report.CommandSync}, func() (report.Run, error) {
		return report.Run{}, skip(nil)
	})
	if err != nil {
		t.Fatalf("runReported returned %v, want nil for a clean no-op", err)
	}

	runs, _ := report.Load(dir)
	if len(runs) != 0 {
		t.Errorf("a skipped no-op wrote %d run records, want 0", len(runs))
	}

	mu.Lock()
	got := append([]string(nil), paths...)
	mu.Unlock()
	want := []string{"/start", "/"}
	if len(got) != len(want) {
		t.Fatalf("healthcheck paths = %v, want %v (start, then a success ping — no /fail)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
