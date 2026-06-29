package record

import (
	"sort"
	"testing"
)

func TestIDAndParse(t *testing.T) {
	cases := []struct {
		date string
		seq  int
		id   string
	}{
		{"2026-06-21", 1, "slot-2026-06-21.001"},
		{"2026-06-21", 2, "slot-2026-06-21.002"},
		{"2026-06-21", 10, "slot-2026-06-21.010"},
	}
	for _, c := range cases {
		if got := IDFromParts(c.date, c.seq); got != c.id {
			t.Errorf("IDFromParts(%q,%d) = %q, want %q", c.date, c.seq, got, c.id)
		}
		d, s, err := ParseID(c.id)
		if err != nil {
			t.Fatalf("ParseID(%q): %v", c.id, err)
		}
		if d != c.date || s != c.seq {
			t.Errorf("ParseID(%q) = (%q,%d), want (%q,%d)", c.id, d, s, c.date, c.seq)
		}
	}
}

// TestParseIDRejectsSequenceless pins the single canonical id shape: a sequence-less
// "slot-DATE" is not a valid id and is rejected, so a bare date can never masquerade
// as the day's first run. A present-but-unpadded sequence (".8") still parses — the
// padding is only the producer's sort-stability discipline, not a parse requirement.
func TestParseIDRejectsSequenceless(t *testing.T) {
	if _, _, err := ParseID("slot-2026-06-21"); err == nil {
		t.Errorf("ParseID(%q) succeeded; want a sequence-less id to be rejected", "slot-2026-06-21")
	}
	d, s, err := ParseID("slot-2026-06-21.8")
	if err != nil {
		t.Fatalf("ParseID(%q): %v", "slot-2026-06-21.8", err)
	}
	if d != "2026-06-21" || s != 8 {
		t.Errorf("ParseID(%q) = (%q,%d), want (%q,8)", "slot-2026-06-21.8", d, s, "2026-06-21")
	}
}

// TestIDSortsLexically is the regression test for the original bug: the old scheme
// left the day's first run a bare "slot-DATE" and used unpadded sequences, so once
// a delimiter followed the id — an object-store key "slots/<id>/…", '/' = 0x2F — the
// bare run sorted *after* its same-day reruns ('.' = 0x2E precedes '/'), and ".10"
// sorted before ".2". A fixed-width, always-suffixed id sorts in run order under a
// plain string compare, with or without a trailing delimiter.
func TestIDSortsLexically(t *testing.T) {
	chronological := []string{
		IDFromParts("2026-06-21", 1),  // first run of the 21st
		IDFromParts("2026-06-21", 2),  // same-day rerun
		IDFromParts("2026-06-21", 10), // the 10th run — must follow the 2nd, not precede it
		IDFromParts("2026-06-22", 1),  // next day
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

// TestSlotIDLess ensures same-day slot ids order by numeric sequence, not
// lexicographically (so .010 follows .002), and that a non-slot-shaped id falls back
// to a plain lexical compare.
func TestSlotIDLess(t *testing.T) {
	ids := []string{
		IDFromParts("2026-06-21", 10),
		IDFromParts("2026-06-21", 2),
		IDFromParts("2026-06-20", 1),
		IDFromParts("2026-06-21", 1),
	}
	sort.Slice(ids, func(i, j int) bool { return SlotIDLess(ids[i], ids[j]) })
	want := []string{
		IDFromParts("2026-06-20", 1),
		IDFromParts("2026-06-21", 1),
		IDFromParts("2026-06-21", 2),
		IDFromParts("2026-06-21", 10),
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("position %d = %s, want %s", i, ids[i], want[i])
		}
	}
	if !SlotIDLess("a", "b") || SlotIDLess("b", "a") {
		t.Errorf("non-slot ids should fall back to lexical order")
	}
}
