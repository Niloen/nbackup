package drill

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// mkSlot tags each archive with the slot id and returns them (Select works on archives,
// each carrying its slot tag). date is implied by the id, so it is unused here.
func mkSlot(id, date string, archives ...record.Archive) []record.Archive {
	_ = date
	for i := range archives {
		archives[i].Slot = id
	}
	return archives
}

// cat flattens several slots' archives into the one corpus Select takes.
func cat(slots ...[]record.Archive) []record.Archive {
	var out []record.Archive
	for _, s := range slots {
		out = append(out, s...)
	}
	return out
}

func arch(dle string, level int) record.Archive {
	return record.Archive{DLE: dle, Level: level, Archiver: "gnutar", Compress: "none"}
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
	slots := cat(
		mkSlot("slot-2026-06-20", "2026-06-20", arch("a", 0), arch("b", 0), arch("c", 0)),
		mkSlot("slot-2026-06-21", "2026-06-21", arch("b", 1)),
		mkSlot("slot-2026-06-22", "2026-06-22", arch("b", 2)),
	)
	dles := []string{"a", "b", "c"}
	asOf := "2026-06-24"

	// Cold start: every DLE never drilled. Sample of 2 picks the riskiest two; b
	// (chain 3) must be among them.
	led := &Ledger{Records: map[string]Record{}}
	got := Select(dles, slots, asOf, led, 30*24*time.Hour, 2, now)
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
	got = Select(dles, slots, asOf, led, 30*24*time.Hour, 5, now)
	for _, tg := range got {
		if tg.DLE == "b" {
			t.Fatalf("b is covered within the window and must not be reselected: %+v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("after covering b, due set = %d, want 2 (a, c)", len(got))
	}
}

// TestSelectPointInTime confirms a target resolves to the newest slot at or before
// the as-of date, not the latest overall.
func TestSelectPointInTime(t *testing.T) {
	now := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	slots := cat(
		mkSlot("slot-2026-06-20", "2026-06-20", arch("a", 0)),
		mkSlot("slot-2026-06-22", "2026-06-22", arch("a", 1)),
		mkSlot("slot-2026-06-24", "2026-06-24", arch("a", 1)),
	)
	led := &Ledger{Records: map[string]Record{}}
	got := Select([]string{"a"}, slots, "2026-06-22", led, 30*24*time.Hour, 1, now)
	if len(got) != 1 || got[0].SlotID != "slot-2026-06-22" {
		t.Fatalf("point-in-time target = %+v, want slot-2026-06-22", got)
	}
	if got[0].ChainLen != 2 {
		t.Fatalf("chain as of 2026-06-22 = %d, want 2 (L0+L1)", got[0].ChainLen)
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
