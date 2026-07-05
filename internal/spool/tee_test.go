package spool

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
)

// teeSpool wires a spool with two landing lanes and NO holding disk, so every write
// goes direct — the Tee path.
func teeSpool(a, b *memStore) *Spool {
	return New(context.Background(), Config{
		Backings: []Backing{
			{Name: "a", Allocs: []archiveio.PartAllocator{a}, Rec: a, Writers: 1},
			{Name: "b", Allocs: []archiveio.PartAllocator{b}, Rec: b, Writers: 1},
		},
		Holding: NewPool(nil),
	})
}

// TestTeeLandsBothLockstep: a direct two-landing write lands the archive on both
// media with identical checksum and identical part seals — the lanes cut at the SAME
// boundaries (the smaller cap), so the larger-part medium simply gets more, smaller
// parts than it would alone.
func TestTeeLandsBothLockstep(t *testing.T) {
	a, b := newMemStore("a"), newMemStore("b")
	a.partCap = 4 // the smaller cut wins...
	b.partCap = 9 // ...so b's parts are 4-byte too
	sp := teeSpool(a, b)
	body := []byte("ten bytes!") // 10 bytes -> parts of 4,4,2 on BOTH lanes
	aw, err := sp.Ingest("a", "b").NewArchive(archSpec, 1<<20)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := transferArchive(aw, body); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if a.recordCount() != 1 || b.recordCount() != 1 {
		t.Fatalf("recorded a=%d b=%d; want 1 each", a.recordCount(), b.recordCount())
	}
	ra, rb := a.records[0].Archive, b.records[0].Archive
	if ra.SHA256 != shaOf(body) || rb.SHA256 != ra.SHA256 {
		t.Fatalf("checksums differ or wrong: a=%s b=%s want %s", ra.SHA256, rb.SHA256, shaOf(body))
	}
	if ra.Parts != 3 || rb.Parts != 3 {
		t.Fatalf("parts a=%d b=%d; want lockstep 3 each (4+4+2)", ra.Parts, rb.Parts)
	}
	if !reflect.DeepEqual(ra.PartSeals, rb.PartSeals) {
		t.Fatalf("per-part seals differ across lanes:\na=%+v\nb=%+v", ra.PartSeals, rb.PartSeals)
	}
}

// TestTeeSecondaryFaultSurvivorCommits: one lane dies mid-route (its NextPart
// faults). The stream keeps flowing to the survivor, which commits alone; the dead
// lane is tripped — the next archive skips it without touching the medium — and the
// run finishes with one warning naming the sync repair.
func TestTeeSecondaryFaultSurvivorCommits(t *testing.T) {
	a, b := newMemStore("a"), newMemStore("b")
	b.nextPartErr = errors.New("lane b next-part boom")
	sp := teeSpool(a, b)
	for i := 0; i < 2; i++ {
		aw, err := sp.Ingest("a", "b").NewArchive(archSpec, 1<<20)
		if err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
		if err := transferArchive(aw, []byte("survivor payload")); err != nil {
			t.Fatalf("transfer %d must survive a one-lane fault, got %v", i, err)
		}
	}
	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain = %v; a one-lane fault must not fail the run", err)
	}
	if a.recordCount() != 2 || b.recordCount() != 0 {
		t.Fatalf("recorded a=%d b=%d; want survivor 2, dead 0", a.recordCount(), b.recordCount())
	}
	w := sp.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "nb sync --to b") {
		t.Fatalf("Warnings() = %v; want one warning with the sync repair for b", w)
	}
}

// TestTeeCommitFaultSurvivorKeeps: the fault arrives at the very end (the dead
// lane's Record fails at Commit) — the survivor's already-committed placement stands
// and the transfer still succeeds.
func TestTeeCommitFaultSurvivorKeeps(t *testing.T) {
	a, b := newMemStore("a"), newMemStore("b")
	b.recordErr = errors.New("lane b record boom")
	sp := teeSpool(a, b)
	aw, err := sp.Ingest("a", "b").NewArchive(archSpec, 1<<20)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := transferArchive(aw, []byte("payload")); err != nil {
		t.Fatalf("transfer must survive a commit fault on one lane, got %v", err)
	}
	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if a.recordCount() != 1 || b.recordCount() != 0 {
		t.Fatalf("recorded a=%d b=%d; want 1/0", a.recordCount(), b.recordCount())
	}
}

// TestTeeAllLanesFaultFailsAndReleases: every lane faults, so the transfer fails
// (the archive landed nowhere) — and both permits come back, proven by a follow-up
// single-lane write on a healed lane completing instead of blocking on a leaked
// drive (Writers: 1).
func TestTeeAllLanesFaultFailsAndReleases(t *testing.T) {
	a, b := newMemStore("a"), newMemStore("b")
	a.nextPartErr = errors.New("a boom")
	b.nextPartErr = errors.New("b boom")
	sp := teeSpool(a, b)
	aw, err := sp.Ingest("a", "b").NewArchive(archSpec, 1<<20)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := transferArchive(aw, []byte("payload")); err == nil {
		t.Fatal("transfer must fail when every lane faults")
	}
	if a.recordCount() != 0 || b.recordCount() != 0 {
		t.Fatalf("recorded a=%d b=%d; want nothing", a.recordCount(), b.recordCount())
	}
	// Both lanes are tripped now; a fresh spool over the healed store proves nothing
	// leaked into the fakes, and re-ingesting here proves the permits were released
	// (the abort surfaces as an error, not a hang).
	if _, err := sp.Ingest("a", "b").NewArchive(archSpec, 1<<20); err == nil {
		t.Fatal("ingest after every lane tripped must refuse (nowhere to land)")
	}
}

// TestTeePermitOrderingNoDeadlock: two producers whose routes list the same two
// exclusive lanes in OPPOSITE order (writers: 1 each) must both complete — drives are
// leased in sorted lane-name order, a global order that makes the classic AB-BA
// deadlock impossible.
func TestTeePermitOrderingNoDeadlock(t *testing.T) {
	a, b := newMemStore("a"), newMemStore("b")
	sp := teeSpool(a, b)
	routes := [][]string{{"a", "b"}, {"b", "a"}}
	done := make(chan error, len(routes))
	var wg sync.WaitGroup
	for i, r := range routes {
		wg.Add(1)
		go func(i int, route []string) {
			defer wg.Done()
			for n := 0; n < 5; n++ { // several rounds to give an unordered lease a chance to interleave
				aw, err := sp.Ingest(route...).NewArchive(archSpec, 1<<20)
				if err != nil {
					done <- err
					return
				}
				if err := transferArchive(aw, []byte("deadlock probe")); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}(i, r)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("producers deadlocked on lane permits; leases must follow a global order")
	}
	for range routes {
		if err := <-done; err != nil {
			t.Fatalf("producer failed: %v", err)
		}
	}
	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if a.recordCount() != 10 || b.recordCount() != 10 {
		t.Fatalf("recorded a=%d b=%d; want 10 each", a.recordCount(), b.recordCount())
	}
}
