package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
)

// fakeSource is a canned read-only Source for exercising the handlers without an
// engine. That it need implement only these five methods is the read-only guarantee
// made concrete: there is no write verb to stub.
type fakeSource struct {
	runs  []*catalog.Run
	media []engine.MediumInfo
}

func (f fakeSource) Runs() []*catalog.Run { return f.runs }

func (f fakeSource) ReadRun(id string) (*catalog.Run, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, fmt.Errorf("run %s not in catalog", id)
}

func (f fakeSource) Placements(runID string) []catalog.Placement {
	return []catalog.Placement{{Medium: "disk"}}
}

func (f fakeSource) Media() []engine.MediumInfo { return f.media }

func (f fakeSource) DisplayDLE(slug string) string { return slug }

func sampleSource() fakeSource {
	return fakeSource{
		runs: []*catalog.Run{{
			ID: "run-2026-07-03.120000",
			Archives: []record.Archive{{
				Run: "run-2026-07-03.120000", DLE: "local", Host: "localhost", Path: "/src",
				Level: 0, Compressed: 200_000, FileCount: 2, Compress: "gzip", Encrypt: "none",
				CreatedAt: time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC),
			}},
		}},
		media: []engine.MediumInfo{{Name: "disk", Type: "disk", Runs: 1, Used: 200_000, Capacity: 10 << 30}},
	}
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestPagesRenderPopulated(t *testing.T) {
	h := NewServer(sampleSource(), t.TempDir()).Handler()
	cases := []struct{ path, want string }{
		{"/", "200.00 kB"},                                // cataloged total, formatted like the CLI
		{"/runs", "run-2026-07-03.120000"},                // run id links in the list
		{"/runs/run-2026-07-03.120000", "localhost:/src"}, // archive DLE identity
		{"/runs/run-2026-07-03.120000", "gzip"},           // per-archive compression scheme
		{"/media", "disk"},                                // medium name
	}
	for _, c := range cases {
		code, body := get(t, h, c.path)
		if code != http.StatusOK {
			t.Errorf("%s: code=%d, want 200", c.path, code)
		}
		if !strings.Contains(body, c.want) {
			t.Errorf("%s: body missing %q", c.path, c.want)
		}
	}
}

func TestUnknownRunIsFriendly(t *testing.T) {
	h := NewServer(sampleSource(), t.TempDir()).Handler()
	code, body := get(t, h, "/runs/run-does-not-exist")
	if code != http.StatusOK {
		t.Fatalf("code=%d, want 200 (a rendered not-found page, not a raw error)", code)
	}
	if !strings.Contains(body, "not found") {
		t.Errorf("expected a not-found message, got:\n%s", body)
	}
}

func TestFinishedRunHasNoInProgressBanner(t *testing.T) {
	dir := t.TempDir()
	// A finished run leaves its status file behind (phase done); the overview must not
	// still claim a run is in progress.
	progress.NewFileSink(dir, time.Now)(progress.Snapshot{
		RunID: "run-2026-07-03.140000", Phase: progress.PhaseDone,
		DLEs: []progress.DLE{{Name: "local", State: progress.StateDone, EstBytes: 1000, DoneBytes: 1000}},
	}, true)

	_, body := get(t, NewServer(sampleSource(), dir).Handler(), "/")
	if strings.Contains(body, "in progress") {
		t.Errorf("overview shows an in-progress banner for a finished run:\n%s", body)
	}
}

func TestUnknownPath404(t *testing.T) {
	h := NewServer(sampleSource(), t.TempDir()).Handler()
	if code, _ := get(t, h, "/nope"); code != http.StatusNotFound {
		t.Errorf("code=%d, want 404", code)
	}
}

func TestEmptyCatalog(t *testing.T) {
	h := NewServer(fakeSource{}, t.TempDir()).Handler()
	for _, p := range []string{"/", "/runs", "/media", "/report", "/status"} {
		if code, _ := get(t, h, p); code != http.StatusOK {
			t.Errorf("%s: code=%d, want 200 on an empty catalog", p, code)
		}
	}
}

func TestLiveStatusRenders(t *testing.T) {
	dir := t.TempDir()
	// A live status file makes /status render the in-progress run and its per-DLE table.
	progress.NewFileSink(dir, time.Now)(progress.Snapshot{
		RunID: "run-2026-07-03.130000", Phase: progress.PhaseRunning, Workers: 2,
		DLEs: []progress.DLE{{Name: "local", State: progress.StateDumping, EstBytes: 1000, DoneBytes: 500}},
	}, true)

	code, body := get(t, NewServer(sampleSource(), dir).Handler(), "/status")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(body, "run-2026-07-03.130000") || !strings.Contains(body, "50%") {
		t.Errorf("status page missing live run or 50%% progress:\n%s", body)
	}
}
