package spool

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// The Pool back-pressures a producer: once a disk is full, the next acquire for it blocks
// until the drain releases space — "holding disk full ⇒ producer waits". With one disk this is a
// plain byte gate.
func TestPoolBlocksUntilReleased(t *testing.T) {
	p := NewPool([]Disk{{Capacity: 100}})
	if d, direct, err := p.Acquire(10); err != nil || direct || d != &p.disks[0] {
		t.Fatalf("acquire under capacity = (%p,%v,%v); want (disk 0,false,nil)", d, direct, err)
	}
	p.disks[0].used = 130 // force over capacity

	acquired := make(chan *Disk, 1)
	go func() {
		d, _, _ := p.Acquire(10) // must block: the only disk is full
		acquired <- d
	}()
	select {
	case <-acquired:
		t.Fatal("acquire must block while the only disk is full")
	case <-time.After(50 * time.Millisecond):
	}

	p.Release(&p.disks[0], 130) // used drops to 0: the waiter proceeds
	select {
	case d := <-acquired:
		if d != &p.disks[0] {
			t.Fatalf("woke onto disk %p, want disk 0", d)
		}
	case <-time.After(time.Second):
		t.Fatal("release must wake the blocked acquire")
	}
}

// A holding disk's `writers` cap gates staging concurrency: with writers: 1 a second acquire
// blocks — even with plenty of capacity — until the first write closes (ReleaseWriter). With two
// disks, the acquire spills to the disk with a free slot instead of blocking.
func TestPoolWritersCapGatesStaging(t *testing.T) {
	p := NewPool([]Disk{{Capacity: 100, Writers: 1}})
	if d, direct, err := p.Acquire(10); err != nil || direct || d != &p.disks[0] {
		t.Fatalf("first acquire = (%p,%v,%v); want (disk 0,false,nil)", d, direct, err)
	}

	acquired := make(chan *Disk, 1)
	go func() {
		d, _, _ := p.Acquire(10) // must block: the only disk is at its writer cap
		acquired <- d
	}()
	select {
	case <-acquired:
		t.Fatal("acquire must block while the only disk is at its writers cap")
	case <-time.After(50 * time.Millisecond):
	}

	p.ReleaseWriter(&p.disks[0]) // the first write closed: the waiter proceeds
	select {
	case d := <-acquired:
		if d != &p.disks[0] {
			t.Fatalf("woke onto disk %p, want disk 0", d)
		}
	case <-time.After(time.Second):
		t.Fatal("ReleaseWriter must wake the blocked acquire")
	}

	// Two disks: a disk at its cap is skipped, not waited on.
	p2 := NewPool([]Disk{{Capacity: 100, Writers: 1}, {Capacity: 100, Writers: 1}})
	first, _, _ := p2.Acquire(10)
	second, direct, err := p2.Acquire(10)
	if err != nil || direct || second == first {
		t.Fatalf("second acquire = (%p,%v,%v); want the other disk than %p", second, direct, err, first)
	}
}

// A landing failure aborts the pool, so every blocked producer wakes and fails fast instead of
// waiting for space that will never free.
func TestPoolAbortWakesBlocked(t *testing.T) {
	p := NewPool([]Disk{{Capacity: 100}})
	p.disks[0].used = 100 // full

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = p.Acquire(50) // all block (disk full)
		}(i)
	}
	time.Sleep(20 * time.Millisecond)

	boom := errors.New("landing down")
	p.Abort(boom)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, boom) {
			t.Errorf("blocked producer %d returned %v, want the abort error", i, err)
		}
	}
	if p.Err() == nil {
		t.Error("Err() must report the abort after Abort()")
	}
}

