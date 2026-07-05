package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/report"
)

// fakeSource is a canned read-only Source for exercising the handlers without an
// engine. That it need implement only these read methods is the read-only guarantee
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

// DLENames returns the DLEs seen in the fake's runs — the "configured" set for
// drill coverage.
func (f fakeSource) DLENames() []string {
	var names []string
	seen := map[string]bool{}
	for _, r := range f.runs {
		for _, a := range r.Archives {
			if !seen[a.DLE] {
				seen[a.DLE] = true
				names = append(names, a.DLE)
			}
		}
	}
	return names
}

func (f fakeSource) DrillWindow() time.Duration { return 30 * 24 * time.Hour }

// DLESummaries aggregates the fake's runs per DLE, mirroring catalog.DLESummaries
// closely enough to render the DLE pages (every archive here is on "disk").
func (f fakeSource) DLESummaries() []catalog.DLESummary {
	byDLE := map[string]*catalog.DLESummary{}
	var order []string
	for _, r := range f.runs {
		for _, a := range r.Archives {
			s := byDLE[a.DLE]
			if s == nil {
				s = &catalog.DLESummary{DLE: a.DLE, Display: a.DLEID(), Media: []string{"disk"}}
				byDLE[a.DLE] = s
				order = append(order, a.DLE)
			}
			s.Runs++
			s.Bytes += a.Compressed
			s.LastLevel = a.Level
			if a.Level == 0 {
				s.LastFull = r.Date()
			}
		}
	}
	out := make([]catalog.DLESummary, 0, len(order))
	for _, slug := range order {
		out = append(out, *byDLE[slug])
	}
	return out
}

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
		{"/dles", "localhost:/src"},                       // DLE identity links in the list
		{"/dles/local", "run-2026-07-03.120000"},          // the DLE's per-run history links back to the run
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

func TestUnknownDLEIsFriendly(t *testing.T) {
	h := NewServer(sampleSource(), t.TempDir()).Handler()
	code, body := get(t, h, "/dles/does-not-exist")
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

func TestDrillsPageRendersLedgerAndHistory(t *testing.T) {
	dir := t.TempDir()
	// One passing and one failing ledger record, plus an unknown-to-the-ledger DLE
	// ("local" from sampleSource) that must show as never drilled.
	ledger := &drill.Ledger{}
	ledger.Update(drill.Record{
		DLE: "svc-a", LastDrill: time.Now().Add(-2 * 24 * time.Hour), Tier: "structural",
		Medium: "disk", AsOf: "2026-07-03", RunID: "run-2026-07-03.120000", OK: true,
		Bytes: 123_000, Drills: 3,
	})
	ledger.Update(drill.Record{
		DLE: "svc-b", LastDrill: time.Now().Add(-24 * time.Hour), Tier: "chain",
		Medium: "offsite", AsOf: "2026-07-03", RunID: "run-2026-07-03.120000", OK: false,
		Class: "pipeline", Detail: "gpg: decryption failed",
	})
	if err := ledger.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := report.Append(dir, report.Run{
		Command: report.CommandDrill, EndedAt: time.Now(), Outcome: report.OutcomeSuccess,
		Tier: "structural", BytesMoved: 123_000,
		DrillHealth: []report.DrillHealth{{DLE: "svc-a", OK: true, Drilled: true, Bytes: 123_000}},
	}); err != nil {
		t.Fatal(err)
	}

	code, body := get(t, NewServer(sampleSource(), dir).Handler(), "/drills")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{
		"svc-a", "structural", "123.00 kB", // passing ledger row: tier + egress
		"svc-b", "pipeline", "gpg: decryption failed", // failing row: class + detail
		"Never drilled", "local", // coverage: the configured DLE with no record
		"1 DLE(s) drilled", // the recent drill run from the history
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/drills missing %q", want)
		}
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
	for _, p := range []string{"/", "/runs", "/dles", "/media", "/drills", "/report", "/status"} {
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
