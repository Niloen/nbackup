// Package web serves NBackup's read-only status pages: a small HTTP view over the
// same catalog, run-history, and live-progress data the CLI renders — so backup
// health can be glanced at from a browser or phone without shell access.
//
// It is deliberately read-only. The Source interface it renders from exposes only
// reads, and every route is a GET that renders a page; no request can start a run,
// prune, relabel, or touch a medium. `nb web` is a status page, not a management
// console — keep it that way by not widening Source.
package web

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/report"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// Source is the read-only slice of the engine the webui renders. Listing only read
// methods here is the guardrail that keeps `nb web` a status view: a handler has no
// verb reachable through this interface to mutate the catalog or a medium. Widening
// it (to add an action) is the moment to stop and reconsider scope.
type Source interface {
	Runs() []*catalog.Run
	ReadRun(id string) (*catalog.Run, error)
	Placements(runID string) []catalog.Placement
	Media() []engine.MediumInfo
	MediumStats(name string) (engine.MediumStats, bool) // one medium's usage history + statistics
	// MediumProtected reports the bytes a prune cannot reclaim on the named medium
	// (the protected recovery set) and its capacity as of now; ok is false for an
	// unknown medium. The rollup reads this instead of raw Used/Capacity for
	// address-identified media (disk, s3): the planner fills free space and prune
	// trims to capacity, so Used sits permanently near 100% at steady state — only
	// the protected residual distinguishes "full by design" from "actually stuck."
	MediumProtected(name string, now time.Time) (residual, capacity int64, ok bool)
	DisplayDLE(slug string) string
	DLESummaries() []catalog.DLESummary
	DLENames() []string         // configured DLE slugs, for drill coverage (never-drilled)
	DrillWindow() time.Duration // the configured drill coverage window
	// StaleDLEs reports the configured DLEs overdue against the dump cycle (or
	// never backed up at all) as of now — always on, since the cycle is the
	// existing freshness promise ("a full never ages past one cycle") and needs
	// no separate config to enforce.
	StaleDLEs(now time.Time) []catalog.StaleDLE
}

// Server renders the status pages from a Source plus the catalog workdir, where the
// run-history (run-log.jsonl) and live-progress (run-status.json) files live.
type Server struct {
	src     Source
	workdir string
	now     func() time.Time // injectable clock for tests; production passes time.Now
}

// NewServer builds a status server over src, reading history/progress files from
// workdir (the catalog workdir, cfg.WorkdirPath()).
func NewServer(src Source, workdir string) *Server {
	return &Server{src: src, workdir: workdir, now: time.Now}
}

// Handler returns the router for the status site. Every route is read-only.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)         // exact "/"; unknown paths 404 in the handler
	mux.HandleFunc("/runs", s.handleRuns)     // exact
	mux.HandleFunc("/runs/", s.handleRun)     // subtree: /runs/<id>
	mux.HandleFunc("/dles", s.handleDLEs)     // exact
	mux.HandleFunc("/dles/", s.handleDLE)     // subtree: /dles/<slug>
	mux.HandleFunc("/media", s.handleMedia)   // exact
	mux.HandleFunc("/media/", s.handleMedium) // subtree: /media/<name>
	mux.HandleFunc("/drills", s.handleDrills)
	mux.HandleFunc("/report", s.handleReport)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/metrics", s.handleMetrics) // Prometheus text exposition (metrics.go)
	return mux
}

// live returns the current run's progress snapshot, or nil when no run has written a
// status file (the common at-rest case).
func (s *Server) live() *progress.Snapshot {
	snap, err := progress.Load(s.workdir)
	if err != nil {
		return nil // includes "no status file yet"
	}
	return &snap
}

// inProgress returns the live status view only while a run is actually running
// (non-terminal). The status file lingers after a run ends, so this is what the home
// banner keys off — nil once the run is done/failed/canceled.
func (s *Server) inProgress() *statusView {
	live := s.live()
	if live == nil || live.Phase.Terminal() {
		return nil
	}
	return newStatusView(live, s.now())
}

// history returns run records newest-first (report.Load is oldest-first). n<=0 loads
// the whole history.
func (s *Server) history(n int) []report.Run {
	runs, err := report.Last(s.workdir, n)
	if err != nil {
		return nil
	}
	// Reverse in place to newest-first for display.
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	return runs
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	runs := s.src.Runs()
	var total int64
	for _, run := range runs {
		total += run.TotalBytes()
	}
	// The rollup keys off the most recent run of *each* command, which a fixed recent
	// window could miss (many dumps can bury the last sync), so it reads the full
	// history; the "recent activity" table below shows only the newest slice.
	histAll := s.history(0)
	hist := histAll
	if len(hist) > 12 {
		hist = hist[:12]
	}
	// The status file persists after a run finishes (phase done/failed/canceled), so
	// the "run in progress" banner is shown only for a genuinely in-flight run — a
	// completed run is already summarized by the "last backup" card and the runs list.
	data := homeData{
		Live:       s.inProgress(),
		Alerts:     s.rollup(s.now(), histAll),
		RunCount:   len(runs),
		TotalBytes: total,
		Media:      s.src.Media(),
		History:    hist,
		LastDump:   lastDump(hist),
	}
	refresh := 0
	if data.Live != nil {
		refresh = 5 // a run is in flight — auto-refresh the banner
	}
	s.render(w, "home", page{Title: "Overview", Active: "home", Refresh: refresh, Data: data})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs := s.src.Runs()
	rows := make([]runRow, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, runRow{
			ID:       run.ID,
			Partial:  run.Partial(),
			Archives: len(run.Archives),
			Bytes:    run.TotalBytes(),
			At:       run.LastArchiveAt(),
			Copies:   s.copies(run),
		})
	}
	// Newest run first.
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	all := showAll(r)
	shown := rows
	if !all && len(shown) > maxListRows {
		shown = shown[:maxListRows]
	}
	s.render(w, "runs", page{Title: "Runs", Active: "runs", Data: runsData{Rows: shown, Total: len(rows), All: all}})
}

// maxListRows caps /runs and /report to the most recent rows by default, matching
// the /drills recent-runs cap (maxDrillRuns); ?all=1 shows the full history.
const maxListRows = 50