// Charge holds a committed archive's landed bytes against the disk until its drain copies it off, so
// the *drain backlog* itself back-pressures the next producer — not just the dumps still writing. Here
// the in-flight estimate is freed (Release) but the charged landed bytes keep the disk full, so the
// next acquire still blocks until the drain releases them.
func TestPoolChargeBackpressuresOnDrainBacklog(t *testing.T) {
	p := NewPool([]Disk{{Capacity: 100}})
	d, direct, err := p.Acquire(80) // reserves 80 of 100 for the in-flight write
	if err != nil || direct || d != &p.disks[0] {
		t.Fatalf("acquire = (%p,%v,%v); want (disk 0,false,nil)", d, direct, err)
	}
	p.Charge(d, 80)  // the archive committed: 80 landed bytes now occupy the disk (used = 160)
	p.Release(d, 80) // the producer frees its in-flight estimate on Close (used = 80, still the backlog)

	acquired := make(chan struct{}, 1)
	go func() {
		p.Acquire(30) // 80 + 30 > 100: must block on the drain backlog
		acquired <- struct{}{}
	}()
	select {
	case <-acquired:
		t.Fatal("acquire must block while a charged (not-yet-drained) archive fills the disk")
	case <-time.After(50 * time.Millisecond):
	}

	p.Release(d, 80) // the drain copied the archive off and freed its landed bytes
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("releasing the drained bytes must wake the blocked acquire")
	}
}

// Allocation is round-robin across the disks, so successive dumps spread across spindles.
func TestPoolRoundRobin(t *testing.T) {
	p := NewPool([]Disk{{Capacity: 1000}, {Capacity: 1000}, {Capacity: 1000}})
	var got []*Disk
	for i := 0; i < 6; i++ {
		d, direct, err := p.Acquire(10)
		if err != nil || direct {
			t.Fatalf("acquire %d = (%p,%v,%v)", i, d, direct, err)
		}
		got = append(got, d)
	}
	want := []*Disk{&p.disks[0], &p.disks[1], &p.disks[2], &p.disks[0], &p.disks[1], &p.disks[2]}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round-robin order = %v, want %v", got, want)
		}
	}
}

// A full disk is skipped: acquire keeps landing on the disk that still has room.
func TestPoolSkipsFull(t *testing.T) {
	p := NewPool([]Disk{{Capacity: 100}, {Capacity: 100}})
	p.disks[0].used = 100 // disk 0 full
	for i := 0; i < 3; i++ {
		d, direct, err := p.Acquire(10)
		if err != nil || direct {
			t.Fatalf("acquire %d = (%p,%v,%v)", i, d, direct, err)
		}
		if d != &p.disks[1] {
			t.Errorf("acquire %d landed on disk %p, want disk 1 (0 is full)", i, d)
		}
	}
}

// Acquire routes a DLE direct (bypassing the disks) only when no disk can ever fit it — its
// estimate meets/exceeds the largest disk's capacity and there is no unbounded disk. An unbounded
// disk fits anything; an unknown estimate (0) buffers.
func TestPoolRoutesDirect(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []int64
		est  int64
		want bool
	}{
		{"over the only disk routes direct", []int64{500}, 600, true},
		{"at capacity routes direct", []int64{500}, 500, true},
		{"under capacity buffers", []int64{500}, 400, false},
		{"fits the larger of two disks", []int64{500, 1000}, 600, false},
		{"too big for both disks", []int64{500, 800}, 900, true},
		{"unbounded disk never direct", []int64{0}, 10 << 30, false},
		{"unbounded among bounded never direct", []int64{500, 0}, 10 << 30, false},
		{"unknown estimate buffers", []int64{500}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disks := make([]Disk, len(tc.caps))
			for i, c := range tc.caps {
				disks[i] = Disk{Capacity: c}
			}
			_, direct, err := NewPool(disks).Acquire(tc.est)
			if err != nil {
				t.Fatal(err)
			}
			if direct != tc.want {
				t.Errorf("acquire(est=%d, caps=%v) direct=%v, want %v", tc.est, tc.caps, direct, tc.want)
			}
		})
	}
}
