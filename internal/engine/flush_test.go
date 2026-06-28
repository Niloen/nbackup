package engine

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// The holdingPool back-pressures a dumper: once a disk is full, the next acquire for it blocks
// until the drain releases space — "holding disk full ⇒ dumper waits". With one disk this is the
// old byteGate behavior.
func TestHoldingPoolBlocksUntilReleased(t *testing.T) {
	p := newHoldingPool([]holdingDisk{{capacity: 100}})
	if idx, direct, err := p.acquire(10); err != nil || direct || idx != 0 {
		t.Fatalf("acquire under capacity = (%d,%v,%v); want (0,false,nil)", idx, direct, err)
	}
	p.charge(0, 130) // now over capacity

	acquired := make(chan int, 1)
	go func() {
		i, _, _ := p.acquire(10) // must block: the only disk is full
		acquired <- i
	}()
	select {
	case <-acquired:
		t.Fatal("acquire must block while the only disk is full")
	case <-time.After(50 * time.Millisecond):
	}

	p.release(0, 130) // used drops to 0: the waiter proceeds
	select {
	case i := <-acquired:
		if i != 0 {
			t.Fatalf("woke onto disk %d, want 0", i)
		}
	case <-time.After(time.Second):
		t.Fatal("release must wake the blocked acquire")
	}
}

// A landing failure aborts the pool, so every blocked dumper wakes and fails fast instead of
// waiting for space that will never free.
func TestHoldingPoolAbortWakesBlocked(t *testing.T) {
	p := newHoldingPool([]holdingDisk{{capacity: 100}})
	p.charge(0, 100) // full

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = p.acquire(50) // all block (disk full)
		}(i)
	}
	time.Sleep(20 * time.Millisecond)

	boom := errors.New("landing down")
	p.abort(boom)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, boom) {
			t.Errorf("blocked dumper %d returned %v, want the abort error", i, err)
		}
	}
	if p.err() == nil {
		t.Error("err() must report the abort after abort()")
	}
}

// Allocation is round-robin across the disks, so successive dumps spread across spindles.
func TestHoldingPoolRoundRobin(t *testing.T) {
	p := newHoldingPool([]holdingDisk{{capacity: 1000}, {capacity: 1000}, {capacity: 1000}})
	var got []int
	for i := 0; i < 6; i++ {
		idx, direct, err := p.acquire(10)
		if err != nil || direct {
			t.Fatalf("acquire %d = (%d,%v,%v)", i, idx, direct, err)
		}
		p.charge(idx, 10)
		got = append(got, idx)
	}
	want := []int{0, 1, 2, 0, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round-robin order = %v, want %v", got, want)
		}
	}
}

// A full disk is skipped: acquire keeps landing on the disk that still has room.
func TestHoldingPoolSkipsFull(t *testing.T) {
	p := newHoldingPool([]holdingDisk{{capacity: 100}, {capacity: 100}})
	p.charge(0, 100) // disk 0 full
	for i := 0; i < 3; i++ {
		idx, direct, err := p.acquire(10)
		if err != nil || direct {
			t.Fatalf("acquire %d = (%d,%v,%v)", i, idx, direct, err)
		}
		if idx != 1 {
			t.Errorf("acquire %d landed on disk %d, want 1 (0 is full)", i, idx)
		}
	}
}

// acquire routes a DLE direct (bypassing the disks) only when no disk can ever fit it — its
// estimate meets/exceeds the largest disk's capacity and there is no unbounded disk. An unbounded
// disk fits anything; an unknown estimate (0) buffers.
func TestHoldingPoolRoutesDirect(t *testing.T) {
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
			disks := make([]holdingDisk, len(tc.caps))
			for i, c := range tc.caps {
				disks[i] = holdingDisk{capacity: c}
			}
			_, direct, err := newHoldingPool(disks).acquire(tc.est)
			if err != nil {
				t.Fatal(err)
			}
			if direct != tc.want {
				t.Errorf("acquire(est=%d, caps=%v) direct=%v, want %v", tc.est, tc.caps, direct, tc.want)
			}
		})
	}
}
