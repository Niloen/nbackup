package engine

import (
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/record"
)

// coverage.go judges a run's copies against the CURRENT config: which media are
// supposed to hold which of the run's archives, and on what promise. The judgment
// is evaluated live, never recorded — the config is the promise of record, so
// adding a landing or a sync rule deliberately makes existing runs read as behind
// on the new medium (and `nb sync` is how they catch up), while a medium holding
// archives nothing routes to it anymore holds a bonus copy, not a defect.
//
// The promise is also time-bounded, per medium: retention is per-medium (each
// store prunes against its own capacity and minimum_age), so media of different
// sizes keep different depths of history, and an archive a medium's own retention
// may delete cannot be demanded of it. owesTo is that bound — an archive is owed
// only while it is on its DLE's live recovery chain or within the medium's
// minimum_age; past it, an expectation decays to CopyAged and a gap there is
// rotation doing its job, never a "missing" defect.

// CopyClass is one archive's relationship to one medium, in ascending promise
// strength (a route outranks a sync promise when both name the same medium).
type CopyClass int

const (
	// CopyNone: the medium is neither in the archive's landing route nor a sync
	// target for it. A copy there is a bonus; a gap there is nobody's problem.
	CopyNone CopyClass = iota
	// CopyAged: the route (or an auto sync rule) once owed the archive to the
	// medium, but it has aged out of that medium's retention window (owesTo) — a
	// prune there may delete it at any time, so nothing may demand it. A gap is
	// the medium's rotation, not a defect; a copy still held is history the
	// medium simply has not needed to reclaim yet.
	CopyAged
	// CopyPromised: a sync rule mirrors the archive onto the medium, so a missing
	// copy is lag, not an error — the next `nb sync` closes it.
	CopyPromised
	// CopyRouted: the DLE's landing route includes the medium, so a missing copy
	// is a real gap — a tripped lane, a pruned copy, or a landing added since the
	// run (deliberately behind until a sync backfills it).
	CopyRouted
)

// archKey identifies one archive of a run: a run dumps each DLE once, but the
// placement primitives key on (DLE, level), so the judgment does too.
type archKey struct {
	dle   string
	level int
}

// RunCoverage is one run's expectation map: per medium, the class of every
// archive that medium is supposed to hold. Build it with JudgeRun (tests) or
// Engine.RunCoverage (live config).
type RunCoverage struct {
	run      *catalog.Run
	classes  map[string]map[archKey]CopyClass
	syncFrom map[string]string // promised medium -> its rule's source ("" = resolved per run)
}

// RunCoverage judges run's copies against this engine's current intent; see JudgeRun.
// The judgment is a live display computation, so it reads the wall clock.
func (e *Engine) RunCoverage(run *catalog.Run) *RunCoverage {
	minAge := func(medium string) time.Duration { return e.cfg.MinAgeFor(e.cfg.Media[medium]) }
	return JudgeRun(run, promiseRoutes(e.cfg, e.cat), e.cfg.Sync, e.cat.Runs(), e.cat.Placements, minAge, time.Now())
}

// promiseRoutes is the current-intent route map: every configured slug's route
// (config.Routes) plus each latest-resolved unit's dumptype route — the pattern
// children, whose slugs config structurally cannot contain (the catalog's resolved-set
// record supplies their dumptype; see catalog.RecordResolved). Both halves express
// CURRENT intent: a config route edit re-owes old archives immediately, and a unit that
// stops being resolved is retired exactly like a DLE removed from config. On a catalog
// with no resolved set (pre-record history, or rebuilt from media) this degrades to
// config-only — the shipped behavior.
func promiseRoutes(cfg *config.Config, cat *catalog.Catalog) map[string][]string {
	routes := cfg.Routes()
	for _, r := range cat.LatestResolved() {
		if _, ok := routes[r.DLE]; ok {
			continue
		}
		if media, err := cfg.LandingsForDumptype(r.DumpType); err == nil && len(media) > 0 {
			routes[r.DLE] = media
		}
	}
	return routes
}

