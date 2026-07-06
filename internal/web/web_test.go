package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
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
	usage []catalog.UsageSample // the canned ledger the medium page's chart draws
	stale []catalog.StaleDLE    // overdue DLEs against the dump cycle
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

// MediumStats derives a usage picture for a named medium from the fake's runs (every
// archive here lands on "disk", matching the single placement Placements returns) plus
// the fake's canned ledger samples — mirroring accounting.MediumStats closely enough
// to render the medium detail page.
func (f fakeSource) MediumStats(name string) (engine.MediumStats, bool) {
	var info engine.MediumInfo
	found := false
	for _, m := range f.media {
		if m.Name == name {
			info, found = m, true
		}
	}
	if !found {
		return engine.MediumStats{}, false
	}
	st := engine.MediumStats{MediumInfo: info}
	if name != "disk" {
		return st, true // the fake places everything on "disk"; other media hold nothing
	}
	type point struct {
		id    string
		at    time.Time
		bytes int64
	}
	var pts []point
	for _, r := range f.runs {
		var p point
		p.id = r.ID
		for _, a := range r.Archives {
			st.Archives++
			if a.Level == 0 {
				st.FullBytes += a.Compressed
			} else {
				st.IncrBytes += a.Compressed
			}
			p.bytes += a.Compressed
			if a.CreatedAt.After(p.at) {
				p.at = a.CreatedAt
			}
		}
		if len(r.Archives) > 0 {
			pts = append(pts, p)
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].id < pts[j].id })
	var cum int64
	for _, p := range pts {
		cum += p.bytes
		st.ByRun = append(st.ByRun, engine.UsagePoint{Run: p.id, At: p.at, Added: p.bytes, Used: cum})
	}
	if n := len(st.ByRun); n > 0 {
		st.First, st.Last = st.ByRun[0].At, st.ByRun[n-1].At
	}
	for _, s := range f.usage {
		if s.Medium == name {
			st.Usage = append(st.Usage, s)
		}
	}
	st.Growth = engine.UsageStats{Samples: len(st.Usage)}
	if n := len(st.Usage); n >= 2 {
		st.Growth.First, st.Growth.Last = st.Usage[0].At, st.Usage[n-1].At
	}
	return st, true
}

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

func (f fakeSource) StaleDLEs(now time.Time) []catalog.StaleDLE { return f.stale }

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
			if a.CreatedAt.After(s.LastBackupAt) {
				s.LastBackupAt = a.CreatedAt
			}
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
		{"/media", `href="/media/disk"`},                  // the list links to each medium's detail
		{"/media/disk", "run-2026-07-03.120000"},          // the per-run usage table links back to the run
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

func TestUnknownMediumIsFriendly(t *testing.T) {
	h := NewServer(sampleSource(), t.TempDir()).Handler()
	code, body := get(t, h, "/media/nope")
	if code != http.StatusOK {
		t.Fatalf("code=%d, want 200 (a rendered not-found page, not a raw error)", code)
	}
	if !strings.Contains(body, "No medium named") {
		t.Errorf("expected a friendly unknown-medium message, got:\n%s", body)
	}
}

