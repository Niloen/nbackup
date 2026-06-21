package slot

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
		{"2026-06-21", 1, "slot-2026-06-21"},
		{"2026-06-21", 2, "slot-2026-06-21.2"},
		{"2026-06-21", 10, "slot-2026-06-21.10"},
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

// TestOrderingSequenceNumeric ensures same-day slots sort by numeric sequence,
// not lexicographically (so .10 follows .2).
func TestOrderingSequenceNumeric(t *testing.T) {
	slots := []*Slot{
		{Date: "2026-06-21", Sequence: 10},
		{Date: "2026-06-21", Sequence: 2},
		{Date: "2026-06-20", Sequence: 1},
		{Date: "2026-06-21", Sequence: 1},
	}
	sort.Slice(slots, func(i, j int) bool { return Less(slots[i], slots[j]) })
	want := []struct {
		date string
		seq  int
	}{
		{"2026-06-20", 1},
		{"2026-06-21", 1},
		{"2026-06-21", 2},
		{"2026-06-21", 10},
	}
	for i, w := range want {
		if slots[i].Date != w.date || slots[i].Sequence != w.seq {
			t.Errorf("position %d = (%s,%d), want (%s,%d)", i, slots[i].Date, slots[i].Sequence, w.date, w.seq)
		}
	}
}