// JudgeRun builds a run's expectation map from the DLE landing routes
// (config.Routes) and the sync rules. runs and placements feed the rule
// evaluation: a rule bounded by `last:` only promises the runs inside its
// window, and an explicit-source rule only promises what its source copy holds.
// minAge is each medium's retention floor (nil = zero for all) and now the
// judgment instant — together the owesTo bound that decays a stale expectation
// to CopyAged instead of reading a pruned copy as missing.
func JudgeRun(run *catalog.Run, routes map[string][]string, rules []config.SyncRule, runs []*catalog.Run, placements func(runID string) []catalog.Placement, minAge func(medium string) time.Duration, now time.Time) *RunCoverage {
	rc := &RunCoverage{run: run, classes: map[string]map[archKey]CopyClass{}, syncFrom: map[string]string{}}
	lastFull := lastFullRuns(runs)
	for _, a := range run.Archives {
		// The judged run itself anchors too (tests may pass it outside runs).
		if a.Level == 0 && run.ID > lastFull[a.DLE] {
			lastFull[a.DLE] = run.ID
		}
	}
	owes := func(medium string, a record.Archive) bool {
		age := time.Duration(0)
		if minAge != nil {
			age = minAge(medium)
		}
		return owesTo(run.ID, a, lastFull, age, now)
	}
	mark := func(medium string, k archKey, class CopyClass) {
		m := rc.classes[medium]
		if m == nil {
			m = map[archKey]CopyClass{}
			rc.classes[medium] = m
		}
		if class > m[k] {
			m[k] = class
		}
	}
	// The landing routes: each archive is owed to every medium on its DLE's route.
	// A DLE absent from routes (removed from the config) is owed nowhere.
	isLanding := map[string]bool{}
	for _, media := range routes {
		for _, m := range media {
			isLanding[m] = true
		}
	}
	for _, a := range run.Archives {
		for _, m := range routes[a.DLE] {
			if owes(m, a) {
				mark(m, archKey{a.DLE, a.Level}, CopyRouted)
			} else {
				mark(m, archKey{a.DLE, a.Level}, CopyAged)
			}
		}
	}
	// The sync rules' promises.
	for _, r := range rules {
		if r.To == "" || r.To == r.From || !ruleSelects(run, r, runs, placements) {
			continue
		}
		if r.From == "" {
			// An auto-source rule tops up "the landing" — with per-DLE routing that
			// means each archive's own route, which the routed marks above already
			// cover (and SyncTo scopes the same way). Only a target that is no
			// DLE's landing is a whole-run mirror.
			if isLanding[r.To] {
				continue
			}
			// An auto promise ages like a route does (SyncTo's auto mode skips
			// what the target is no longer owed, so the display and the sync
			// backlog stay one computation).
			for _, a := range run.Archives {
				if owes(r.To, a) {
					mark(r.To, archKey{a.DLE, a.Level}, CopyPromised)
				} else {
					mark(r.To, archKey{a.DLE, a.Level}, CopyAged)
				}
			}
		} else {
			// An explicit-source rule mirrors that medium: it promises exactly what
			// the source copy holds today (copySets' selection) — never aged, because
			// an explicit mirror is bounded by the source's own retention instead.
			src := placementOnMedium(placements(run.ID), r.From)
			for _, a := range run.Archives {
				if src.Holds(a.DLE, a.Level) {
					mark(r.To, archKey{a.DLE, a.Level}, CopyPromised)
				}
			}
		}
		if _, ok := rc.syncFrom[r.To]; !ok {
			rc.syncFrom[r.To] = r.From
		}
	}
	return rc
}

// ruleSelects reports whether the rule's selection window (the last N runs of its
// source) includes the run — mirroring SyncTo's candidates + applySelection, so
// the coverage display and the sync backlog agree about what a bounded rule owes.
func ruleSelects(run *catalog.Run, r config.SyncRule, runs []*catalog.Run, placements func(string) []catalog.Placement) bool {
	if r.Last <= 0 {
		return true
	}
	candidates := runs
	if r.From != "" {
		kept := make([]*catalog.Run, 0, len(runs))
		for _, s := range runs {
			if placementOnMedium(placements(s.ID), r.From).Medium != "" {
				kept = append(kept, s)
			}
		}
		candidates = kept
	}
	for _, s := range applySelection(candidates, SyncSelection{Last: r.Last}) {
		if s.ID == run.ID {
			return true
		}
	}
	return false
}

// owesTo reports whether a medium with retention floor minAge is still owed an
// archive of run runID: the archive is on its DLE's live recovery chain (at or
// after the DLE's newest full in lastFull — the chain retention.Compute pins on
// every medium), or still within minAge. What falls outside is exactly what a
// prune on that medium may delete, so nothing may demand it there: coverage
// classes it CopyAged and SyncTo's auto mode skips it (copying it back would
// only churn — the next make-room reclaims it again). A DLE with no known full
// is owed everywhere, the safe default.
func owesTo(runID string, a record.Archive, lastFull map[string]string, minAge time.Duration, now time.Time) bool {
	if lf, ok := lastFull[a.DLE]; !ok || runID >= lf {
		return true
	}
	return minAge > 0 && !a.CreatedAt.IsZero() && now.Sub(a.CreatedAt) < minAge
}

// lastFullRuns maps each DLE to the run id of its newest full across runs — the
// start of its live recovery chain, the anchor owesTo judges against. Run ids
// are clock-minted, so string order is run order.
func lastFullRuns(runs []*catalog.Run) map[string]string {
	last := map[string]string{}
	for _, s := range runs {
		for _, a := range s.Archives {
			if a.Level == 0 && s.ID > last[a.DLE] {
				last[a.DLE] = s.ID
			}
		}
	}
	return last
}