// showAll reports whether the request asked for the uncapped list via ?all=1. Any
// other or missing value (including garbage) is treated as "no" rather than erroring.
func showAll(r *http.Request) bool {
	return r.URL.Query().Get("all") == "1"
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/runs/")
	if id == "" {
		http.Redirect(w, r, "/runs", http.StatusSeeOther)
		return
	}
	run, err := s.src.ReadRun(id)
	if err != nil {
		s.render(w, "run", page{Title: id, Active: "runs", Data: runDetail{NotFound: true, ID: id}})
		return
	}
	placements := s.src.Placements(run.ID)
	copies := make([]copyRow, 0, len(placements))
	for _, p := range placements {
		held, total := p.Covers(run)
		copies = append(copies, copyRow{Medium: p.Medium, Labels: strings.Join(p.Labels(), "+"), Held: held, Total: total})
	}
	s.render(w, "run", page{Title: run.ID, Active: "runs", Data: runDetail{
		ID:       run.ID,
		Date:     run.Date(),
		At:       run.LastArchiveAt(),
		Bytes:    run.TotalBytes(),
		Partial:  run.Partial(),
		Archives: run.Archives,
		Copies:   copies,
		Grid:     buildPlacementGrid(run, placements, copies),
		Dump:     s.findDumpReport(run.ID),
	}})
}

// buildPlacementGrid assembles the /runs/<id> archives × placements matrix. cols are
// the coverage rows handleRun already built, reused as column headers so the two
// sections agree by construction.
func buildPlacementGrid(run *catalog.Run, placements []catalog.Placement, cols []copyRow) *placementGrid {
	if len(placements) == 0 {
		return nil
	}
	g := &placementGrid{Cols: cols}
	for _, a := range run.Archives {
		row := placementRow{DLE: a.DLE, DLEID: a.DLEID(), Level: a.Level}
		for _, p := range placements {
			var cell placementCell
			if pa, ok := p.Placed(a.DLE, a.Level); ok {
				cell = placementCell{Held: true, Pos: archivePosText(pa)}
			}
			row.Cells = append(row.Cells, cell)
		}
		g.Rows = append(g.Rows, row)
	}
	return g
}

// findDumpReport looks up run.ID's dump record in the run history and, when found
// with per-DLE statistics, builds the /runs/<id> dump-report section (the web mirror
// of `nb report --dump`). A run predating the run-log, or one compacted out, simply
// has no dump section — the archives list already shows sizes.
func (s *Server) findDumpReport(runID string) *dumpReportView {
	for _, r := range s.history(0) {
		if r.Command == report.CommandDump && r.RunID == runID && len(r.DumpStats) > 0 {
			return newDumpReportView(r)
		}
	}
	return nil
}

// handleDLEs renders the DLE-major catalog view: one row per backup source, each
// linking to its own history so an operator can drill into a single DLE.
func (s *Server) handleDLEs(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	data := groupDLEs(s.src.DLESummaries(), s.src.StaleDLEs(now), s.buildHeatmap(now))
	s.render(w, "dles", page{Title: "DLEs", Active: "dles", Data: data})
}

// handleDLE renders one DLE's history — its archive in every run that dumped it,
// newest first, each linking back to the run. The slug is the internal DLE id (as
// listed on /dles); an unknown slug renders a not-found page rather than 404 so the
// nav stays intact.
func (s *Server) handleDLE(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/dles/")
	if slug == "" {
		http.Redirect(w, r, "/dles", http.StatusSeeOther)
		return
	}
	var sum *catalog.DLESummary
	for _, d := range s.src.DLESummaries() {
		if d.DLE == slug {
			d := d
			sum = &d
			break
		}
	}
	if sum == nil {
		s.render(w, "dle", page{Title: slug, Active: "dles", Data: dleDetail{NotFound: true, Slug: slug}})
		return
	}
	points := s.recoveryPoints(slug, s.drillLedger())
	all := showAll(r)
	shown := points
	if !all && len(shown) > maxRecoveryPoints {
		shown = shown[:maxRecoveryPoints]
	}
	s.render(w, "dle", page{Title: sum.Display, Active: "dles", Data: dleDetail{
		Slug:     sum.DLE,
		Display:  sum.Display,
		Runs:     sum.Runs,
		Bytes:    sum.Bytes,
		Media:    strings.Join(sum.Media, ", "),
		Trend:    dleTrendSVG(s.dleTrend(slug)),
		Recovery: shown,
		RecTotal: len(points),
		RecAll:   all,
		VolMap:   s.buildDLEVolMap(slug),
		History:  s.dleHistory(slug),
	}})
}

// maxRecoveryPoints caps the /dles/<slug> recovery-points list to the newest points by
// default; ?all=1 shows every point, matching the paging pattern the other pages use.
const maxRecoveryPoints = 20

// dleTrend gathers a DLE's dump-history points — original/output size, dump time,
// and level — from every dump record in the run history that dumped it, oldest
// first, for the /dles/<slug> trend chart.
func (s *Server) dleTrend(slug string) []dleTrendPoint {
	var pts []dleTrendPoint
	for _, r := range s.history(0) { // newest-first; sorted below
		if r.Command != report.CommandDump {
			continue
		}
		at := r.EndedAt
		if at.IsZero() {
			at = r.StartedAt
		}
		for _, d := range r.DumpStats {
			if d.DLE == slug {
				pts = append(pts, dleTrendPoint{At: at, Orig: d.Orig, Out: d.Out, Seconds: d.Seconds, Level: d.Level})
			}
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].At.Before(pts[j].At) })
	return pts
}

