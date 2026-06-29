package record

import (
	"sort"
	"testing"
	"time"
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

// TestParseIDToleratesLegacy keeps ParseID reading ids written by the older,
// pre-fixed-width scheme — a bare "slot-DATE" (the day's first run) and an
// unpadded ".N" — so a catalog scan of media that predates the padding still
// groups their archives correctly.
func TestParseIDToleratesLegacy(t *testing.T) {
	cases := []struct {
		id   string
		date string
		seq  int
	}{
		{"slot-2026-06-21", "2026-06-21", 1},
		{"slot-2026-06-21.8", "2026-06-21", 8},
	}
	for _, c := range cases {
		d, s, err := ParseID(c.id)
		if err != nil {
			t.Fatalf("ParseID(%q): %v", c.id, err)
		}
		if d != c.date || s != c.seq {
			t.Errorf("ParseID(%q) = (%q,%d), want (%q,%d)", c.id, d, s, c.date, c.seq)
		}
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

// TestLifecycle covers the new slot lifecycle API: AddArchive keeps TotalBytes
// in sync, and Seal refuses an empty slot but stamps a populated one.
func TestLifecycle(t *testing.T) {
	now := time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC)
	s := NewSlot("slot-2026-06-21", "2026-06-21", 1, "nbdump", now)
	if s.Status != StatusOpen || s.IsSealed() {
		t.Fatalf("new slot should be open, got %q", s.Status)
	}

	// Sealing an empty slot is refused (no recovery point from nothing).
	if err := s.Seal(now); err == nil {
		t.Fatal("expected Seal to fail on an empty slot")
	}

	s.AddArchive(Archive{DLE: "h-data", Level: 0, Compressed: 100})
	s.AddArchive(Archive{DLE: "h-etc", Level: 0, Compressed: 23})
	if s.TotalBytes != 123 {
		t.Errorf("TotalBytes = %d, want 123 (kept in sync by AddArchive)", s.TotalBytes)
	}

	sealedAt := now.Add(time.Minute)
	if err := s.Seal(sealedAt); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !s.IsSealed() || !s.SealedAt.Equal(sealedAt) {
		t.Errorf("after Seal: sealed=%v at=%v", s.IsSealed(), s.SealedAt)
	}
}
