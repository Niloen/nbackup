package record

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

// TestCommitRoundTrip pins that a commit footer marshals and parses back to an equal
// archive — the marker that makes a dump durable must survive a write/read cycle
// intact. A malformed footer is a parse error, not a zero-value archive.
func TestCommitRoundTrip(t *testing.T) {
	a := Archive{
		Run: "run-2026-06-21.020000", DLE: "app01-home", Host: "app01", Path: "/home",
		Archiver: "gnutar", Compress: "none", Encrypt: "none", Level: 1,
		Compressed: 1024, Uncompressed: 2048, FileCount: 7, SHA256: "deadbeef",
		Parts: 2, BaseRun: "run-2026-06-20.020000",
		CreatedAt: time.Date(2026, 6, 21, 14, 30, 0, 0, time.UTC),
		Members:   []string{"./", "./etc/hosts"},
	}
	data, err := MarshalCommit(a)
	if err != nil {
		t.Fatalf("MarshalCommit: %v", err)
	}
	got, err := ParseCommit(data)
	if err != nil {
		t.Fatalf("ParseCommit: %v", err)
	}
	if !reflect.DeepEqual(*got, a) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", *got, a)
	}

	if _, err := ParseCommit([]byte("{not valid json")); err == nil {
		t.Error("ParseCommit of malformed input should error")
	}
}

// TestIDAndParse pins the id shape (run-DATE.HHMMSS from the run's start instant)
// and that ParseID and IDTime both recover what IDFromTime encoded.
func TestIDAndParse(t *testing.T) {
	loc := time.FixedZone("east", 2*3600)
	cases := []struct {
		instant   time.Time
		id        string
		date      string
		timeOfDay int
	}{
		{time.Date(2026, 6, 21, 2, 0, 0, 0, loc), "run-2026-06-21.020000", "2026-06-21", 20000},
		{time.Date(2026, 6, 21, 14, 30, 5, 0, loc), "run-2026-06-21.143005", "2026-06-21", 143005},
		{time.Date(2026, 6, 21, 0, 0, 0, 0, loc), "run-2026-06-21.000000", "2026-06-21", 0},
	}
	for _, c := range cases {
		if got := IDFromTime(c.instant); got != c.id {
			t.Errorf("IDFromTime(%s) = %q, want %q", c.instant, got, c.id)
		}
		d, s, err := ParseID(c.id)
		if err != nil {
			t.Fatalf("ParseID(%q): %v", c.id, err)
		}
		if d != c.date || s != c.timeOfDay {
			t.Errorf("ParseID(%q) = (%q,%d), want (%q,%d)", c.id, d, s, c.date, c.timeOfDay)
		}
		back, err := IDTime(c.id, loc)
		if err != nil {
			t.Fatalf("IDTime(%q): %v", c.id, err)
		}
		if !back.Equal(c.instant) {
			t.Errorf("IDTime(%q) = %s, want %s", c.id, back, c.instant)
		}
	}
	if _, err := IDTime("run-2026-06-21.9999", time.UTC); err == nil {
		t.Error("IDTime of a malformed suffix should error")
	}
	if _, err := IDTime("2026-06-21.020000", time.UTC); err == nil {
		t.Error("IDTime without the run- prefix should error")
	}
}

// TestParseIDRejectsSuffixless pins the single canonical id shape: a suffix-less
// "run-DATE" is not a valid id and is rejected, so a bare date can never masquerade
// as a run id.
func TestParseIDRejectsSuffixless(t *testing.T) {
	if _, _, err := ParseID("run-2026-06-21"); err == nil {
		t.Errorf("ParseID(%q) succeeded; want a suffix-less id to be rejected", "run-2026-06-21")
	}
	if _, _, err := ParseID("2026-06-21.020000"); err == nil {
		t.Errorf("ParseID without the run- prefix should be rejected")
	}
}

// TestIDSortsLexically is the regression test for the original bug: an id scheme
// with a bare or unpadded suffix breaks lexical ordering once a delimiter follows
// the id — an object-store key "runs/<id>/…", '/' = 0x2F — because '.' (0x2E)
// precedes '/'. A fixed-width, always-suffixed id sorts in run order under a plain
// string compare, with or without a trailing delimiter.
func TestIDSortsLexically(t *testing.T) {
	chronological := []string{
		IDFromTime(time.Date(2026, 6, 21, 2, 0, 0, 0, time.UTC)),   // first run of the 21st
		IDFromTime(time.Date(2026, 6, 21, 9, 15, 30, 0, time.UTC)), // same-day rerun
		IDFromTime(time.Date(2026, 6, 21, 23, 5, 0, 0, time.UTC)),  // late same-day run
		IDFromTime(time.Date(2026, 6, 22, 2, 0, 0, 0, time.UTC)),   // next day
	}
	for _, suffix := range []string{"", "/"} {
		keys := make([]string, len(chronological))
		for i, id := range chronological {
			keys[i] = id + suffix
		}
		sorted := append([]string(nil), keys...)
		sort.Strings(sorted)
		for i := range keys {
			if sorted[i] != keys[i] {
				t.Errorf("suffix %q: lexical order %v != chronological order %v", suffix, sorted, keys)
				break
			}
		}
	}
}

// TestRunIDLess ensures run ids order by date then time-of-day, and that a
// non-run-shaped id falls back to a plain lexical compare.
func TestRunIDLess(t *testing.T) {
	ids := []string{
		IDFromTime(time.Date(2026, 6, 21, 23, 5, 0, 0, time.UTC)),
		IDFromTime(time.Date(2026, 6, 21, 9, 15, 30, 0, time.UTC)),
		IDFromTime(time.Date(2026, 6, 20, 2, 0, 0, 0, time.UTC)),
		IDFromTime(time.Date(2026, 6, 21, 2, 0, 0, 0, time.UTC)),
	}
	sort.Slice(ids, func(i, j int) bool { return RunIDLess(ids[i], ids[j]) })
	want := []string{
		"run-2026-06-20.020000",
		"run-2026-06-21.020000",
		"run-2026-06-21.091530",
		"run-2026-06-21.230500",
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("position %d = %s, want %s", i, ids[i], want[i])
		}
	}
	if !RunIDLess("a", "b") || RunIDLess("b", "a") {
		t.Errorf("non-run ids should fall back to lexical order")
	}
}
