package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/accounting"
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
	runs       []*catalog.Run
	media      []engine.MediumInfo
	usage      []catalog.UsageSample          // the canned ledger the medium page's chart draws
	stale      []catalog.StaleDLE             // overdue DLEs against the dump cycle
	placements map[string][]catalog.Placement // per-run placements; nil falls back to a single "disk" copy
	// perVolume/poolVolumes canning a labeled pool's inventory directly (rather than
	// deriving it from placements+labels, as accounting.perVolume does) — MediumStats
	// below fabricates its picture straight from the fake's fields, so a pool fixture
	// just sets these to exercise the web layer's pool-aware rendering in isolation.
	perVolume   map[string][]engine.VolumeUsage
	poolVolumes map[string]int64
	// protected cans the residual a prune cannot reclaim per medium (the accounting
	// package's own MediumProtected has its own unit tests; the web layer only needs
	// a canned reading to exercise the rollup's threshold logic in isolation). A
	// medium absent from this map reports ok=false, as an unknown medium would.
	protected map[string]int64
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
	if f.placements != nil {
		return f.placements[runID] // an unknown run has no copy, on purpose (broken-chain fixtures)
	}
	return []catalog.Placement{{Medium: "disk"}}
}

// heldOn builds a placement on a medium that holds each of the given archives — the
// recovery-point fixtures need Holds()/placementName() to see a real copy (the default
// single "disk" placement records no archives, so it holds nothing).
func heldOn(medium string, archs ...record.Archive) catalog.Placement {
	p := catalog.Placement{Medium: medium}
	for _, a := range archs {
		p.Archives = append(p.Archives, catalog.PlacedArchive{DLE: a.DLE, Level: a.Level})
	}
	return p
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
	if name == "disk" { // the fake places every archive on "disk"; other media hold none
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
	}
	// The recorded usage ledger (ByRun's usage-samples counterpart) is not tied to
	// "disk" — a test can seed growth/projection samples for any named medium.
	for _, s := range f.usage {
		if s.Medium == name {
			st.Usage = append(st.Usage, s)
		}
	}
	st.Growth = accounting.Summarize(st.Usage, info.Capacity)
	st.PerVolume = f.perVolume[name]
	st.PoolVolumes = f.poolVolumes[name]
	return st, true
}

// MediumProtected returns the canned residual for name (see the protected field);
// ok is false for a medium the test never seeded, matching an unknown medium.
func (f fakeSource) MediumProtected(name string, now time.Time) (residual, capacity int64, ok bool) {
	residual, ok = f.protected[name]
	if !ok {
		return 0, 0, false
	}
	for _, m := range f.media {
		if m.Name == name {
			capacity = m.Capacity
		}
	}
	return residual, capacity, true
}

// DisplayDLE resolves slug to the host:path identity of any archive of it seen in
// the fake's runs (mirroring engine.DisplayDLE closely enough for host-grouping
// tests), falling back to the bare slug for a DLE the fake has no archive for.
func (f fakeSource) DisplayDLE(slug string) string {
	for _, r := range f.runs {
		for _, a := range r.Archives {
			if a.DLE == slug {
				return a.DLEID()
			}
		}
	}
	return slug
}

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

// TestStatusGroupsByState checks the active-first per-DLE layout: full cards (with
// progress bars) only for in-flight DLEs, and the failed/done/pending majority as
// compact rows so a many-DLE run is not a wall of static bars.
func TestStatusGroupsByState(t *testing.T) {
	dir := t.TempDir()
	progress.NewFileSink(dir, time.Now)(progress.Snapshot{
		RunID: "run-2026-07-06.100000", Phase: progress.PhaseRunning, Workers: 2,
		DLEs: []progress.DLE{
			{Name: "dumping-dle", Slug: "d1", State: progress.StateDumping, EstBytes: 1000, DoneBytes: 500},
			{Name: "flushing-dle", Slug: "d2", State: progress.StateFlushing, Holding: "scratch", OutBytes: 400, DrainBytes: 200},
			{Name: "done-dle", Slug: "d3", State: progress.StateDone, DoneBytes: 800},
			{Name: "failed-dle", Slug: "d4", State: progress.StateFailed, Err: "tar exited 2"},
			{Name: "pending-dle", Slug: "d5", State: progress.StatePending},
		},
	}, true)

	code, body := get(t, NewServer(sampleSource(), dir).Handler(), "/status")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{
		"Active (2)", "Failed (1)", "Done (1)", "Pending (1)", // section headers with counts
		"tar exited 2",            // the failed DLE's error text
		"done-dle", "pending-dle", // done and pending DLE names present
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/status missing %q:\n%s", want, body)
		}
	}
	// Progress bars render only for the active DLEs: 1 run-level + 2 active dump bars +
	// 1 flush bar (the flushing DLE) = 4; the done/failed/pending rows carry none.
	if got := strings.Count(body, `class="bar"`); got != 4 {
		t.Errorf("/status rendered %d progress bars, want 4 (1 run-level + 2 dump + 1 flush)", got)
	}
}

