package engine

import (
	"sort"

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

// CopyClass is one archive's relationship to one medium, in ascending promise
// strength (a route outranks a sync promise when both name the same medium).
type CopyClass int

const (
	// CopyNone: the medium is neither in the archive's landing route nor a sync
	// target for it. A copy there is a bonus; a gap there is nobody's problem.
	CopyNone CopyClass = iota
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

// RunCoverage judges run's copies against this engine's config; see JudgeRun.
func (e *Engine) RunCoverage(run *catalog.Run) *RunCoverage {
	return JudgeRun(run, e.cfg.Routes(), e.cfg.Sync, e.cat.Runs(), e.cat.Placements)
}

// JudgeRun builds a run's expectation map from the DLE landing routes
// (config.Routes) and the sync rules. runs and placements feed the rule
// evaluation: a rule bounded by `last:` only promises the runs inside its
// window, and an explicit-source rule only promises what its source copy holds.
func JudgeRun(run *catalog.Run, routes map[string][]string, rules []config.SyncRule, runs []*catalog.Run, placements func(runID string) []catalog.Placement) *RunCoverage {
	rc := &RunCoverage{run: run, classes: map[string]map[archKey]CopyClass{}, syncFrom: map[string]string{}}
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
			mark(m, archKey{a.DLE, a.Level}, CopyRouted)
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
			for _, a := range run.Archives {
				mark(r.To, archKey{a.DLE, a.Level}, CopyPromised)
			}
		} else {
			// An explicit-source rule mirrors that medium: it promises exactly what
			// the source copy holds today (copySets' selection).
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
// or a landing/sync target added since the run).
func (rc *RunCoverage) ExpectedMedia() []string {
	names := make([]string, 0, len(rc.classes))
	for m := range rc.classes {
		names = append(names, m)
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
		}
	}
	return j
}

// routedScope is the set of DLE slugs whose landing route includes the medium,
// or nil when no DLE routes there — the distinction SyncTo's auto mode keys on
// (top up a landing's own route vs mirror a whole source onto a vault).
func routedScope(cfg *config.Config, medium string) map[string]bool {
	var set map[string]bool
	for dle, media := range cfg.Routes() {
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
