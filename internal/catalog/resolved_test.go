package catalog

import "testing"

// TestResolvedSetRoundTrip: RecordResolved persists the latest run's resolved set and a
// re-Open reads it back; a second record REPLACES it (only current intent is kept); a
// fresh catalog (or one rebuilt from media, which never records intent) returns nil.
func TestResolvedSetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cat, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cat.LatestResolved(); got != nil {
		t.Fatalf("fresh catalog must have no resolved set, got %v", got)
	}

	set1 := []ResolvedDLE{
		{DLE: "fs-data-alice", Host: "fs", Source: "/data/alice", DumpType: "big"},
		{DLE: "fs-data", Host: "fs", Source: "/data", DumpType: "big"},
	}
	if err := cat.RecordResolved("run-2026-07-09.010101", set1); err != nil {
		t.Fatal(err)
	}

	cat2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := cat2.LatestResolved()
	if len(got) != 2 || got[0] != set1[0] || got[1] != set1[1] {
		t.Fatalf("round-trip mismatch: %v != %v", got, set1)
	}

	// The next run's record replaces the previous — retirement by omission.
	if err := cat2.RecordResolved("run-2026-07-10.010101", set1[:1]); err != nil {
		t.Fatal(err)
	}
	cat3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cat3.LatestResolved(); len(got) != 1 || got[0].DLE != "fs-data-alice" {
		t.Fatalf("latest set must replace, got %v", got)
	}
}
