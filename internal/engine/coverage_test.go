package engine

import (
	"testing"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/record"
)

// covFixture is a mixed-route run: etc lands on [c2, vtape], home on [c2, gdrive]
// — the config that used to make every landing read as a partial copy of the
// whole run. c2 holds both archives; each side landing holds only its own.
func covFixture() (*catalog.Run, map[string][]string, map[string][]catalog.Placement) {
	run := &catalog.Run{ID: "run-2026-07-08.020000", Archives: []record.Archive{
		{DLE: "etc", Level: 0, Compressed: 100},
		{DLE: "home", Level: 0, Compressed: 400},
	}}
	routes := map[string][]string{
		"etc":  {"c2", "vtape"},
		"home": {"c2", "gdrive"},
	}
	placements := map[string][]catalog.Placement{run.ID: {
		{Medium: "c2", Archives: []catalog.PlacedArchive{{DLE: "etc", Level: 0}, {DLE: "home", Level: 0}}},
		{Medium: "vtape", Archives: []catalog.PlacedArchive{{DLE: "etc", Level: 0}}},
		{Medium: "gdrive", Archives: []catalog.PlacedArchive{{DLE: "home", Level: 0}}},
	}}
	return run, routes, placements
}

// TestJudgeRunRoutes: each medium is judged only against the archives its landing
// routes owe it — a side landing holding exactly its routed subset is complete,
// not partial, and an archive it never was to hold classes as CopyNone.
func TestJudgeRunRoutes(t *testing.T) {
	run, routes, placements := covFixture()
	lookup := func(id string) []catalog.Placement { return placements[id] }
	rc := JudgeRun(run, routes, nil, []*catalog.Run{run}, lookup)

	byMedium := map[string]catalog.Placement{}
	for _, p := range placements[run.ID] {
		byMedium[p.Medium] = p
	}
	for medium, want := range map[string]CopyJudgment{
		"c2":     {Held: 2, Routed: 2, RoutedHeld: 2},
		"vtape":  {Held: 1, Routed: 1, RoutedHeld: 1},
		"gdrive": {Held: 1, Routed: 1, RoutedHeld: 1},
	} {
		if got := rc.Judge(medium, byMedium[medium]); got != want {
			t.Errorf("Judge(%s) = %+v, want %+v", medium, got, want)
		}
	}
	if got := rc.Class("gdrive", "etc", 0); got != CopyNone {
		t.Errorf("etc on gdrive classes %v, want CopyNone (never routed there)", got)
	}
	// A wholly absent routed medium is still expected — the zero placement's
	// judgment reports the gap (a lane that tripped before writing anything).
	if got := rc.Judge("vtape", catalog.Placement{}); got.MissingRouted() != 1 {
		t.Errorf("absent vtape MissingRouted = %d, want 1", got.MissingRouted())
	}
	if got, want := rc.ExpectedMedia(), []string{"c2", "gdrive", "vtape"}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("ExpectedMedia = %v, want %v", got, want)
	}
	// A DLE removed from the config (absent from routes) is owed nowhere.
	rcNone := JudgeRun(run, map[string][]string{}, nil, []*catalog.Run{run}, lookup)
	if got := rcNone.Judge("c2", byMedium["c2"]); got.Expected() != 0 || got.Held != 2 {
		t.Errorf("unrouted judge = %+v, want expectation 0 with Held 2 (a bonus copy)", got)
	}
}

// TestJudgeRunSyncPromises: sync rules promise, never alarm. An auto-source rule
// to a non-landing mirrors the whole run; one to a landing adds nothing (the route
// already owes it, and SyncTo scopes the same way); an explicit-source rule
// promises exactly what its source copy holds; and a route outranks a promise.
func TestJudgeRunSyncPromises(t *testing.T) {
	run, routes, placements := covFixture()
	lookup := func(id string) []catalog.Placement { return placements[id] }
	rules := []config.SyncRule{
		{To: "offsite"},              // auto source, not a landing: whole-run mirror
		{To: "gdrive"},               // auto source, a landing: nothing beyond its route
		{To: "vault", From: "vtape"}, // explicit source: promises vtape's holdings
		{To: "c2", From: "vtape"},    // routed there already: the route wins
	}
	rc := JudgeRun(run, routes, rules, []*catalog.Run{run}, lookup)

	if got := rc.Judge("offsite", catalog.Placement{}); got.Promised != 2 || got.Behind() != 2 || got.Routed != 0 {
		t.Errorf("offsite judge = %+v, want 2 promised, 2 behind", got)
	}
	if got := rc.Class("gdrive", "etc", 0); got != CopyNone {
		t.Errorf("auto rule to a landing promised etc on gdrive (%v); its route owes it nothing", got)
	}
	// vault is promised only vtape's holdings (etc), not the whole run.
	if got := rc.Judge("vault", catalog.Placement{}); got.Promised != 1 || got.Routed != 0 {
		t.Errorf("vault judge = %+v, want exactly 1 promised (vtape holds only etc)", got)
	}
	if got := rc.Class("c2", "etc", 0); got != CopyRouted {
		t.Errorf("etc on c2 classes %v, want CopyRouted (route outranks promise)", got)
	}
	if got := rc.SyncSource("vault"); got != "vtape" {
		t.Errorf("SyncSource(vault) = %q, want vtape", got)
	}
}

// TestJudgeRunRuleLastWindow: a `last: N` bounded rule only promises the runs
// inside its window — an old run outside it is not "behind" forever.
func TestJudgeRunRuleLastWindow(t *testing.T) {
	old := &catalog.Run{ID: "run-2026-07-01.020000", Archives: []record.Archive{{DLE: "etc", Level: 0}}}
	cur := &catalog.Run{ID: "run-2026-07-08.020000", Archives: []record.Archive{{DLE: "etc", Level: 0}}}
	runs := []*catalog.Run{old, cur} // oldest-first, like catalog.Runs()
	routes := map[string][]string{"etc": {"c2"}}
	rules := []config.SyncRule{{To: "offsite", Last: 1}}
	lookup := func(string) []catalog.Placement { return nil }

	if got := JudgeRun(cur, routes, rules, runs, lookup).Judge("offsite", catalog.Placement{}); got.Promised != 1 {
		t.Errorf("newest run: offsite promised = %d, want 1", got.Promised)
	}
	if got := JudgeRun(old, routes, rules, runs, lookup).Judge("offsite", catalog.Placement{}); got.Promised != 0 {
		t.Errorf("run outside the rule's window: offsite promised = %d, want 0", got.Promised)
	}
}
