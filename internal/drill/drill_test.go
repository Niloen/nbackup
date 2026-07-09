package drill

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// mkRun tags each archive with the run id and returns them (Select works on archives,
// each carrying its run tag). date is implied by the id, so it is unused here.
func mkRun(id, date string, archives ...record.Archive) []record.Archive {
	_ = date
	for i := range archives {
		archives[i].Run = id
	}
	return archives
}

// cat flattens several runs' archives into the one corpus Select takes.
func cat(runs ...[]record.Archive) []record.Archive {
	var out []record.Archive
	for _, s := range runs {
		out = append(out, s...)
	}
	return out
}

func arch(dle string, level int) record.Archive {
	return record.Archive{DLE: dle, Level: level, ArchiverType: "gnutar", Compress: "none"}
}

// TestLedgerRoundTrip checks the ledger persists and reloads, and that Drilled honors
// the window and pass/fail.
func TestLedgerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l, err := Load(dir) // absent file -> empty
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Records) != 0 {
		t.Fatalf("fresh ledger not empty: %+v", l.Records)
	}
	now := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	l.Update(Record{DLE: "a", LastDrill: now, Tier: "structural", OK: true})
	l.Update(Record{DLE: "b", LastDrill: now.AddDate(0, 0, -40), OK: true}) // stale
	l.Update(Record{DLE: "c", LastDrill: now, OK: false})                   // failing
	if err := l.Save(dir); err != nil {
		t.Fatal(err)
	}

	l2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	win := 30 * 24 * time.Hour
	if !l2.Drilled("a", win, now) {
		t.Errorf("a should be covered")
	}
	if l2.Drilled("b", win, now) {
		t.Errorf("b is stale (40d > 30d window), should not be covered")
	}
	if l2.Drilled("c", win, now) {
		t.Errorf("c failed, should not be covered")
	}
	if l2.Drilled("missing", win, now) {
		t.Errorf("never-recorded DLE should not be covered")
	}
}

