package spool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archiveio"
)

// fanoutSpool wires a spool with two landing lanes ("a", "b") over one holding disk,
// the fixture every fan-out test starts from.
func fanoutSpool(a, b, holding *memStore) *Spool {
	return New(context.Background(), Config{
		Backings: []Backing{
			{Name: "a", Allocs: []archiveio.PartAllocator{a}, Rec: a, Writers: 1},
			{Name: "b", Allocs: []archiveio.PartAllocator{b}, Rec: b, Writers: 1},
		},
		Holding: NewPool([]Disk{{Name: "hd0", Alloc: holding, Storage: holding, Capacity: 0}}),
	})
}

// stageFanout routes one dump through the spool bound for BOTH lanes.
func stageFanout(t *testing.T, sp *Spool, body []byte) error {
	t.Helper()
	aw, err := sp.Ingest("a", "b").NewArchive(archSpec, 1<<20)
	if err != nil {
		return err
	}
	return transferArchive(aw, body)
}

// TestFanoutDrainsBothThenReclaimsOnce: a two-landing route stages once, drains to
// each landing, and reclaims the staged copy exactly once — only after both copies
// committed (a reclaim with a missing copy would trip the fault tests below).
func TestFanoutDrainsBothThenReclaimsOnce(t *testing.T) {
	a, b, holding := newMemStore("a"), newMemStore("b"), newMemStore("holding")
	sp := fanoutSpool(a, b, holding)
	if err := stageFanout(t, sp, []byte("fan me out")); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if a.recordCount() != 1 || b.recordCount() != 1 {
		t.Fatalf("landings recorded a=%d b=%d archives; want 1 each", a.recordCount(), b.recordCount())
	}
	holding.mu.Lock()
	reclaimed := holding.reclaimed
	holding.mu.Unlock()
	if reclaimed != 1 {
		t.Fatalf("holding reclaimed %d times; want exactly 1, after both drains", reclaimed)
	}
	if w := sp.Warnings(); len(w) != 0 {
		t.Fatalf("healthy fan-out produced warnings: %v", w)
	}
}

// TestFanoutOneLaneFaultContinues: one landing's Record faults. The run continues
// (any-lane-suffices): the survivor holds the archive, the staged copy is reclaimed,
// the dead lane is tripped — the NEXT archive skips it without touching the medium —
// and the run carries one warning naming the repair.
func TestFanoutOneLaneFaultContinues(t *testing.T) {
	boom := errors.New("lane b record boom")
	a, b, holding := newMemStore("a"), newMemStore("b"), newMemStore("holding")
	b.recordErr = boom
	sp := fanoutSpool(a, b, holding)
	if err := stageFanout(t, sp, []byte("first archive")); err != nil {
		t.Fatalf("stage 1: %v", err)
	}
	if err := stageFanout(t, sp, []byte("second archive")); err != nil {
		t.Fatalf("stage 2: %v", err)
	}
	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain = %v; a one-lane fault must not fail the run", err)
	}
	if a.recordCount() != 2 {
		t.Fatalf("surviving landing recorded %d archives; want 2", a.recordCount())
	}
	if b.recordCount() != 0 {
		t.Fatalf("dead landing recorded %d archives; want 0", b.recordCount())
	}
	holding.mu.Lock()
	reclaimed := holding.reclaimed
	holding.mu.Unlock()
	if reclaimed != 2 {
		t.Fatalf("holding reclaimed %d times; want 2 (survivor has each archive)", reclaimed)
	}
	w := sp.Warnings()
	if len(w) != 1 {
		t.Fatalf("Warnings() = %v; want exactly one, for lane b", w)
	}
	if !strings.Contains(w[0], `"b"`) || !strings.Contains(w[0], "nb sync --to b") {
		t.Fatalf("warning %q must name the lane and the sync repair", w[0])
	}
}

// A trip counts every archive missing on the dead lane — the one that failed and the
// ones skipped after — so the warning's tally matches what sync will find.
func TestFanoutTripCountsMissing(t *testing.T) {
	a, b, holding := newMemStore("a"), newMemStore("b"), newMemStore("holding")
	b.recordErr = errors.New("down")
	sp := fanoutSpool(a, b, holding)
	for i := 0; i < 3; i++ {
		if err := stageFanout(t, sp, []byte("archive body")); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
	}
	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	w := sp.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "3 archive(s) missing") {
		t.Fatalf("Warnings() = %v; want one warning counting 3 missing archives", w)
	}
}

// TestFanoutAllLanesFaultAborts: every landing on the route fails, so the archive
// landed nowhere — the run must abort and the staged copy must stay for `nb flush`
// (today's single-landing semantics, generalized).
func TestFanoutAllLanesFaultAborts(t *testing.T) {
	a, b, holding := newMemStore("a"), newMemStore("b"), newMemStore("holding")
	a.recordErr = errors.New("a down")
	b.recordErr = errors.New("b down")
	sp := fanoutSpool(a, b, holding)
	if err := stageFanout(t, sp, []byte("nowhere to go")); err != nil {
		t.Fatalf("stage: %v", err)
	}
	err := sp.Drain()
	if err == nil || !strings.Contains(err.Error(), "every landing") {
		t.Fatalf("Drain = %v; want an every-landing-failed abort", err)
	}
	holding.mu.Lock()
	reclaimed := holding.reclaimed
	holding.mu.Unlock()
	if reclaimed != 0 {
		t.Fatalf("holding reclaimed %d times; the staged copy must stay for flush", reclaimed)
	}
}

// TestFanoutSingleLaneUnchanged: the classic one-landing route through the same path
// still drains and reclaims exactly as before (regression guard for the refactor).
func TestFanoutSingleLaneUnchanged(t *testing.T) {
	a, holding := newMemStore("a"), newMemStore("holding")
	sp := New(context.Background(), Config{
		Backings: []Backing{{Name: "a", Allocs: []archiveio.PartAllocator{a}, Rec: a, Writers: 1}},
		Holding:  NewPool([]Disk{{Name: "hd0", Alloc: holding, Storage: holding, Capacity: 0}}),
	})
	aw, err := sp.Ingest("a").NewArchive(archSpec, 1<<20)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := transferArchive(aw, []byte("solo")); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if a.recordCount() != 1 {
		t.Fatalf("landing recorded %d; want 1", a.recordCount())
	}
}