// TestMediumDetailRendersUsageChart checks that the catalog's usage ledger (two or
// more samples, including a prune-driven decline) renders as the inline-SVG
// used-capacity-over-time chart — the true curve the retained archives alone could
// not draw.
func TestMediumDetailRendersUsageChart(t *testing.T) {
	src := sampleSource()
	// Growth then a prune reclaims some bytes — the decline the media cannot show.
	src.usage = []catalog.UsageSample{
		{At: time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC), Medium: "disk", Used: 200_000, Runs: 1},
		{At: time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC), Medium: "disk", Used: 260_000, Runs: 2},
		{At: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC), Medium: "disk", Used: 90_000, Runs: 1},
	}
	h := NewServer(src, t.TempDir()).Handler()

	code, body := get(t, h, "/media/disk")
	if code != http.StatusOK {
		t.Fatalf("code=%d, want 200", code)
	}
	for _, want := range []string{
		"Used capacity over time", // the chart section header
		"<svg",                    // the inline SVG chart itself
		"3 recorded samples",      // the history caption
		"90.00 kB",                // the final (post-prune) sample, drawn as the curve's end
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
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

// TestRunsPagination checks that /runs caps to the most recent maxListRows by
// default, offers a "show all" link once the catalog exceeds the cap, and that
// ?all=1 lifts the cap. A garbage query value must fall back to the capped view
// rather than erroring.
func TestRunsPagination(t *testing.T) {
	src := fakeSource{}
	for i := 0; i < maxListRows+5; i++ {
		src.runs = append(src.runs, &catalog.Run{ID: fmt.Sprintf("run-2026-07-%02d.120000", i+1)})
	}
	h := NewServer(src, t.TempDir()).Handler()

	code, body := get(t, h, "/runs")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if got := strings.Count(body, "<tr>") - 1; got != maxListRows { // -1 for the header row
		t.Errorf("/runs rendered %d rows, want capped to %d", got, maxListRows)
	}
	if !strings.Contains(body, `href="?all=1"`) {
		t.Errorf("/runs missing the show-all link when over the cap:\n%s", body)
	}

	_, all := get(t, h, "/runs?all=1")
	if got := strings.Count(all, "<tr>") - 1; got != maxListRows+5 {
		t.Errorf("/runs?all=1 rendered %d rows, want all %d", got, maxListRows+5)
	}

	code, garbage := get(t, h, "/runs?all=garbage")
	if code != http.StatusOK {
		t.Fatalf("/runs?all=garbage code=%d, want 200 (garbage query must not 500)", code)
	}
	if got := strings.Count(garbage, "<tr>") - 1; got != maxListRows {
		t.Errorf("/runs?all=garbage rendered %d rows, want capped to %d", got, maxListRows)
	}
}

// TestReportPagination mirrors TestRunsPagination for /report.
func TestReportPagination(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxListRows+3; i++ {
		if err := report.Append(dir, report.Run{
			Command: report.CommandDump, EndedAt: time.Now().Add(time.Duration(i) * time.Minute),
			Outcome: report.OutcomeSuccess,
		}); err != nil {
			t.Fatal(err)
		}
	}
	h := NewServer(fakeSource{}, dir).Handler()

	_, body := get(t, h, "/report")
	if got := strings.Count(body, "<tr>") - 1; got != maxListRows {
		t.Errorf("/report rendered %d rows, want capped to %d", got, maxListRows)
	}
	if !strings.Contains(body, `href="?all=1"`) {
		t.Errorf("/report missing the show-all link when over the cap:\n%s", body)
	}

	_, all := get(t, h, "/report?all=1")
	if got := strings.Count(all, "<tr>") - 1; got != maxListRows+3 {
		t.Errorf("/report?all=1 rendered %d rows, want all %d", got, maxListRows+3)
	}
}

// TestCrossLinks checks the cross-linking added between related pages: a live
// /status per-DLE row links to its /dles/<slug> page, and a run's archive links back
// to its DLE too.
func TestCrossLinks(t *testing.T) {
	dir := t.TempDir()
	// The snapshot names the DLE by its host:path identity (progress.DLE.Name) but
	// carries the internal slug separately — the /dles link must use the slug, not
	// the unescaped host:path.
	progress.NewFileSink(dir, time.Now)(progress.Snapshot{
		RunID: "run-2026-07-03.130000", Phase: progress.PhaseRunning, Workers: 1,
		DLEs: []progress.DLE{{Name: "localhost:/src", Slug: "local", State: progress.StateDumping, EstBytes: 1000, DoneBytes: 500}},
	}, true)
	h := NewServer(sampleSource(), dir).Handler()

	if _, body := get(t, h, "/status"); !strings.Contains(body, `href="/dles/local"`) {
		t.Errorf("/status per-DLE row missing link to /dles/local:\n%s", body)
	} else if strings.Contains(body, `href="/dles/localhost:/src"`) {
		t.Errorf("/status per-DLE row linked to the unsluggified host:path:\n%s", body)
	}
	if _, body := get(t, h, "/runs/run-2026-07-03.120000"); !strings.Contains(body, `href="/dles/local"`) {
		t.Errorf("/runs/<id> archive missing link to its DLE:\n%s", body)
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
	for _, p := range []string{"/", "/runs", "/dles", "/media", "/drills", "/report", "/status", "/metrics"} {
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

// TestEstimatingStatusShowsSizingNotDumpTable guards against the estimating phase
// rendering the dump view: during sizing a "done" DLE is merely measured and
// DoneBytes is its estimate, so the dump table would misread as a previous run's
// results. /status and the home banner must show the sizing view instead.
func TestEstimatingStatusShowsSizingNotDumpTable(t *testing.T) {
	dir := t.TempDir()
	progress.NewFileSink(dir, time.Now)(progress.Snapshot{
		RunID: "estimate", Phase: progress.PhaseEstimating, Workers: 1,
		DLEs: []progress.DLE{
			{Name: "local", State: progress.StateDone, DoneBytes: 4096}, // sized
			{Name: "other", State: progress.StatePending},               // not yet
		},
	}, true)
	h := NewServer(sampleSource(), dir).Handler()

	code, body := get(t, h, "/status")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(body, "1 of 2 DLE(s) measured") || !strings.Contains(body, "so far") {
		t.Errorf("/status while estimating missing the sizing view:\n%s", body)
	}
	if strings.Contains(body, "Per-DLE") || strings.Contains(body, "dump ·") {
		t.Errorf("/status while estimating leaked the dump table (sized DLEs read as done dumps):\n%s", body)
	}

	if _, body := get(t, h, "/"); !strings.Contains(body, "sizing 1 of 2 DLE(s)") {
		t.Errorf("home banner while estimating missing the sizing line:\n%s", body)
	}
}

// TestHomeRollupRedFlags seeds one instance of each broken-thing the "attention
// needed" rollup surfaces — a failed run, a failing drill, a stale DLE, and a medium
// over capacity — and asserts each renders as an alert (and the all-clear does not).
func TestHomeRollupRedFlags(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	if err := report.Append(dir, report.Run{
		Command: report.CommandDump, Outcome: report.OutcomeFailure, ExitClass: "dump-failed",
		Error: "tar exited 2", RunID: "run-2026-07-05.020000",
		StartedAt: now.Add(-time.Hour), EndedAt: now.Add(-time.Hour).Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	ledger := &drill.Ledger{}
	ledger.Update(drill.Record{
		DLE: "local", LastDrill: now.Add(-24 * time.Hour), Tier: "chain", Medium: "disk",
		OK: false, Class: "pipeline", Detail: "gpg: decryption failed",
	})
	if err := ledger.Save(dir); err != nil {
		t.Fatal(err)
	}

	src := sampleSource() // exposes DLE "local" and medium "disk"
	src.stale = []catalog.StaleDLE{{DLE: "local", Display: "localhost:/src", LastBackup: now.Add(-72 * time.Hour)}}
	src.media = append(src.media, engine.MediumInfo{Name: "vault", Type: "disk", Used: 100, Capacity: 50})

	_, body := get(t, NewServer(src, dir).Handler(), "/")
	for _, want := range []string{
		"last dump failed",                 // failed-run alert
		"dump-failed",                      // exit-class detail
		"recovery drill failing for local", // failing drill (DisplayDLE)
		"last backed up",                   // stale DLE (localhost:/src, 3d ago)
		"vault is full",                    // medium over capacity
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/ rollup missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "all clear") {
		t.Errorf("/ rollup shows the all-clear line despite red flags:\n%s", body)
	}
}

// TestHomeRollupAllClear checks that a healthy catalog — a successful last run, no
// configured DLEs (so no drill/stale gaps), no bounded medium over capacity — shows
// the single quiet all-clear line and no alert rows.
func TestHomeRollupAllClear(t *testing.T) {
	dir := t.TempDir()
	if err := report.Append(dir, report.Run{
		Command: report.CommandDump, Outcome: report.OutcomeSuccess, RunID: "run-2026-07-05.020000",
		StartedAt: time.Now().Add(-time.Hour), EndedAt: time.Now().Add(-time.Hour).Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, NewServer(fakeSource{}, dir).Handler(), "/")
	if !strings.Contains(body, "all clear") {
		t.Errorf("/ rollup missing the all-clear line on a healthy catalog:\n%s", body)
	}
	if strings.Contains(body, `class="alert bad"`) || strings.Contains(body, `class="alert warn"`) {
		t.Errorf("/ rollup shows alert rows on a healthy catalog:\n%s", body)
	}
}

// TestMetrics exercises the /metrics exposition: family headers, per-command run
// gauges, per-DLE freshness, drill/stale counts, medium capacity (omitted when
// unbounded), and Prometheus label-value escaping.
func TestMetrics(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := report.Append(dir, report.Run{
		Command: report.CommandDump, Outcome: report.OutcomeSuccess, RunID: "run-2026-07-05.020000",
		StartedAt: now.Add(-10 * time.Minute), EndedAt: now.Add(-8 * time.Minute), // 120s
	}); err != nil {
		t.Fatal(err)
	}
	if err := report.Append(dir, report.Run{
		Command: report.CommandSync, Outcome: report.OutcomeFailure, ExitClass: "sync-error",
		StartedAt: now.Add(-5 * time.Minute), EndedAt: now.Add(-4 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	src := sampleSource() // DLE "local" (last-backup set); medium "disk" 200kB/10GiB
	src.stale = []catalog.StaleDLE{{DLE: "local"}}
	src.media = append(src.media, engine.MediumInfo{Name: `a"b\c`, Type: "disk", Used: 1}) // unbounded + escapable name

	code, body := get(t, NewServer(src, dir).Handler(), "/metrics")
	if code != http.StatusOK {
		t.Fatalf("code=%d, want 200", code)
	}
	for _, want := range []string{
		"# HELP nbackup_last_run_success",
		"# TYPE nbackup_last_run_success gauge",
		`nbackup_last_run_success{command="dump"} 1`,
		`nbackup_last_run_success{command="sync"} 0`,
		`nbackup_last_run_timestamp_seconds{command="dump"} `,
		`nbackup_last_run_duration_seconds{command="dump"} 120`,
		`nbackup_dle_last_backup_timestamp_seconds{dle="local"} `,
		"nbackup_dle_count 1",
		"nbackup_dle_stale_count 1",
		"nbackup_drill_overdue_count 1", // "local" is never drilled
		"nbackup_drill_failing_count 0",
		`nbackup_medium_used_bytes{medium="disk"} 200000`,
		`nbackup_medium_capacity_bytes{medium="disk"} `,
		`nbackup_medium_used_bytes{medium="a\"b\\c"} 1`, // label escaping
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q\n---\n%s", want, body)
		}
	}
	// An unbounded medium emits no capacity series (0 = unbounded, omitted).
	if strings.Contains(body, `nbackup_medium_capacity_bytes{medium="a\"b\\c"}`) {
		t.Errorf("/metrics emitted a capacity series for an unbounded medium:\n%s", body)
	}
	// Content type carries the exposition-format version.
	if ct := metricsContentType(t, NewServer(src, dir).Handler()); !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("/metrics Content-Type = %q, want version=0.0.4", ct)
	}
}

func metricsContentType(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Header().Get("Content-Type")
}
