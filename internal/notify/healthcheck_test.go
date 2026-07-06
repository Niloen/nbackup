package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Niloen/nbackup/internal/config"
)

// pathRecorder is an httptest handler that records every request path it sees, so
// tests can assert which of /start, "" (success), /fail was hit.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *pathRecorder) handler(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (r *pathRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func TestHealthcheckStartSuccessFailPaths(t *testing.T) {
	rec := &pathRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	n, err := newHealthcheck(config.NotifyBackend{Type: "healthcheck", URL: srv.URL})
	if err != nil {
		t.Fatalf("newHealthcheck: %v", err)
	}
	starter, ok := n.(Starter)
	if !ok {
		t.Fatalf("healthcheck backend does not implement Starter")
	}
	if err := starter.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := n.Notify(context.Background(), Event{Failed: false}); err != nil {
		t.Fatalf("Notify (success): %v", err)
	}
	if err := n.Notify(context.Background(), Event{Failed: true}); err != nil {
		t.Fatalf("Notify (failure): %v", err)
	}

	got := rec.got()
	want := []string{"/start", "/", "/fail"}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHealthcheckNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n, err := newHealthcheck(config.NotifyBackend{Type: "healthcheck", URL: srv.URL})
	if err != nil {
		t.Fatalf("newHealthcheck: %v", err)
	}
	if err := n.Notify(context.Background(), Event{}); err == nil {
		t.Errorf("expected error on a 500 response")
	}
}

func TestNewHealthcheckMissingSecret(t *testing.T) {
	_, err := newHealthcheck(config.NotifyBackend{Type: "healthcheck", URLEnv: "NB_TEST_HC_UNSET"})
	if err == nil {
		t.Errorf("expected error when url env is unset")
	}
}

func TestNewHealthcheckFromEnv(t *testing.T) {
	t.Setenv("NB_TEST_HC", "https://hc.example/ping/abc")
	n, err := newHealthcheck(config.NotifyBackend{Type: "healthcheck", URLEnv: "NB_TEST_HC"})
	if err != nil {
		t.Fatalf("newHealthcheck: %v", err)
	}
	if got := n.(*healthcheckNotifier).url; got != "https://hc.example/ping/abc" {
		t.Errorf("resolved url = %q, want the env value", got)
	}
}

// TestDispatchRunAlwaysIncludesHealthcheck pins the dead-man's-switch bypass: a
// healthcheck backend fires on a success outcome for a non-dump command even
// though routeFor would normally leave it silent (opt-in success routing).
func TestDispatchRunAlwaysIncludesHealthcheck(t *testing.T) {
	rec := &pathRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	cfg := config.NotifyConfig{Backends: map[string]config.NotifyBackend{
		"hc": {Type: "healthcheck", URL: srv.URL},
	}}
	syncSuccess := failRun()
	syncSuccess.Command, syncSuccess.Outcome = "sync", "success"
	DispatchRun(context.Background(), cfg, "", syncSuccess, failWarn(t))

	got := rec.got()
	if len(got) != 1 || got[0] != "/" {
		t.Errorf("healthcheck paths = %v, want a single success ping even though sync success is opt-in for report backends", got)
	}
}

// TestDispatchStartPingsOnlyHealthchecks confirms DispatchStart fires for every
// healthcheck backend and skips backend types that don't implement Starter.
func TestDispatchStartPingsOnlyHealthchecks(t *testing.T) {
	rec := &pathRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	var fakeCalls int
	register(Spec{Name: "fake-nonstarter", build: func(b config.NotifyBackend) (Notifier, error) {
		return fakeNotifier{got: new([]Event)}, nil
	}})
	_ = fakeCalls
	cfg := config.NotifyConfig{Backends: map[string]config.NotifyBackend{
		"hc":   {Type: "healthcheck", URL: srv.URL},
		"chat": {Type: "fake-nonstarter"},
	}}
	DispatchStart(context.Background(), cfg, "dump", failWarn(t))

	got := rec.got()
	if len(got) != 1 || got[0] != "/start" {
		t.Errorf("start paths = %v, want a single /start ping", got)
	}
}

// TestDispatchFinishClosesOutASkippedNoOp pins the no-op fix: a skipped run
// (never a report.Run, never routed to a report channel) still owes its
// healthcheck backend a completion ping to match the /start DispatchStart already
// sent — otherwise healthchecks.io flags a started-but-unfinished check after its
// grace period, a false alarm on a healthy no-op. It also asserts a non-Starter
// backend (a webhook stand-in) never sees the skip at all: skip semantics ("this
// never happened") must hold for every report channel.
func TestDispatchFinishClosesOutASkippedNoOp(t *testing.T) {
	rec := &pathRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	var webhookEvents []Event
	register(Spec{Name: "fake-webhook", build: func(b config.NotifyBackend) (Notifier, error) {
		return fakeNotifier{got: &webhookEvents}, nil
	}})
	cfg := config.NotifyConfig{Backends: map[string]config.NotifyBackend{
		"hc":   {Type: "healthcheck", URL: srv.URL},
		"chat": {Type: "fake-webhook"},
	}}

	// The seam's normal sequence for a no-op: /start, then (build skips), then finish.
	DispatchStart(context.Background(), cfg, "sync", failWarn(t))
	DispatchFinish(context.Background(), cfg, false, failWarn(t))

	got := rec.got()
	want := []string{"/start", "/"}
	if len(got) != len(want) {
		t.Fatalf("healthcheck paths = %v, want %v (start, then a success ping — no /fail)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if len(webhookEvents) != 0 {
		t.Errorf("a skipped no-op reached the webhook backend (%d events); it must never leave the healthcheck-only path", len(webhookEvents))
	}
}

// TestDispatchFinishFailedSkipPingsFail confirms an argument-validation skip (a
// non-nil skipRun error) pings /fail rather than the success URL.
func TestDispatchFinishFailedSkipPingsFail(t *testing.T) {
	rec := &pathRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	cfg := config.NotifyConfig{Backends: map[string]config.NotifyBackend{
		"hc": {Type: "healthcheck", URL: srv.URL},
	}}
	DispatchFinish(context.Background(), cfg, true, failWarn(t))

	got := rec.got()
	if len(got) != 1 || got[0] != "/fail" {
		t.Errorf("paths = %v, want a single /fail ping", got)
	}
}
