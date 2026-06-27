package engine

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// The byteGate back-pressures a committing dumper: charging past capacity makes it wait until
// the taper releases space — "holding disk full ⇒ dumper waits".
func TestByteGateBlocksUntilReleased(t *testing.T) {
	g := newByteGate(100)
	g.charge(80) // under capacity

	waited := make(chan struct{})
	go func() {
		g.charge(50)              // 80+50 > 100
		_ = g.waitUnderCapacity() // must block until the release below drops it under cap
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("a charge past capacity must block while the disk is full")
	case <-time.After(50 * time.Millisecond):
	}

	g.release(80) // used drops to 50 (<= 100): the dumper proceeds
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("release must wake the blocked dumper")
	}
}

// A landing failure aborts the gate, so every blocked dumper wakes and fails fast instead of
// waiting for space that will never free.
func TestByteGateAbortWakesBlocked(t *testing.T) {
	g := newByteGate(100)
	g.charge(100) // full

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g.charge(50)
			errs[i] = g.waitUnderCapacity() // all block (over capacity)
		}(i)
	}
	time.Sleep(20 * time.Millisecond)

	boom := errors.New("tape down")
	g.abort(boom)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, boom) {
			t.Errorf("blocked dumper %d returned %v, want the abort error", i, err)
		}
	}
	if g.err() == nil {
		t.Error("err() must report the abort after abort()")
	}
}