// TestRunDumpReport checks the /runs/<id> dump-report section: the STATISTICS grid
// and a per-DLE row render when the history holds a dump record for that run, and
// the section is omitted (no false wall of dashes) when it doesn't.
func TestRunDumpReport(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	if err := report.Append(dir, report.Run{
		Command: report.CommandDump, RunID: "run-2026-07-03.120000",
		StartedAt: start, EndedAt: start.Add(90 * time.Second),
		Outcome: report.OutcomeSuccess, Archives: 1, BytesMoved: 200_000,
		DumpStats: []report.DLEStat{
			{DLE: "local", Host: "localhost", Path: "/src", Level: 0, Orig: 1_000_000, Out: 200_000, Files: 2, Seconds: 90},
		},
	}); err != nil {
		t.Fatal(err)
	}
	h := NewServer(sampleSource(), dir).Handler()

	code, body := get(t, h, "/runs/run-2026-07-03.120000")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{
		"STATISTICS",
		"DLEs dumped",
		"localhost:/src",
		`href="/dles/local"`,
		"1.00 MB", // original size, formatted like the CLI
		"200.00 kB",
		"20%", // compression: 200k out of 1M orig
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/runs/<id> missing %q in dump report:\n%s", want, body)
		}
	}

	// A run with no dump-report record (predates the run-log, or was compacted out)
	// simply omits the section — the existing archives list already shows sizes.
	_, noRecord := get(t, h, "/runs/run-with-no-history-2026-07-03.120000")
	if strings.Contains(noRecord, "STATISTICS") {
		t.Errorf("/runs/<id> rendered a dump-report section with no history record:\n%s", noRecord)
	}
}

// TestStatusFlushLanes checks the per-landing flush itemization: a two-landing
// snapshot gets one Flush row per landing, and a single-landing snapshot keeps
// today's one aggregate row.
func TestStatusFlushLanes(t *testing.T) {
	twoLane := func(dir string) {
		progress.NewFileSink(dir, time.Now)(progress.Snapshot{
			RunID: "run-2026-07-06.110000", Phase: progress.PhaseRunning, Workers: 1,
			DrainStartedAt: time.Now().Add(-time.Minute),
			DLEs: []progress.DLE{{
				Name: "fanout-dle", Slug: "d1", State: progress.StateFlushing, Holding: "scratch",
				Landings: []string{"s3", "gdrive"}, OutBytes: 1000,
				Drained: map[string]int64{"s3": 300, "gdrive": 100},
			}},
		}, true)
	}
	dir := t.TempDir()
	twoLane(dir)
	_, body := get(t, NewServer(sampleSource(), dir).Handler(), "/status")
	if got := strings.Count(body, ">Flush<"); got != 2 {
		t.Errorf("/status rendered %d Flush rows for a two-landing snapshot, want 2:\n%s", got, body)
	}
	for _, want := range []string{"s3 ·", "gdrive ·", "30%", "10%"} {
		if !strings.Contains(body, want) {
			t.Errorf("/status missing %q in the per-landing flush lanes:\n%s", want, body)
		}
	}

	oneLane := t.TempDir()
	progress.NewFileSink(oneLane, time.Now)(progress.Snapshot{
		RunID: "run-2026-07-06.120000", Phase: progress.PhaseRunning, Workers: 1,
		DrainStartedAt: time.Now().Add(-time.Minute),
		DLEs: []progress.DLE{{
			Name: "single-dle", Slug: "d2", State: progress.StateFlushing, Holding: "scratch",
			OutBytes: 1000, DrainBytes: 400,
		}},
	}, true)
	_, single := get(t, NewServer(sampleSource(), oneLane).Handler(), "/status")
	if got := strings.Count(single, ">Flush<"); got != 1 {
		t.Errorf("/status rendered %d Flush rows for a single-landing snapshot, want 1 (the aggregate):\n%s", got, single)
	}
}