// TestSelectRotatesAndRanks checks the risk-biased selection: never-drilled before
// covered, longest chain breaks ties, and the window/sample bound the set.
func TestSelectRotatesAndRanks(t *testing.T) {
	now := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	// a: full only (chain 1). b: full + 2 incrementals (chain 3). c: full only.
	runs := cat(
		mkRun("run-2026-06-20.001", "2026-06-20", arch("a", 0), arch("b", 0), arch("c", 0)),
		mkRun("run-2026-06-21.001", "2026-06-21", arch("b", 1)),
		mkRun("run-2026-06-22.001", "2026-06-22", arch("b", 2)),
	)
	dles := []string{"a", "b", "c"}
	asOf := "2026-06-24"

	// Cold start: every DLE never drilled. Sample of 2 picks the riskiest two; b
	// (chain 3) must be among them.
	led := &Ledger{Records: map[string]Record{}}
	got := Select(dles, runs, asOf, led, 30*24*time.Hour, 2, now)
	if len(got) != 2 {
		t.Fatalf("sample=2 returned %d targets", len(got))
	}
	if got[0].DLE != "b" {
		t.Fatalf("riskiest target = %q, want b (longest chain)", got[0].DLE)
	}
	if got[0].ChainLen != 3 {
		t.Fatalf("b chain length = %d, want 3", got[0].ChainLen)
	}

	// Mark b covered recently; now b is excluded and a/c (still due) are selected.
	led.Update(Record{DLE: "b", LastDrill: now, OK: true})
	got = Select(dles, runs, asOf, led, 30*24*time.Hour, 5, now)
	for _, tg := range got {
		if tg.DLE == "b" {
			t.Fatalf("b is covered within the window and must not be reselected: %+v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("after covering b, due set = %d, want 2 (a, c)", len(got))
	}
}

// TestSelectPointInTime confirms a target resolves to the newest run at or before
// the as-of date, not the latest overall.
func TestSelectPointInTime(t *testing.T) {
	now := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	runs := cat(
		mkRun("run-2026-06-20.001", "2026-06-20", arch("a", 0)),
		mkRun("run-2026-06-22.001", "2026-06-22", arch("a", 1)),
		mkRun("run-2026-06-24.001", "2026-06-24", arch("a", 1)),
	)
	led := &Ledger{Records: map[string]Record{}}
	got := Select([]string{"a"}, runs, "2026-06-22", led, 30*24*time.Hour, 1, now)
	if len(got) != 1 || got[0].RunID != "run-2026-06-22.001" {
		t.Fatalf("point-in-time target = %+v, want run-2026-06-22.001", got)
	}
	if got[0].ChainLen != 2 {
		t.Fatalf("chain as of 2026-06-22 = %d, want 2 (L0+L1)", got[0].ChainLen)
	}
}

// TestClassRoundTrip pins the persisted contract the ledger relies on: every Class
// serializes to a stable token and parses back to the same Class. This is what lets a
// report recover the failure class recorded on disk and print its Remedy.
func TestClassRoundTrip(t *testing.T) {
	for _, c := range []Class{ClassNone, ClassIntegrity, ClassPipeline, ClassChain, ClassMissing, ClassSkipped} {
		if got := ParseClass(c.String()); got != c {
			t.Errorf("ParseClass(%q) = %v, want %v (round-trip broken)", c.String(), got, c)
		}
	}
	// An unknown token (including "ok", the passing token) resolves to ClassNone.
	for _, tok := range []string{"ok", "", "bogus"} {
		if got := ParseClass(tok); got != ClassNone {
			t.Errorf("ParseClass(%q) = %v, want ClassNone", tok, got)
		}
	}
	// An out-of-range class stringifies to a diagnostic token, not a real one.
	if s := Class(99).String(); s != "class(99)" {
		t.Errorf("Class(99).String() = %q, want class(99)", s)
	}
}

// TestClassIsFailureAndRemedy checks the failure taxonomy: the four hard faults fail a
// drill and carry non-empty guidance; a pass fails nothing and has no remedy; a skip is
// not a failure but still carries operator guidance.
func TestClassIsFailureAndRemedy(t *testing.T) {
	for _, c := range []Class{ClassIntegrity, ClassPipeline, ClassChain, ClassMissing} {
		if !c.IsFailure() {
			t.Errorf("%v should be a failure", c)
		}
		if c.Remedy() == "" {
			t.Errorf("%v should have a remedy", c)
		}
	}
	if ClassNone.IsFailure() || ClassSkipped.IsFailure() {
		t.Error("ClassNone and ClassSkipped must not be failures")
	}
	if ClassNone.Remedy() != "" {
		t.Error("ClassNone should have no remedy")
	}
	if ClassSkipped.Remedy() == "" {
		t.Error("ClassSkipped should carry operator guidance even though it is not a failure")
	}
}

// TestCoverage checks the pure coverage computation the drill audit and report share:
// which configured DLEs have never been drilled, and how many are not covered within
// the window (never-drilled, stale, or failing — anything Drilled rejects).
func TestCoverage(t *testing.T) {
	now := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	win := 30 * 24 * time.Hour
	l := &Ledger{Records: map[string]Record{}}
	l.Update(Record{DLE: "a", LastDrill: now, OK: true})                    // covered
	l.Update(Record{DLE: "b", LastDrill: now.AddDate(0, 0, -40), OK: true}) // stale
	l.Update(Record{DLE: "c", LastDrill: now, OK: false})                   // failing
	l.Update(Record{DLE: "e", OK: true})                                    // recorded but zero LastDrill = never
	// "d" is not recorded at all.

	never, overdue := l.Coverage([]string{"a", "b", "c", "d", "e"}, win, now)
	wantNever := []string{"d", "e"}
	if len(never) != len(wantNever) {
		t.Fatalf("never = %v, want %v", never, wantNever)
	}
	for i := range wantNever {
		if never[i] != wantNever[i] {
			t.Fatalf("never = %v, want %v", never, wantNever)
		}
	}
	if overdue != 4 { // b (stale), c (fail), d (unrecorded), e (zero) — a is covered
		t.Fatalf("overdue = %d, want 4", overdue)
	}
}

// TestSorted confirms records render in stable DLE-name order regardless of map
// iteration order.
func TestSorted(t *testing.T) {
	l := &Ledger{Records: map[string]Record{
		"c": {DLE: "c"}, "a": {DLE: "a"}, "b": {DLE: "b"},
	}}
	got := l.Sorted()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Sorted returned %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].DLE != want[i] {
			t.Fatalf("Sorted order = %v, want %v", got, want)
		}
	}
}

// TestParseTier covers the tier token mapping and its default.
func TestParseTier(t *testing.T) {
	cases := map[string]Tier{"": TierStructural, "checksum": TierChecksum, "structural": TierStructural, "chain": TierChain, "stock": TierStock}
	for in, want := range cases {
		got, err := ParseTier(in)
		if err != nil || got != want {
			t.Fatalf("ParseTier(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseTier("bogus"); err == nil {
		t.Fatal("ParseTier(bogus) should error")
	}
}
