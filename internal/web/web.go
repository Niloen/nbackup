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
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/progress"
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
	DisplayDLE(slug string) string
	DLESummaries() []catalog.DLESummary
	DLENames() []string         // configured DLE slugs, for drill coverage (never-drilled)
	DrillWindow() time.Duration // the configured drill coverage window
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
	hist := s.history(12)
	// The status file persists after a run finishes (phase done/failed/canceled), so
	// the "run in progress" banner is shown only for a genuinely in-flight run — a
	// completed run is already summarized by the "last backup" card and the runs list.
	data := homeData{
		Live:       s.inProgress(),
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
			Copies:   s.copies(run.ID),
		})
	}
	// Newest run first.
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	s.render(w, "runs", page{Title: "Runs", Active: "runs", Data: rows})
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
		copies = append(copies, copyRow{Medium: p.Medium, Labels: strings.Join(p.Labels(), "+")})
	}
	s.render(w, "run", page{Title: run.ID, Active: "runs", Data: runDetail{
		ID:       run.ID,
		Date:     run.Date(),
		At:       run.LastArchiveAt(),
		Bytes:    run.TotalBytes(),
		Partial:  run.Partial(),
		Archives: run.Archives,
		Copies:   copies,
	}})
}

// handleDLEs renders the DLE-major catalog view: one row per backup source, each
// linking to its own history so an operator can drill into a single DLE.
func (s *Server) handleDLEs(w http.ResponseWriter, r *http.Request) {
	sums := s.src.DLESummaries()
	rows := make([]dleRow, 0, len(sums))
	for _, d := range sums {
		rows = append(rows, dleRow{
			Slug: d.DLE, Display: d.Display, Runs: d.Runs, LastLevel: d.LastLevel,
			LastFull: d.LastFull, Bytes: d.Bytes, Media: strings.Join(d.Media, ", "),
		})
	}
	s.render(w, "dles", page{Title: "DLEs", Active: "dles", Data: rows})
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
	s.render(w, "dle", page{Title: sum.Display, Active: "dles", Data: dleDetail{
		Slug:    sum.DLE,
		Display: sum.Display,
		Runs:    sum.Runs,
		Bytes:   sum.Bytes,
		Media:   strings.Join(sum.Media, ", "),
		History: s.dleHistory(slug),
	}})
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
					if labels := p.Labels(); len(labels) > 0 {
						media = append(media, p.Medium+":"+strings.Join(labels, "+"))
					} else {
						media = append(media, p.Medium)
					}
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

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	s.render(w, "media", page{Title: "Media", Active: "media", Data: s.src.Media()})
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
	s.render(w, "medium", page{Title: name, Active: "media", Data: newMediumData(st)})
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

	ledger, err := drill.Load(s.workdir)
	if err != nil {
		ledger = &drill.Ledger{} // unreadable ledger renders as never-drilled, not a 500
	}
	never, overdue := ledger.Coverage(s.src.DLENames(), window, now)
	data.Overdue = overdue
	for _, slug := range never {
		data.Never = append(data.Never, s.src.DisplayDLE(slug))
	}
	sort.Strings(data.Never)

	for _, rec := range ledger.Sorted() {
		row := drillLedgerRow{
			DLE:    s.src.DisplayDLE(rec.DLE),
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
				DLE: s.src.DisplayDLE(h.DLE), OK: h.OK, Drilled: h.Drilled,
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
	s.render(w, "report", page{Title: "History", Active: "report", Data: s.history(0)})
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
// CLI's copiesSummary), naming the volume label only when a medium carries one.
func (s *Server) copies(runID string) string {
	ps := s.src.Placements(runID)
	if len(ps) == 0 {
		return "—"
	}
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		if labels := p.Labels(); len(labels) > 0 {
			names = append(names, p.Medium+":"+strings.Join(labels, "+"))
		} else {
			names = append(names, p.Medium)
		}
	}
	return strings.Join(names, ", ")
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