// placementOnMedium finds the run's copy on a medium among its placements; the
// zero Placement (which holds nothing) when the medium has no copy.
func placementOnMedium(ps []catalog.Placement, medium string) catalog.Placement {
	for _, p := range ps {
		if p.Medium == medium {
			return p
		}
	}
	return catalog.Placement{}
}

// Class is the expectation for one archive on one medium.
func (rc *RunCoverage) Class(medium, dle string, level int) CopyClass {
	return rc.classes[medium][archKey{dle, level}]
}

// ExpectedMedia lists the media expected to hold any of the run's archives
// (routed or promised), sorted — the rows a coverage display must show even when
// a medium has no placement at all (a lane that tripped before writing anything,
// or a landing/sync target added since the run). A medium whose every
// expectation has decayed to CopyAged is not expected: the run has rotated out
// of its retention window, so its absence there says nothing.
func (rc *RunCoverage) ExpectedMedia() []string {
	names := make([]string, 0, len(rc.classes))
	for m, ks := range rc.classes {
		for _, class := range ks {
			if class >= CopyPromised {
				names = append(names, m)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

// SyncSource names the source of the sync rule promising archives to the medium
// ("" when the rule resolves its source per run, or when nothing is promised).
func (rc *RunCoverage) SyncSource(medium string) string { return rc.syncFrom[medium] }

// CopyJudgment weighs one medium's copy of a run against its expectation. Held
// counts every archive the copy holds, expected there or not; the Routed and
// Promised pairs count the expectation and how much of it is met.
type CopyJudgment struct {
	Held         int // archives the copy holds, expected or not
	Routed       int // archives whose landing route includes the medium
	RoutedHeld   int
	Promised     int // archives a sync rule promises to the medium
	PromisedHeld int
	Aged         int // archives once owed here but past the medium's retention window (a gap is rotation, a copy is history)
}

// MissingRouted is the copy's real gap: routed archives the medium lacks.
func (j CopyJudgment) MissingRouted() int { return j.Routed - j.RoutedHeld }

// Behind is the copy's sync lag: promised archives not yet mirrored.
func (j CopyJudgment) Behind() int { return j.Promised - j.PromisedHeld }

// Expected is what the medium is supposed to hold, on either promise.
func (j CopyJudgment) Expected() int { return j.Routed + j.Promised }

// ExpectedHeld is how much of the expectation is met.
func (j CopyJudgment) ExpectedHeld() int { return j.RoutedHeld + j.PromisedHeld }

// Judge weighs one medium's copy of the run; p may be the zero Placement for a
// medium with no copy at all (then Held is 0 and every expectation is a gap).
func (rc *RunCoverage) Judge(medium string, p catalog.Placement) CopyJudgment {
	var j CopyJudgment
	for _, a := range rc.run.Archives {
		held := p.Holds(a.DLE, a.Level)
		if held {
			j.Held++
		}
		switch rc.Class(medium, a.DLE, a.Level) {
		case CopyRouted:
			j.Routed++
			if held {
				j.RoutedHeld++
			}
		case CopyPromised:
			j.Promised++
			if held {
				j.PromisedHeld++
			}
		case CopyAged:
			j.Aged++
		}
	}
	return j
}

// routedScope is the set of DLE slugs whose landing route includes the medium,
// or nil when no DLE routes there — the distinction SyncTo's auto mode keys on
// (top up a landing's own route vs mirror a whole source onto a vault). routes is
// the current-intent map (promiseRoutes), so pattern children scope correctly.
func routedScope(routes map[string][]string, medium string) map[string]bool {
	var set map[string]bool
	for dle, media := range routes {
		for _, m := range media {
			if m == medium {
				if set == nil {
					set = map[string]bool{}
				}
				set[dle] = true
			}
		}
	}
	return set
}

// scopedArchives narrows archives to the DLEs in scope; a nil scope keeps all.
func scopedArchives(archives []record.Archive, scope map[string]bool) []record.Archive {
	if scope == nil {
		return archives
	}
	kept := make([]record.Archive, 0, len(archives))
	for _, a := range archives {
		if scope[a.DLE] {
			kept = append(kept, a)
		}
	}
	return kept
}

// SyncLag is one sync rule's live backlog: the runs (and bytes) its target has
// not mirrored yet. Lag, not error — the next `nb sync` closes it.
type SyncLag struct {
	To    string
	From  string // the rule's source ("" = resolved per run)
	Runs  int
	Bytes int64
}

// SyncLags computes each configured sync rule's backlog (a dry-run SyncTo), for
// status displays. A rule that cannot be evaluated (e.g. its medium was removed)
// is skipped: config validation owns that complaint, not a status page.
func (e *Engine) SyncLags() []SyncLag {
	var out []SyncLag
	for _, r := range e.cfg.Sync {
		rep, err := e.cop.SyncTo(r.From, r.To, SyncSelection{Last: r.Last}, false, false, nil)
		if err != nil {
			continue
		}
		out = append(out, SyncLag{To: r.To, From: r.From, Runs: len(rep.Items), Bytes: rep.Bytes()})
	}
	return out
}