// TestReportDetailAndDuration checks the /report enrichment: a per-command detail
// cell (mirroring report.detailCell) and a Duration column.
func TestReportDetailAndDuration(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	if err := report.Append(dir, report.Run{
		Command: report.CommandSync, StartedAt: start, EndedAt: start.Add(2 * time.Minute),
		Outcome: report.OutcomeSuccess, RunsCopied: 2, BytesMoved: 50_000,
	}); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, NewServer(fakeSource{}, dir).Handler(), "/report")
	for _, want := range []string{"2 run(s) copied", "2m"} {
		if !strings.Contains(body, want) {
			t.Errorf("/report missing %q:\n%s", want, body)
		}
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

// dumpAt appends a dump record with one DLE's statistics to the run history at the
// given time, for the trend-chart and anomaly tests below.
func dumpAt(t *testing.T, dir string, at time.Time, dle string, level int, orig int64) {
	t.Helper()
	if err := report.Append(dir, report.Run{
		Command: report.CommandDump, RunID: "run-" + at.Format("20060102.150405"),
		StartedAt: at, EndedAt: at.Add(time.Minute), Outcome: report.OutcomeSuccess,
		DumpStats: []report.DLEStat{{DLE: dle, Level: level, Orig: orig, Out: orig / 5, Seconds: 60}},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDLETrendChart checks the /dles/<slug> size-trend chart: it renders once the
// history holds two or more dump records for the DLE, and is omitted (no empty
// section) with fewer than two.
func TestDLETrendChart(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	dumpAt(t, dir, base, "local", 0, 1_000_000)
	dumpAt(t, dir, base.Add(24*time.Hour), "local", 1, 200_000)

	code, body := get(t, NewServer(sampleSource(), dir).Handler(), "/dles/local")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(body, "Size trend") || !strings.Contains(body, "<svg") {
		t.Errorf("/dles/local missing the trend chart with 2 dump records:\n%s", body)
	}

	// Fewer than two dump records for the DLE: the section is omitted entirely.
	_, single := get(t, NewServer(sampleSource(), t.TempDir()).Handler(), "/dles/local")
	if strings.Contains(single, "Size trend") || strings.Contains(single, "<svg") {
		t.Errorf("/dles/local rendered a trend chart with fewer than 2 dump records:\n%s", single)
	}
}

// TestHomeRollupSizeAnomaly checks the size-anomaly nudge: a DLE dumping 3x the
// median of 3 same-level priors (well past the 64 MiB noise floor) warns and links
// to the DLE; a small (10%) deviation from a similar baseline does not.
func TestHomeRollupSizeAnomaly(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-96 * time.Hour)
	for i, sz := range []int64{100_000_000, 105_000_000, 99_000_000, 350_000_000} {
		dumpAt(t, dir, base.Add(time.Duration(i)*24*time.Hour), "local", 0, sz)
	}
	_, body := get(t, NewServer(sampleSource(), dir).Handler(), "/")
	for _, want := range []string{"size anomaly", "dumped", "typically", `href="/dles/local"`} {
		if !strings.Contains(body, want) {
			t.Errorf("/ rollup missing %q for a 3x size deviation:\n%s", want, body)
		}
	}

	smallDir := t.TempDir()
	for i, sz := range []int64{100_000_000, 105_000_000, 99_000_000, 110_000_000} {
		dumpAt(t, smallDir, base.Add(time.Duration(i)*24*time.Hour), "local", 0, sz)
	}
	_, small := get(t, NewServer(sampleSource(), smallDir).Handler(), "/")
	if strings.Contains(small, "size anomaly") {
		t.Errorf("/ rollup emitted a size anomaly for a 10%% deviation:\n%s", small)
	}
}

// TestHomeRollupCapacityForesight checks the capacity nudges for an address-identified
// medium (disk, s3): a 95%-used medium with a small protected (unreclaimable) set
// stays quiet — raw Used sitting near capacity is the planner/prune steady state, not
// a problem — while the same medium with a protected set at >=90% of capacity warns,
// since pruning genuinely cannot free enough there. A medium whose recorded growth
// projects filling within 30 days still warns with the projected day count either way.
func TestHomeRollupCapacityForesight(t *testing.T) {
	dir := t.TempDir()

	quiet := sampleSource()
	quiet.media = append(quiet.media, engine.MediumInfo{Name: "near-full", Type: "disk", Used: 95, Capacity: 100})
	quiet.protected = map[string]int64{"near-full": 5}
	_, qbody := get(t, NewServer(quiet, dir).Handler(), "/")
	if strings.Contains(qbody, "near-full: retention needs") || strings.Contains(qbody, "near-full is at") {
		t.Errorf("/ rollup warned on a 95%%-used medium whose protected set is small:\n%s", qbody)
	}

	src := sampleSource()
	src.media = append(src.media, engine.MediumInfo{Name: "near-full", Type: "disk", Used: 95, Capacity: 100})
	src.protected = map[string]int64{"near-full": 92}
	_, body := get(t, NewServer(src, dir).Handler(), "/")
	if !strings.Contains(body, "near-full: retention needs 92 B of 100 B") {
		t.Errorf("/ rollup missing the protected-set warn:\n%s", body)
	}

	now := time.Now()
	forecast := sampleSource()
	forecast.media = append(forecast.media, engine.MediumInfo{Name: "vault", Type: "disk", Used: 700_000_000, Capacity: 1_000_000_000})
	forecast.usage = []catalog.UsageSample{
		{At: now.Add(-10 * 24 * time.Hour), Medium: "vault", Used: 500_000_000, Runs: 1},
		{At: now, Medium: "vault", Used: 700_000_000, Runs: 2},
	}
	_, fbody := get(t, NewServer(forecast, dir).Handler(), "/")
	if !strings.Contains(fbody, "vault projected full in ~15d") {
		t.Errorf("/ rollup missing the growth-projection warn:\n%s", fbody)
	}
}

// TestHomeRollupOverCapacityReclaimable checks the red "over capacity" alert's
// reclaimable-bytes hint: it appends how much a prune could free when the protected
// residual leaves room to reclaim, and stays unchanged (no false promise) when the
// protected set is the entire used total — the truly stuck case a prune can't help.
func TestHomeRollupOverCapacityReclaimable(t *testing.T) {
	dir := t.TempDir()

	src := sampleSource()
	src.media = append(src.media, engine.MediumInfo{Name: "vault", Type: "disk", Used: 100, Capacity: 50})
	src.protected = map[string]int64{"vault": 69} // 100 - 69 = 31 reclaimable
	_, body := get(t, NewServer(src, dir).Handler(), "/")
	if !strings.Contains(body, "vault is full — 100 B of 50 B used — 31 B reclaimable, run nb prune") {
		t.Errorf("/ rollup missing the reclaimable hint on an over-capacity medium:\n%s", body)
	}

	stuck := sampleSource()
	stuck.media = append(stuck.media, engine.MediumInfo{Name: "vault", Type: "disk", Used: 100, Capacity: 50})
	stuck.protected = map[string]int64{"vault": 100} // nothing reclaimable
	_, sbody := get(t, NewServer(stuck, dir).Handler(), "/")
	if !strings.Contains(sbody, "vault is full — 100 B of 50 B used") {
		t.Errorf("/ rollup missing the base over-capacity text:\n%s", sbody)
	}
	if strings.Contains(sbody, "reclaimable") {
		t.Errorf("/ rollup claimed reclaimable bytes when the protected set is the whole total:\n%s", sbody)
	}
}

// recoverySource builds a two-run fixture for the recovery-points tests: a full in run
// A and an L1 in run B based on it, each held on "disk" by default.
func recoverySource() (fakeSource, record.Archive, record.Archive) {
	full := record.Archive{
		Run: "run-2026-07-01.120000", DLE: "local", Host: "localhost", Path: "/src",
		Level: 0, Compressed: 200_000, FileCount: 2, Compress: "none", Encrypt: "none",
		CreatedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
	incr := record.Archive{
		Run: "run-2026-07-02.120000", DLE: "local", Host: "localhost", Path: "/src",
		Level: 1, BaseRun: full.Run, Compressed: 40_000, FileCount: 1, Compress: "none", Encrypt: "none",
		CreatedAt: time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	}
	src := fakeSource{
		runs: []*catalog.Run{
			{ID: full.Run, Archives: []record.Archive{full}},
			{ID: incr.Run, Archives: []record.Archive{incr}},
		},
		placements: map[string][]catalog.Placement{
			full.Run: {heldOn("disk", full)},
			incr.Run: {heldOn("disk", incr)},
		},
	}
	return src, full, incr
}

// TestRecoveryPointsComplete checks that an L1 point whose full base is held renders a
// COMPLETE chain naming the base full, restorable from the one medium holding it all.
func TestRecoveryPointsComplete(t *testing.T) {
	src, full, _ := recoverySource()
	_, body := get(t, NewServer(src, t.TempDir()).Handler(), "/dles/local")
	for _, want := range []string{
		"Recovery points",
		"L1 ← full " + full.Run, // the chain, tip-first, naming the base full's run
		"complete",              // chain health
		"restore from disk",     // the whole chain on one medium
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/dles/local recovery points missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "broken") {
		t.Errorf("/dles/local marked a held chain broken:\n%s", body)
	}
}

// TestRecoveryPointsBroken drops the full's copy: the L1 point can no longer be
// restored (its base has no surviving copy), so the point renders BROKEN and the home
// rollup carries the red "cannot restore … to its latest point" alert.
func TestRecoveryPointsBroken(t *testing.T) {
	src, full, _ := recoverySource()
	src.placements[full.Run] = nil // the base full exists in the catalog but has no copy

	_, body := get(t, NewServer(src, t.TempDir()).Handler(), "/dles/local")
	if !strings.Contains(body, "broken") || !strings.Contains(body, "base "+full.Run+" has no copy") {
		t.Errorf("/dles/local did not mark the point broken with the missing-copy reason:\n%s", body)
	}

	_, home := get(t, NewServer(src, t.TempDir()).Handler(), "/")
	if !strings.Contains(home, "cannot restore localhost:/src to its latest point") {
		t.Errorf("home rollup missing the broken-latest-point alert:\n%s", home)
	}
}

// TestRecoveryPointsDrilled checks the drilled badge: a ledger record whose RunID is
// exactly this point (and OK) marks the point drilled and, being the newest point,
// shows the tier gloss.
func TestRecoveryPointsDrilled(t *testing.T) {
	src, _, incr := recoverySource()
	dir := t.TempDir()
	ledger := &drill.Ledger{}
	ledger.Update(drill.Record{
		DLE: "local", LastDrill: time.Now().Add(-24 * time.Hour), Tier: "chain",
		Medium: "disk", AsOf: "2026-07-02", RunID: incr.Run, OK: true,
	})
	if err := ledger.Save(dir); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, NewServer(src, dir).Handler(), "/dles/local")
	if !strings.Contains(body, "drilled") {
		t.Errorf("/dles/local missing the drilled badge for the drilled point:\n%s", body)
	}
	if !strings.Contains(body, tierWhat("chain")) {
		t.Errorf("/dles/local newest drilled point missing the tier gloss:\n%s", body)
	}
}

// TestRecoveryPointsCycleDoesNotHang guards against a corrupted catalog whose BaseRun
// links point back on themselves: run-a's base is run-b and run-b's base is run-a.
// Without cycle detection, recoveryChain's map walk never reaches a level-0 archive
// or a missing base, so it loops forever — and since brokenLatestPoints calls
// recoveryPoints for every configured DLE on every home-page render, one corrupted
// DLE would hang every request. The request runs on a background goroutine with a
// timeout so a regression fails the test instead of hanging it.
func TestRecoveryPointsCycleDoesNotHang(t *testing.T) {
	a := record.Archive{
		Run: "run-2026-07-02.120000", DLE: "local", Host: "localhost", Path: "/src",
		Level: 1, BaseRun: "run-2026-07-01.120000", Compressed: 40_000,
		CreatedAt: time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	}
	b := record.Archive{
		Run: "run-2026-07-01.120000", DLE: "local", Host: "localhost", Path: "/src",
		Level: 1, BaseRun: a.Run, Compressed: 40_000,
		CreatedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
	src := fakeSource{runs: []*catalog.Run{
		{ID: a.Run, Archives: []record.Archive{a}},
		{ID: b.Run, Archives: []record.Archive{b}},
	}}

	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	go func() {
		code, body := get(t, NewServer(src, t.TempDir()).Handler(), "/dles/local")
		done <- result{code, body}
	}()
	select {
	case r := <-done:
		if r.code != http.StatusOK {
			t.Fatalf("code=%d", r.code)
		}
		if !strings.Contains(r.body, "broken") || !strings.Contains(r.body, "cycles back to run") {
			t.Errorf("/dles/local did not mark a cyclic base chain broken:\n%s", r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recoveryPoints hung on a cyclic BaseRun chain (run-a <-> run-b)")
	}
}

// TestActivityHeatmap checks the /dles matrix: archives on distinct days render filled
// cells with the right class (full vs incremental), a partial archive gets the warn
// class, and an empty catalog omits the section entirely.
func TestActivityHeatmap(t *testing.T) {
	now := time.Now()
	day := func(d int) time.Time { return now.AddDate(0, 0, -d) }
	mk := func(run string, level int, unreadable int, at time.Time) record.Archive {
		return record.Archive{
			Run: run, DLE: "local", Host: "localhost", Path: "/src", Level: level,
			Compressed: 100_000, Unreadable: unreadable, CreatedAt: at,
		}
	}
	src := fakeSource{runs: []*catalog.Run{
		{ID: "run-a", Archives: []record.Archive{mk("run-a", 0, 0, day(4))}}, // full
		{ID: "run-b", Archives: []record.Archive{mk("run-b", 1, 0, day(2))}}, // incremental
		{ID: "run-c", Archives: []record.Archive{mk("run-c", 1, 3, day(1))}}, // partial (warn wins)
	}}

	code, body := get(t, NewServer(src, t.TempDir()).Handler(), "/dles")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{
		"Activity",
		`class="cell full"`,
		`class="cell incr"`,
		`class="cell partial"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/dles heatmap missing %q:\n%s", want, body)
		}
	}

	// Empty catalog: no heatmap section at all.
	_, empty := get(t, NewServer(fakeSource{}, t.TempDir()).Handler(), "/dles")
	if strings.Contains(empty, "Activity") || strings.Contains(empty, `class="heat"`) {
		t.Errorf("/dles rendered a heatmap for an empty catalog:\n%s", empty)
	}
}

// TestMediaPageShowsForecastColumn checks that /media renders the utilization bar
// and projected-full column for a bounded medium with recorded growth.
func TestMediaPageShowsForecastColumn(t *testing.T) {
	now := time.Now()
	src := fakeSource{
		media: []engine.MediumInfo{{Name: "vault", Type: "disk", Used: 700_000_000, Capacity: 1_000_000_000}},
		usage: []catalog.UsageSample{
			{At: now.Add(-10 * 24 * time.Hour), Medium: "vault", Used: 500_000_000, Runs: 1},
			{At: now, Medium: "vault", Used: 700_000_000, Runs: 2},
		},
	}
	code, body := get(t, NewServer(src, t.TempDir()).Handler(), "/media")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{`class="bar"`, "~15d"} {
		if !strings.Contains(body, want) {
			t.Errorf("/media missing %q:\n%s", want, body)
		}
	}
}

// poolMediumInfo is the MediumInfo shape accounting.Medium would report for a
// labeled pool: Volumes > 0 is the only signal the web layer keys pool treatment
// on (never medium type), so a fixture using a plain "tape" type but a nonzero
// Volumes count exercises that neutrality directly.
func poolMediumInfo(name string, used, capacity int64, volumes int) engine.MediumInfo {
	return engine.MediumInfo{Name: name, Type: "tape", Used: used, Capacity: capacity, Volumes: volumes}
}

// TestMediumPagePoolInventory checks a labeled pool's detail page: two full
// volumes and one with room render the per-volume table (labels, fill bars) and
// the "1 of 3 with room" summary, while the aggregate 90%-used/projected-full
// warnings are suppressed even though the pool is at 95% aggregate.
func TestMediumPagePoolInventory(t *testing.T) {
	src := fakeSource{
		media: []engine.MediumInfo{poolMediumInfo("vault", 950_000, 1_000_000, 3)},
		perVolume: map[string][]engine.VolumeUsage{
			"vault": {
				{Label: "lto-01", Epoch: 2, Barcode: "BC001", Bytes: 500_000, Capacity: 500_000, Runs: 3, Archives: 5, HasRoom: false},
				{Label: "lto-02", Epoch: 1, Barcode: "BC002", Bytes: 450_000, Capacity: 500_000, Runs: 2, Archives: 3, HasRoom: false},
				{Label: "lto-03", Epoch: 1, Barcode: "BC003", Bytes: 0, Capacity: 500_000, Runs: 0, Archives: 0, HasRoom: true},
			},
		},
		poolVolumes: map[string]int64{"vault": 3},
		usage: []catalog.UsageSample{ // would otherwise trip both the 90% and forecast warns
			{At: time.Now().Add(-10 * 24 * time.Hour), Medium: "vault", Used: 500_000, Runs: 1},
			{At: time.Now(), Medium: "vault", Used: 950_000, Runs: 2},
		},
	}
	code, body := get(t, NewServer(src, t.TempDir()).Handler(), "/media/vault")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"lto-01", "lto-02", "lto-03", "BC001", "2 of 3 labeled", "1 with room", `class="bar"`, "pool capacity"} {
		if !strings.Contains(body, want) {
			t.Errorf("/media/vault missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"over capacity", "full ~"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("/media/vault pool page shows aggregate-capacity language %q:\n%s", unwanted, body)
		}
	}

	_, listBody := get(t, NewServer(src, t.TempDir()).Handler(), "/media")
	if !strings.Contains(listBody, "1 of 3 with room") {
		t.Errorf("/media list missing pool room summary:\n%s", listBody)
	}

	// The home rollup must not fire the aggregate 90%/projection warn for a pool
	// even though it sits at 95% aggregate — only the pool-native alert (one
	// volume with room, tested separately below) is expected.
	_, homeBody := get(t, NewServer(src, t.TempDir()).Handler(), "/")
	if strings.Contains(homeBody, "at 95% capacity") || strings.Contains(homeBody, "projected full") {
		t.Errorf("/ rollup fired the aggregate capacity warn for a pool medium:\n%s", homeBody)
	}
	if !strings.Contains(homeBody, "vault: last volume with room") {
		t.Errorf("/ rollup missing the pool last-volume-with-room warn:\n%s", homeBody)
	}
}

// TestMediumPageNonAppendableUsedVsFull checks the volume pill vocabulary for a
// non-appendable pool (one run per volume — accounting.volumeHasRoom reports no
// room for any reel already holding a run, regardless of how few bytes it used).
// A 30%-filled reel with one run must render "used" with no red fill bar (the
// rotation working as designed, not an error), while a reel whose bytes actually
// reached capacity still renders "full" with the red bar.
func TestMediumPageNonAppendableUsedVsFull(t *testing.T) {
	src := fakeSource{
		media: []engine.MediumInfo{poolMediumInfo("vault", 650_000, 1_000_000, 2)},
		perVolume: map[string][]engine.VolumeUsage{
			"vault": {
				{Label: "lto-01", Barcode: "BC001", Bytes: 150_000, Used: 150_000, Capacity: 500_000, Runs: 1, Archives: 1, HasRoom: false},
				{Label: "lto-02", Barcode: "BC002", Bytes: 500_000, Used: 500_000, Capacity: 500_000, Runs: 1, Archives: 1, HasRoom: false},
			},
		},
		poolVolumes: map[string]int64{"vault": 2},
	}
	code, body := get(t, NewServer(src, t.TempDir()).Handler(), "/media/vault")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(body, `<span class="pill dim">used</span>`) {
		t.Errorf("/media/vault missing the \"used\" pill for a non-appendable 30%%-filled reel:\n%s", body)
	}
	if !strings.Contains(body, `<span class="pill dim">full</span>`) {
		t.Errorf("/media/vault missing the \"full\" pill for the byte-full reel:\n%s", body)
	}
	if strings.Contains(body, "lto-01") && strings.Contains(body[strings.Index(body, "lto-01"):strings.Index(body, "lto-02")], "background:var(--bad)") {
		t.Errorf("/media/vault renders a red fill bar for a merely-used (not byte-full) reel:\n%s", body)
	}
}

// TestHomeRollupPoolNoRoomUnderAggregateCapacity is the red pool alert: every
// labeled volume is full (none has room), fired instead of the aggregate warns.
// the pool's aggregate Used is comfortably under Capacity (so the over-capacity
// and 90%/projection checks stay silent), but every labeled volume is full.
func TestHomeRollupPoolNoRoomUnderAggregateCapacity(t *testing.T) {
	src := fakeSource{
		media: []engine.MediumInfo{poolMediumInfo("vault", 100, 1_000_000, 2)},
		perVolume: map[string][]engine.VolumeUsage{
			"vault": {
				{Label: "lto-01", Capacity: 500_000, Bytes: 500_000, HasRoom: false},
				{Label: "lto-02", Capacity: 500_000, Bytes: 500_000, HasRoom: false},
			},
		},
		poolVolumes: map[string]int64{"vault": 3}, // 1 configured slot not yet labeled
	}
	_, body := get(t, NewServer(src, t.TempDir()).Handler(), "/")
	if !strings.Contains(body, "vault: no volume with room — label or recycle a reel (1 unlabeled slot(s) configured)") {
		t.Errorf("/ rollup missing the pool no-room alert with headroom text:\n%s", body)
	}
	if strings.Contains(body, "vault is full") || strings.Contains(body, "capacity") && strings.Contains(body, "vault is at") {
		t.Errorf("/ rollup should not also fire the aggregate capacity warn:\n%s", body)
	}
}

// multiHostSource builds a fixture spanning two hosts: "app01" with four configured
// DLEs (a1-a4) and "db01" with one (b1), each with one full archive so DLESummaries/
// DLENames/DisplayDLE all resolve it — the shared fixture for the host-coalescing
// (rollup) and host-grouping (/dles) tests.
func multiHostSource() fakeSource {
	now := time.Now()
	mk := func(dle, host, path string) *catalog.Run {
		return &catalog.Run{ID: "run-" + dle, Archives: []record.Archive{{
			Run: "run-" + dle, DLE: dle, Host: host, Path: path, Level: 0,
			Compressed: 100_000, FileCount: 1, CreatedAt: now.Add(-time.Hour),
		}}}
	}
	return fakeSource{runs: []*catalog.Run{
		mk("a1", "app01", "/a"), mk("a2", "app01", "/b"), mk("a3", "app01", "/c"), mk("a4", "app01", "/d"),
		mk("b1", "db01", "/data"),
	}}
}

// TestHomeRollupStaleHostCoalescing checks that three-of-four stale DLEs sharing a
// host ("app01") collapse into one coalesced alert naming the host and the "3 of 4"
// count, while a lone stale DLE on another host ("db01") keeps its individual alert.
func TestHomeRollupStaleHostCoalescing(t *testing.T) {
	now := time.Now()
	src := multiHostSource()
	src.stale = []catalog.StaleDLE{
		{DLE: "a1", Display: "app01:/a", LastBackup: now.Add(-72 * time.Hour)},
		{DLE: "a2", Display: "app01:/b", LastBackup: now.Add(-96 * time.Hour)}, // oldest of the three
		{DLE: "a3", Display: "app01:/c", LastBackup: now.Add(-48 * time.Hour)},
		{DLE: "b1", Display: "db01:/data", LastBackup: now.Add(-50 * time.Hour)},
	}

	_, body := get(t, NewServer(src, t.TempDir()).Handler(), "/")
	if !strings.Contains(body, "host app01: 3 of 4 DLEs stale") {
		t.Errorf("/ rollup missing the coalesced host alert:\n%s", body)
	}
	for _, individual := range []string{"app01:/a last backed up", "app01:/b last backed up", "app01:/c last backed up"} {
		if strings.Contains(body, individual) {
			t.Errorf("/ rollup shows an individual app01 alert alongside the coalesced one (%q):\n%s", individual, body)
		}
	}
	if !strings.Contains(body, "db01:/data last backed up") {
		t.Errorf("/ rollup missing the individual db01 alert (a lone stale DLE on its host):\n%s", body)
	}
}

// TestHomeRollupSingleStalePerHostStaysIndividual checks that a single stale DLE on
// a host (even one with other, non-stale, configured DLEs) keeps today's individual
// alert rather than being coalesced — coalescing only kicks in at two or more.
func TestHomeRollupSingleStalePerHostStaysIndividual(t *testing.T) {
	now := time.Now()
	src := multiHostSource()
	src.stale = []catalog.StaleDLE{{DLE: "a1", Display: "app01:/a", LastBackup: now.Add(-72 * time.Hour)}}

	_, body := get(t, NewServer(src, t.TempDir()).Handler(), "/")
	if !strings.Contains(body, "app01:/a last backed up") {
		t.Errorf("/ rollup missing the individual alert for a lone stale DLE:\n%s", body)
	}
	if strings.Contains(body, "host app01:") {
		t.Errorf("/ rollup coalesced a single stale DLE into a host alert:\n%s", body)
	}
}

// TestDLEsPageGroupsByHost checks that /dles sections both the heatmap and the
// Sources table by host once more than one host is present, with a header row
// naming each host, its DLE count, and its stale count.
func TestDLEsPageGroupsByHost(t *testing.T) {
	src := multiHostSource()
	src.stale = []catalog.StaleDLE{
		{DLE: "a1", Display: "app01:/a", LastBackup: time.Now().Add(-72 * time.Hour)},
		{DLE: "a2", Display: "app01:/b", LastBackup: time.Now().Add(-96 * time.Hour)},
		{DLE: "a3", Display: "app01:/c", LastBackup: time.Now().Add(-48 * time.Hour)},
	}

	code, body := get(t, NewServer(src, t.TempDir()).Handler(), "/dles")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if got := strings.Count(body, `class="host-row"`); got != 4 { // 2 hosts × (Sources + heatmap)
		t.Errorf("/dles rendered %d host-row headers, want 4 (app01+db01, table+heatmap):\n%s", got, body)
	}
	for _, want := range []string{">app01<", ">db01<", "3 stale"} {
		if !strings.Contains(body, want) {
			t.Errorf("/dles host section missing %q:\n%s", want, body)
		}
	}
}

// TestDLEsPageSingleHostIsUngrouped checks that /dles renders no host headers when
// every DLE shares one host (the pre-grouping, common case) — zero visual change.
func TestDLEsPageSingleHostIsUngrouped(t *testing.T) {
	code, body := get(t, NewServer(sampleSource(), t.TempDir()).Handler(), "/dles")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(body, `class="host-row"`) {
		t.Errorf("/dles rendered host headers for a single-host catalog:\n%s", body)
	}
}