// dleHistory gathers a DLE's archive from every run that holds one, newest run first —
// the DLE-major slice of the same archives the run pages present run-major. Each row
// names the media that actually hold that archive (archive-granular, matching a
// per-archive prune), so a copy that has been reclaimed off one medium shows honestly.
func (s *Server) dleHistory(slug string) []dleArchiveRow {
	var rows []dleArchiveRow
	for _, run := range s.src.Runs() {
		for _, a := range run.Archives {
			if a.DLE != slug {
				continue
			}
			var media []string
			for _, p := range s.src.Placements(run.ID) {
				if p.Holds(a.DLE, a.Level) {
					media = append(media, archiveCopyName(p, a.DLE, a.Level))
				}
			}
			rows = append(rows, dleArchiveRow{
				RunID:   run.ID,
				Date:    run.Date(),
				Level:   a.Level,
				Bytes:   a.Compressed,
				At:      a.CreatedAt,
				Files:   a.FileCount,
				Partial: a.Partial(),
				Copies:  strings.Join(media, ", "),
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RunID > rows[j].RunID })
	return rows
}

// recoveryPoints builds a DLE's restorable points — one per archive of the DLE in the
// catalog, newest run first — each with its restore chain, chain health, media
// availability, and drill status. Reads only Runs()/Placements() and the drill ledger,
// all read-only. ledger is passed in so the home rollup and the DLE page share one load.
func (s *Server) recoveryPoints(slug string, ledger *drill.Ledger) []recoveryPoint {
	// The DLE's archive in each run (a run dumps each DLE once), keyed by run id so the
	// BaseRun walk can resolve each incremental's base.
	archOf := map[string]record.Archive{}
	var tips []record.Archive
	for _, run := range s.src.Runs() {
		for _, a := range run.Archives {
			if a.DLE == slug {
				archOf[a.Run] = a
				tips = append(tips, a)
			}
		}
	}
	sort.Slice(tips, func(i, j int) bool { return tips[i].Run > tips[j].Run }) // newest first

	// held names the copies currently holding an archive, archive-granular — a copy
	// reclaimed off one medium no longer counts, which is what makes a chain honestly
	// "broken" rather than falsely restorable. Each copy carries its medium and the
	// archive's OWN volume labels, so chainMedia can intersect chains by medium while
	// still naming every tape the chain needs.
	held := func(a record.Archive) []chainCopy {
		var copies []chainCopy
		for _, p := range s.src.Placements(a.Run) {
			if pa, ok := p.Placed(a.DLE, a.Level); ok {
				copies = append(copies, chainCopy{Medium: p.Medium, Labels: pa.Labels()})
			}
		}
		return copies
	}

	rec, hasRec := ledger.Get(slug)
	points := make([]recoveryPoint, 0, len(tips))
	for i, tip := range tips {
		members, reason := recoveryChain(tip, archOf, held)
		onePlace, media := chainMedia(members)
		pt := recoveryPoint{
			RunID: tip.Run, Date: record.RunDate(tip.Run), Level: tip.Level, At: tip.CreatedAt,
			Chain: chainDesc(members), Broken: reason != "", Reason: reason,
			OnePlace: onePlace, Media: media,
			Drilled: hasRec && rec.OK && rec.RunID == tip.Run,
		}
		if i == 0 && pt.Drilled { // the newest point carries the ledger's tier gloss
			pt.Gloss = tierWhat(rec.Tier)
		}
		points = append(points, pt)
	}
	return points
}

// recoveryChain walks a recovery point's restore chain from the tip down the recorded
// BaseRun links to the level-0 full, returning the chain members (tip first) and the
// first broken link found, if any — a member with no surviving copy, or a base run
// pruned out of the catalog. An empty reason means COMPLETE: every member exists and
// is held. A no-copy member does not stop the walk (the chain is still describable); a
// missing base does (there is nothing further to walk). visited guards against a
// corrupted catalog whose BaseRun links cycle back on themselves — unlike
// recovery.Chain's index walk (which can only ever move to a strictly earlier run and
// so cannot cycle by construction), this walks a map keyed by run id and has no such
// built-in guarantee.
func recoveryChain(tip record.Archive, archOf map[string]record.Archive, held func(record.Archive) []chainCopy) (members []chainMember, reason string) {
	visited := map[string]bool{}
	cur := tip
	for {
		if visited[cur.Run] {
			return members, "base chain cycles back to run " + cur.Run
		}
		visited[cur.Run] = true
		media := held(cur)
		members = append(members, chainMember{RunID: cur.Run, Level: cur.Level, Copies: media})
		if len(media) == 0 && reason == "" {
			if len(members) == 1 {
				reason = cur.Run + " has no copy"
			} else {
				reason = "base " + cur.Run + " has no copy"
			}
		}
		if cur.Level == 0 {
			return members, reason
		}
		base, ok := archOf[cur.BaseRun]
		if !ok {
			if reason == "" {
				reason = "base " + cur.BaseRun + " missing"
			}
			return members, reason
		}
		cur = base
	}
}

// buildHeatmap builds the /dles activity matrix over the last 35 days (5 weeks) ending
// on now's local day: one row per configured DLE (DLESummaries order), one cell per
// day colored by what landed. Returns nil when the catalog holds no archives at all, so
// the caller can omit the whole section.
func (s *Server) buildHeatmap(now time.Time) *heatmap {
	const days = 35
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	// Per DLE, per local day, what landed that day.
	type dayAgg struct {
		full, incr, partial bool
		runs                map[string]bool
		levels              map[int]bool
		bytes               int64
	}
	byDLE := map[string]map[string]*dayAgg{}
	any := false
	for _, run := range s.src.Runs() {
		for _, a := range run.Archives {
			any = true
			key := a.CreatedAt.In(loc).Format("2006-01-02")
			perDay := byDLE[a.DLE]
			if perDay == nil {
				perDay = map[string]*dayAgg{}
				byDLE[a.DLE] = perDay
			}
			ag := perDay[key]
			if ag == nil {
				ag = &dayAgg{runs: map[string]bool{}, levels: map[int]bool{}}
				perDay[key] = ag
			}
			switch {
			case a.Partial():
				ag.partial = true
			case a.Level == 0:
				ag.full = true
			default:
				ag.incr = true
			}
			ag.runs[a.Run] = true
			ag.levels[a.Level] = true
			ag.bytes += a.Compressed
		}
	}
	if !any {
		return nil
	}

	hm := &heatmap{}
	dates := make([]time.Time, days)
	for i := 0; i < days; i++ {
		d := today.AddDate(0, 0, i-(days-1)) // oldest first, today last
		dates[i] = d
		tick := ""
		switch {
		case d.Day() == 1:
			tick = d.Format("Jan")
		case d.Weekday() == time.Monday:
			tick = fmt.Sprintf("%d", d.Day())
		}
		hm.Days = append(hm.Days, heatDay{Tick: tick})
	}

	for _, sum := range s.src.DLESummaries() {
		row := heatRow{Slug: sum.DLE, Display: sum.Display}
		perDay := byDLE[sum.DLE]
		for _, d := range dates {
			cell := heatCell{Class: "none", Title: d.Format("Mon Jan 2")}
			if ag := perDay[d.Format("2006-01-02")]; ag != nil {
				switch {
				case ag.partial:
					cell.Class = "partial"
				case ag.full:
					cell.Class = "full"
				case ag.incr:
					cell.Class = "incr"
				}
				cell.Title = fmt.Sprintf("%s · %s · %s", d.Format("Mon Jan 2"), heatLevels(ag.levels), sizeutil.FormatBytes(ag.bytes))
				if len(ag.runs) == 1 {
					for id := range ag.runs {
						cell.RunID = id
					}
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		hm.Rows = append(hm.Rows, row)
	}
	return hm
}

// heatLevels renders a day's dump levels as a sorted "L0, L1" list for the cell tooltip.
func heatLevels(set map[int]bool) string {
	lv := make([]int, 0, len(set))
	for l := range set {
		lv = append(lv, l)
	}
	sort.Ints(lv)
	parts := make([]string, len(lv))
	for i, l := range lv {
		parts[i] = levelTag(l)
	}
	return strings.Join(parts, ", ")
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	rows := newMediaRows(s.src.Media(), s.src.MediumStats, s.now())
	s.render(w, "media", page{Title: "Media", Active: "media", Data: rows})
}

// handleMedium renders one medium's detail page: its capacity utilization, the
// full/incremental split, a growth projection, and the used-capacity-over-time chart —
// the browser view of `nb medium <name>`. An unknown name renders a not-found page
// (nav intact) rather than a 404, matching the run/DLE detail pages.
func (s *Server) handleMedium(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/media/")
	if name == "" {
		http.Redirect(w, r, "/media", http.StatusSeeOther)
		return
	}
	st, ok := s.src.MediumStats(name)
	if !ok {
		s.render(w, "medium", page{Title: name, Active: "media", Data: mediumData{NotFound: true, Name: name}})
		return
	}
	d := newMediumData(st)
	d.VolMap = s.buildMediumVolMap(name, st)
	s.render(w, "medium", page{Title: name, Active: "media", Data: d})
}

// handleDrills renders the recovery-drill picture: the coverage rollup and per-DLE
// ledger (what each DLE's last drill tested, against which copy, how much it read,
// and whether it passed — the browser view of `nb report`'s drill-coverage section)
// plus the recent drill runs from the history, each with its per-DLE outcomes. The
// ledger is read from its workdir file per request, like the run history, so the
// page is always current.
func (s *Server) handleDrills(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	window := s.src.DrillWindow()
	data := drillsData{Window: sizeutil.FormatDuration(window)}

	ledger := s.drillLedger()
	never, overdue := ledger.Coverage(s.src.DLENames(), window, now)
	data.Overdue = overdue
	for _, slug := range never {
		data.Never = append(data.Never, dleLink{Slug: slug, Display: s.src.DisplayDLE(slug)})
	}
	sort.Slice(data.Never, func(i, j int) bool { return data.Never[i].Display < data.Never[j].Display })

	for _, rec := range ledger.Sorted() {
		row := drillLedgerRow{
			DLE:    s.src.DisplayDLE(rec.DLE),
			Slug:   rec.DLE,
			Tier:   rec.Tier,
			What:   tierWhat(rec.Tier),
			Medium: rec.Medium,
			AsOf:   rec.AsOf,
			RunID:  rec.RunID,
			At:     rec.LastDrill,
			Age:    sizeutil.FormatDaysHours(now.Sub(rec.LastDrill)),
			Bytes:  rec.Bytes,
			Drills: rec.Drills,
		}
		switch {
		case !rec.OK:
			row.Status, row.Failing = "failing", true
			row.Class, row.Detail = rec.Class, rec.Detail
			row.Remedy = drill.ParseClass(rec.Class).Remedy()
			data.Failing++
		case now.Sub(rec.LastDrill) >= window:
			row.Status, row.Stale = "stale", true
			data.Stale++
		default:
			row.Status = "ok"
			data.Passing++
		}
		data.Ledger = append(data.Ledger, row)
	}

	// Recent drill runs, newest first, each with its per-DLE outcomes.
	for _, run := range s.history(0) {
		if run.Command != report.CommandDrill || len(data.Runs) == maxDrillRuns {
			continue
		}
		dr := drillRunRow{
			EndedAt: run.EndedAt, Failed: run.Failed(), Error: run.Error,
			Tier: run.Tier, What: tierWhat(run.Tier), Bytes: run.BytesMoved,
			Failures: run.Failures, Skipped: run.Skipped, Overdue: run.Overdue,
		}
		for _, h := range run.DrillHealth {
			if h.Drilled {
				dr.Drilled++
			}
			dr.Targets = append(dr.Targets, drillTargetRow{
				DLE: s.src.DisplayDLE(h.DLE), Slug: h.DLE, OK: h.OK, Drilled: h.Drilled,
				Class: h.Class, Degrading: h.Degrading(), Bytes: h.Bytes,
			})
		}
		data.Runs = append(data.Runs, dr)
	}

	s.render(w, "drills", page{Title: "Drills", Active: "drills", Data: data})
}

// maxDrillRuns caps the recent-drills section; the full history is on /report.
const maxDrillRuns = 10

// tierWhat is the one-line "what this tier tested" gloss shown beside the tier
// token, mirroring the ladder documented on `nb drill --help`.
func tierWhat(tier string) string {
	switch tier {
	case "sample":
		return "re-hashed one sealed part per archive"
	case "checksum":
		return "re-hashed stored bytes against the seal"
	case "structural":
		return "decrypted, decompressed, and listed the tar stream"
	case "chain":
		return "point-in-time chain restore to scratch"
	case "stock":
		return "restored via the stock gpg/zstd/tar one-liner"
	default:
		return ""
	}
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	hist := s.history(0)
	all := showAll(r)
	shown := hist
	if !all && len(shown) > maxListRows {
		shown = shown[:maxListRows]
	}
	rows := make([]historyRow, 0, len(shown))
	for _, run := range shown {
		rows = append(rows, newHistoryRow(run))
	}
	s.render(w, "report", page{Title: "History", Active: "report", Data: historyData{Rows: rows, Total: len(hist), All: all}})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	live := s.live()
	refresh := 0
	if live != nil && !live.Phase.Terminal() {
		refresh = 5
	}
	s.render(w, "status", page{Title: "Status", Active: "status", Refresh: refresh, Data: newStatusView(live, s.now())})
}

// copies renders a run's placements as a compact "medium:label" list (matching the
// CLI's copiesSummary), naming the volume label only when a medium carries one and
// marking a placement that holds only some of the run's archives as partial.
func (s *Server) copies(run *catalog.Run) string {
	ps := s.src.Placements(run.ID)
	if len(ps) == 0 {
		return "—"
	}
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		name := placementName(p)
		if held, total := p.Covers(run); held < total {
			name += fmt.Sprintf(" (partial %d/%d)", held, total)
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// placementName is a copy's compact "medium:label" identity — the medium alone for an
// address-identified copy (disk/cloud carry no labels), the medium plus its volume
// labels for a tape. Run-scoped: the labels are every volume the run's copy touches,
// so it names whole copies (the run list); an archive-scoped row uses
// archiveCopyName, which narrows to that archive's own volumes.
func placementName(p catalog.Placement) string {
	if labels := p.Labels(); len(labels) > 0 {
		return p.Medium + ":" + strings.Join(labels, "+")
	}
	return p.Medium
}

// archiveCopyName names one archive's copy on a placement: the medium plus the
// volumes THAT archive occupies — not Placement.Labels, which merges every archive
// of the run's copy and would claim volumes this archive never touched.
func archiveCopyName(p catalog.Placement, dle string, level int) string {
	if pa, ok := p.Placed(dle, level); ok {
		if labels := pa.Labels(); len(labels) > 0 {
			return p.Medium + ":" + strings.Join(labels, "+")
		}
	}
	return p.Medium
}

// lastDump returns the most recent dump record in a newest-first history, so the home
// page can headline "last backup" distinctly from copy/prune/verify activity.
func lastDump(hist []report.Run) *report.Run {
	for i := range hist {
		if hist[i].Command == report.CommandDump {
			return &hist[i]
		}
	}
	return nil
}

// rollup gathers the home page's "attention needed" alerts from the same read-only
// data the detail pages render — no new state, just an aggregation: the most recent
// run of each command that failed, media at or past capacity, drill failures and
// coverage gaps, and DLEs overdue against the dump cycle. hist is the full
// history, newest-first. Red (bad) alerts sort before amber (warn) ones; an empty
// result is the glanceable all-clear the template renders as a single quiet line.
func (s *Server) rollup(now time.Time, hist []report.Run) []alert {
	var bad, warn []alert

	// The most recent run of each command, flagged when it failed.
	for _, run := range lastPerCommand(hist) {
		if !run.Failed() {
			continue
		}
		bad = append(bad, alert{Level: "bad", Tag: "failed",
			Text: fmt.Sprintf("last %s failed%s", run.Command, exitDetail(run)),
			Href: runHref(run)})
	}

	// Capacity foresight, one alert per bounded medium (unbounded media carry none):
	// full is red and supersedes everything else. Short of that, a labeled pool
	// (Volumes > 0 — tape libraries/stations; never keyed on medium type) gets its
	// own native alert instead of the aggregate ≥90%-used/projected-full check: a
	// healthy rotation keeps most volumes permanently near-full by retention design
	// (N-1 of N at capacity), so the aggregate reading is a structurally false alarm
	// for a pool — the real capacity event is running out of reels with room.
	// Address-identified media (disk, s3) warn on the protected recovery set —
	// the bytes a prune cannot reclaim — reaching >=90% of capacity, not on raw
	// Used/Capacity: the planner deliberately fills free space and `nb prune` trims
	// to capacity (a high-water trim), so a mature medium's raw Used sits
	// permanently near 100% by design. The protected residual only grows when
	// retention itself (the recovery chains prune must keep) is the pressure, which
	// is the actual actionable signal. Falls back to no warn if the residual can't
	// be computed (unknown medium). Below that, the medium's own recorded growth
	// (MediumStats.Growth, the same curve /media/<name> charts) projecting filling
	// within 30 days still warns, unchanged.
	for _, m := range s.src.Media() {
		if m.Capacity <= 0 {
			continue
		}
		if m.Used >= m.Capacity {
			text := fmt.Sprintf("%s is full — %s of %s used", m.Name, sizeutil.FormatBytes(m.Used), sizeutil.FormatBytes(m.Capacity))
			if residual, _, ok := s.src.MediumProtected(m.Name, now); ok {
				if reclaimable := m.Used - residual; reclaimable > 0 {
					text += fmt.Sprintf(" — %s reclaimable, run nb prune", sizeutil.FormatBytes(reclaimable))
				}
			}
			bad = append(bad, alert{Level: "bad", Tag: "over capacity", Text: text, Href: "/media/" + m.Name})
			continue
		}
		if m.Volumes > 0 {
			st, _ := s.src.MediumStats(m.Name)
			withRoom, _ := poolRoomCount(st.PerVolume, st.PoolVolumes)
			var gapText string
			if gap := st.PoolVolumes - int64(len(st.PerVolume)); gap > 0 {
				gapText = fmt.Sprintf(" (%d unlabeled slot(s) configured)", gap)
			}
			switch withRoom {
			case 0:
				bad = append(bad, alert{Level: "bad", Tag: "no room",
					Text: fmt.Sprintf("%s: no volume with room — label or recycle a reel%s", m.Name, gapText),
					Href: "/media/" + m.Name})
			case 1:
				warn = append(warn, alert{Level: "warn", Tag: "last volume",
					Text: fmt.Sprintf("%s: last volume with room%s", m.Name, gapText),
					Href: "/media/" + m.Name})
			}
			continue
		}
		residual, capacity, ok := s.src.MediumProtected(m.Name, now)
		switch {
		case ok && capacity > 0 && float64(residual)/float64(capacity) >= 0.9:
			warn = append(warn, alert{Level: "warn", Tag: "retention pressure",
				Text: fmt.Sprintf("%s: retention needs %s of %s — pruning can no longer free a full cycle",
					m.Name, sizeutil.FormatBytes(residual), sizeutil.FormatBytes(capacity)),
				Href: "/media/" + m.Name})
		default:
			// The projection warn belongs to the FILLING regime only (used still
			// under 90% of capacity): a stabilized rotation hovers near capacity by
			// design with a sawtooth curve whose dip-to-peak reads as growth, so it
			// would flicker "projected full" forever — past the line, the
			// retention-pressure warn above owns the signal.
			if float64(m.Used)/float64(m.Capacity) >= 0.9 {
				continue
			}
			if st, ok := s.src.MediumStats(m.Name); ok && !st.Growth.ProjFull.IsZero() && st.Growth.ProjFull.Before(now.Add(30*24*time.Hour)) {
				warn = append(warn, alert{Level: "warn", Tag: "capacity forecast",
					Text: fmt.Sprintf("%s projected full in ~%dd", m.Name, projDays(st.Growth.ProjFull, now)),
					Href: "/media/" + m.Name})
			}
		}
	}

	// Recovery-drill health: a failing drill is red and named; the remaining coverage
	// gap (never-drilled or stale) is a single amber count linking to the drills page.
	dh := s.drillHealth(now)
	for _, f := range dh.Failing {
		bad = append(bad, alert{Level: "bad", Tag: "drill failing",
			Text: "recovery drill failing for " + f.Display,
			Href: "/dles/" + f.Slug})
	}
	if gap := dh.Overdue - len(dh.Failing); gap > 0 {
		warn = append(warn, alert{Level: "warn", Tag: "drill overdue",
			Text: fmt.Sprintf("%d DLE(s) overdue for a recovery drill", gap),
			Href: "/drills"})
	}

	// Recoverability: a DLE whose NEWEST recovery point has a broken chain cannot be
	// restored to its latest backup — red. Older broken points stay visible on the DLE
	// page but don't spam the rollup (only the latest point is the live promise).
	bad = append(bad, s.brokenLatestPoints()...)

	// Stale DLEs — overdue against the dump cycle, or never backed up at all. Two
	// or more stale DLEs sharing a host coalesce into one alert: correlated
	// staleness reads as a host-level problem (network, ssh, agent down), not four
	// independent per-DLE ones, and one smart alert beats four noisy ones. A DLE
	// with no host in its display id (bare-slug fallback) is never grouped.
	warn = append(warn, s.staleAlerts(now)...)

	warn = append(warn, s.dumpAnomalies(hist)...)

	return append(bad, warn...)
}

// brokenLatestPoints flags each configured DLE whose newest recovery point cannot be
// restored — its restore chain is missing a member or a surviving copy. It reuses the
// exact per-point computation the DLE page renders (recoveryPoints), so the rollup and
// the page can never disagree about whether the latest point is restorable.
func (s *Server) brokenLatestPoints() []alert {
	ledger := s.drillLedger()
	var out []alert
	for _, sum := range s.src.DLESummaries() {
		pts := s.recoveryPoints(sum.DLE, ledger)
		if len(pts) == 0 || !pts[0].Broken {
			continue
		}
		out = append(out, alert{Level: "bad", Tag: "unrestorable",
			Text: fmt.Sprintf("cannot restore %s to its latest point — %s", s.src.DisplayDLE(sum.DLE), pts[0].Reason),
			Href: "/dles/" + sum.DLE})
	}
	return out
}

// dumpAnomalies compares the newest dump record against each DLE's own recent
// history and flags what looks off: a DLE's size swinging hard from its usual
// footprint at that level, or the whole run taking much longer than usual. This is
// deliberately coarse — a "did it look wrong" nudge, not a statistical test — so the
// thresholds are blunt on purpose:
//   - size: needs at least 2 priors (of up to the 5 most recent at the same level),
//     a >2x deviation in either direction, AND an absolute delta over 64 MiB, so a
//     tiny DLE doubling from 1 kB to 2 kB doesn't flap.
//   - duration: needs at least 2 priors (of up to the 5 most recent dump runs), the
//     latest taking >2x their median, AND the delta exceeding 10 minutes, so a run
//     that was merely 3 minutes instead of 1 doesn't flap.
//
// hist is the full run history, newest-first.
func (s *Server) dumpAnomalies(hist []report.Run) []alert {
	latest, priors := latestDumpAndPriors(hist)
	if latest == nil {
		return nil
	}
	var out []alert

	const minSizeDelta = 64 << 20 // 64 MiB
	for _, d := range latest.DumpStats {
		var sizes []int64
		for _, r := range priors {
			for _, pd := range r.DumpStats {
				if pd.DLE == d.DLE && pd.Level == d.Level {
					sizes = append(sizes, pd.Orig)
					break
				}
			}
			if len(sizes) == 5 {
				break
			}
		}
		if len(sizes) < 2 {
			continue
		}
		med := medianInt64(sizes)
		delta := d.Orig - med
		if delta < 0 {
			delta = -delta
		}
		if med <= 0 || delta <= minSizeDelta {
			continue
		}
		if d.Orig > med*2 || d.Orig*2 < med {
			out = append(out, alert{Level: "warn", Tag: "size anomaly",
				Text: fmt.Sprintf("%s dumped %s, typically %s at this level", s.src.DisplayDLE(d.DLE), sizeutil.FormatBytes(d.Orig), sizeutil.FormatBytes(med)),
				Href: "/dles/" + d.DLE})
		}
	}

	const minDurationDelta = 10 * time.Minute
	if wall := runWall(*latest); wall > 0 {
		var durs []int64
		for _, r := range priors {
			if r.Command != report.CommandDump {
				continue
			}
			if w := runWall(r); w > 0 {
				durs = append(durs, int64(w))
			}
			if len(durs) == 5 {
				break
			}
		}
		if len(durs) >= 2 {
			med := time.Duration(medianInt64(durs))
			if med > 0 && wall > med*2 && wall-med > minDurationDelta {
				out = append(out, alert{Level: "warn", Tag: "slow dump",
					Text: fmt.Sprintf("last dump took %s, typically %s", sizeutil.FormatElapsed(wall), sizeutil.FormatElapsed(med)),
					Href: runHref(*latest)})
			}
		}
	}
	return out
}

// latestDumpAndPriors splits a newest-first history into the newest dump record that
// carries per-DLE statistics, and the dump records before it (also newest-first) to
// scan for baselines. Both are nil when the history holds no such record.
func latestDumpAndPriors(hist []report.Run) (*report.Run, []report.Run) {
	for i := range hist {
		if hist[i].Command == report.CommandDump && len(hist[i].DumpStats) > 0 {
			return &hist[i], hist[i+1:]
		}
	}
	return nil, nil
}

// runWall is a run's wall-clock duration, or 0 when either endpoint is unrecorded.
func runWall(r report.Run) time.Duration {
	if r.StartedAt.IsZero() || r.EndedAt.IsZero() {
		return 0
	}
	return r.EndedAt.Sub(r.StartedAt)
}

// medianInt64 returns the median of vs, which must be non-empty.
func medianInt64(vs []int64) int64 {
	s := append([]int64(nil), vs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// drillHealth is the shared drill-coverage summary the home rollup and /metrics both
// read, computed once from the ledger's own Coverage so neither reimplements it.
type drillHealth struct {
	Overdue int       // configured DLEs not covered within the window (never-drilled, stale, or failing)
	Failing []dleLink // DLEs whose most recent drill failed, sorted by display name
}

// drillHealth loads the recoverability ledger and classifies it against the drill
// window as of now, reusing the same Coverage the /drills page computes.
func (s *Server) drillHealth(now time.Time) drillHealth {
	ledger := s.drillLedger()
	_, overdue := ledger.Coverage(s.src.DLENames(), s.src.DrillWindow(), now)
	h := drillHealth{Overdue: overdue}
	for _, rec := range ledger.Sorted() {
		if !rec.OK {
			h.Failing = append(h.Failing, dleLink{Slug: rec.DLE, Display: s.src.DisplayDLE(rec.DLE)})
		}
	}
	sort.Slice(h.Failing, func(i, j int) bool { return h.Failing[i].Display < h.Failing[j].Display })
	return h
}

// drillLedger loads the recoverability ledger from the workdir, treating an
// unreadable ledger as empty (never-drilled) rather than an error — the same
// tolerance /drills applies, so a missing ledger never 500s a page or scrape.
func (s *Server) drillLedger() *drill.Ledger {
	ledger, err := drill.Load(s.workdir)
	if err != nil {
		return &drill.Ledger{}
	}
	return ledger
}

// lastPerCommand returns the most recent run of each command from a newest-first
// history, sorted by command name so the rollup and /metrics emit a stable order.
func lastPerCommand(hist []report.Run) []report.Run {
	seen := map[report.Command]bool{}
	var out []report.Run
	for i := range hist {
		if seen[hist[i].Command] {
			continue
		}
		seen[hist[i].Command] = true
		out = append(out, hist[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Command < out[j].Command })
	return out
}

// exitDetail is the short parenthetical the rollup appends to a failed-run alert —
// the stable exit class when one was recorded, nothing otherwise (the full error is
// one click away on the linked page).
func exitDetail(r report.Run) string {
	if r.ExitClass != "" {
		return " (" + r.ExitClass + ")"
	}
	return ""
}

// runHref points a failed-run alert at the most specific page: the run itself when
// it sealed one, the drills page for a drill (which seals no run), else the history.
func runHref(r report.Run) string {
	switch {
	case r.RunID != "":
		return "/runs/" + r.RunID
	case r.Command == report.CommandDrill:
		return "/drills"
	default:
		return "/report"
	}
}

// staleAlerts builds the rollup's stale-DLE alerts: one coalesced host alert for
// each host with two or more stale DLEs, and an individual alert for every other
// stale DLE (a lone stale DLE on a host, or one with no host at all).
func (s *Server) staleAlerts(now time.Time) []alert {
	stale := s.src.StaleDLEs(now)

	// StaleDLE carries no Display for a DLE that has never been backed up at all
	// (catalog.StaleDLEs), so fall back to DisplayDLE, which always has one.
	displayOf := func(d catalog.StaleDLE) string {
		if d.Display != "" {
			return d.Display
		}
		return s.src.DisplayDLE(d.DLE)
	}

	byHost := map[string][]catalog.StaleDLE{}
	var hostOrder []string
	for _, d := range stale {
		host, ok := hostOf(displayOf(d))
		if !ok {
			continue
		}
		if _, seen := byHost[host]; !seen {
			hostOrder = append(hostOrder, host)
		}
		byHost[host] = append(byHost[host], d)
	}

	var out []alert
	grouped := map[string]bool{}
	for _, host := range hostOrder {
		ds := byHost[host]
		if len(ds) < 2 {
			continue
		}
		grouped[host] = true
		out = append(out, alert{Level: "warn", Tag: "stale",
			Text: hostStaleText(host, ds, s.hostDLECount(host), now), Href: "/dles"})
	}
	for _, d := range stale {
		if host, ok := hostOf(displayOf(d)); ok && grouped[host] {
			continue
		}
		out = append(out, alert{Level: "warn", Tag: "stale",
			Text: staleText(d, now), Href: "/dles/" + d.DLE})
	}
	return out
}

// hostDLECount counts the configured DLEs (Source.DLENames) whose display id's
// host prefix matches host — the "of M" denominator in a coalesced host alert.
func (s *Server) hostDLECount(host string) int {
	n := 0
	for _, slug := range s.src.DLENames() {
		if h, ok := hostOf(s.src.DisplayDLE(slug)); ok && h == host {
			n++
		}
	}
	return n
}

// hostStaleText renders a coalesced per-host stale alert: how many of the host's
// configured DLEs are stale, and either the oldest last-backup age among them, or
// a callout when at least one has never been backed up at all.
func hostStaleText(host string, ds []catalog.StaleDLE, total int, now time.Time) string {
	var oldest time.Time
	never := false
	for _, d := range ds {
		if d.LastBackup.IsZero() {
			never = true
			continue
		}
		if oldest.IsZero() || d.LastBackup.Before(oldest) {
			oldest = d.LastBackup
		}
	}
	if never {
		return fmt.Sprintf("host %s: %d of %d DLEs stale, some never backed up", host, len(ds), total)
	}
	return fmt.Sprintf("host %s: %d of %d DLEs stale (oldest %s)", host, len(ds), total, sizeutil.FormatDuration(now.Sub(oldest)))
}

// staleText renders a stale DLE for the rollup: its identity plus how overdue it is,
// or "never been backed up" for a DLE the catalog has no archive for.
func staleText(d catalog.StaleDLE, now time.Time) string {
	name := d.Display
	if name == "" {
		name = d.DLE
	}
	if d.LastBackup.IsZero() {
		return name + " has never been backed up"
	}
	return fmt.Sprintf("%s last backed up %s ago", name, sizeutil.FormatDuration(now.Sub(d.LastBackup)))
}

// render executes a page template inside the shared layout. It buffers first so a
// template error becomes a clean 500 rather than a half-written page.
func (s *Server) render(w http.ResponseWriter, name string, p page) {
	p.Now = s.now()
	t := pages[name]
	if t == nil {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base.html", p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// volumeSegments gathers every placed archive part that sits on a labeled volume,
// keyed by label — the catalog facts behind the volume maps. medium narrows to one
// medium's placements; "" gathers all. Part sizes come from the placement's per-part
// seals when they align; a sealless record falls back to an even split of the
// archive's compressed size, so an old placement still draws (approximately).
func (s *Server) volumeSegments(medium string) map[string][]volSeg {
	segs := map[string][]volSeg{}
	for _, run := range s.src.Runs() {
		for _, p := range s.src.Placements(run.ID) {
			if medium != "" && p.Medium != medium {
				continue
			}
			for _, pa := range p.Archives {
				var arch *record.Archive
				for i := range run.Archives {
					if run.Archives[i].DLE == pa.DLE && run.Archives[i].Level == pa.Level {
						arch = &run.Archives[i]
						break
					}
				}
				if arch == nil {
					continue // a placement of an archive the run record no longer lists
				}
				sealed := len(pa.Seals) == len(pa.Parts) && len(pa.Parts) > 0
				for i, pt := range pa.Parts {
					if pt.Label == "" {
						break // address-identified copy: no volume rows to draw
					}
					var size int64
					if sealed {
						size = pa.Seals[i].Size
					} else if n := len(pa.Parts); n > 0 {
						size = arch.Compressed / int64(n)
					}
					seg := volSeg{Pos: pt.Pos, RunID: run.ID, DLE: pa.DLE, DLEID: arch.DLEID(), Level: pa.Level, Bytes: size}
					if len(pa.Parts) > 1 {
						seg.Part, seg.Parts = i+1, len(pa.Parts)
					}
					segs[pt.Label] = append(segs[pt.Label], seg)
				}
			}
		}
	}
	return segs
}

// volumeCapacities maps every labeled volume the configured pools know to its
// capacity (absent = unknown: the bar scales to its content), for the DLE volume
// map, which crosses media.
func (s *Server) volumeCapacities() map[string]int64 {
	caps := map[string]int64{}
	for _, m := range s.src.Media() {
		if m.Volumes == 0 {
			continue
		}
		if st, ok := s.src.MediumStats(m.Name); ok {
			for _, v := range st.PerVolume {
				if v.Capacity > 0 {
					caps[v.Label] = v.Capacity
				}
			}
		}
	}
	return caps
}

// buildDLEVolMap draws the volumes holding any of the DLE's archives: its own parts
// colored by run age (dark = newest), other runs' content greyed, and the newest
// restore point's chain outlined — the physical answer to "which tapes hold this
// DLE". Nil when no archive of the DLE sits on a labeled volume (disk/cloud-only
// DLEs have no volumes to draw).
func (s *Server) buildDLEVolMap(slug string) *volMap {
	segs := s.volumeSegments("")
	var labels []string
	for label, ss := range segs {
		for _, v := range ss {
			if v.DLE == slug {
				labels = append(labels, label)
				break
			}
		}
	}
	if len(labels) == 0 {
		return nil
	}
	sort.Strings(labels)
	rank := runRank(segs, slug)
	chain := s.chainRuns(slug)
	caps := s.volumeCapacities()
	classOf := func(v volSeg) string {
		if v.DLE != slug {
			return "other"
		}
		cls := ageClass(rank, v.RunID)
		if chain[v.RunID] {
			cls += " chain"
		}
		return cls
	}
	return buildVolMap(segs, labels, func(l string) int64 { return caps[l] }, classOf)
}

// chainRuns is the set of runs in the DLE's newest restore chain (the tip archive
// down its BaseRun links to the level-0 full), for the volume map's chain outline.
// A missing base or a link cycle just ends the walk — chain health is the recovery
// points' story; the map only outlines what is walkable.
func (s *Server) chainRuns(slug string) map[string]bool {
	archOf := map[string]record.Archive{}
	var tip record.Archive
	for _, run := range s.src.Runs() {
		for _, a := range run.Archives {
			if a.DLE == slug {
				archOf[a.Run] = a
				if a.Run > tip.Run {
					tip = a
				}
			}
		}
	}
	set := map[string]bool{}
	cur, ok := tip, tip.Run != ""
	for ok && !set[cur.Run] {
		set[cur.Run] = true
		if cur.Level == 0 {
			break
		}
		cur, ok = archOf[cur.BaseRun]
	}
	return set
}

// buildMediumVolMap draws everything stored on the pool's volumes, in registry order,
// shaded by run age — nil for address-identified media, which have no volumes. A
// label placements reference but the registry has never seen (an offsite tape) still
// draws, after the registry's rows.
func (s *Server) buildMediumVolMap(name string, st engine.MediumStats) *volMap {
	if st.Volumes == 0 {
		return nil
	}
	segs := s.volumeSegments(name)
	caps := map[string]int64{}
	known := map[string]bool{}
	var labels []string
	for _, v := range st.PerVolume {
		labels = append(labels, v.Label)
		known[v.Label] = true
		caps[v.Label] = v.Capacity
	}
	var extra []string
	for label := range segs {
		if !known[label] {
			extra = append(extra, label)
		}
	}
	sort.Strings(extra)
	labels = append(labels, extra...)
	rank := runRank(segs, "")
	classOf := func(v volSeg) string { return ageClass(rank, v.RunID) }
	return buildVolMap(segs, labels, func(l string) int64 { return caps[l] }, classOf)
}
