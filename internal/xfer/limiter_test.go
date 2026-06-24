package xfer

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// A nil limiter (the uncapped default) must hand back the wrapped stream untouched
// so behavior is identical to having no limiter at all.
func TestLimiterUncapped(t *testing.T) {
	if l := NewLimiter(0); l != nil {
		t.Fatalf("NewLimiter(0) = %v, want nil (uncapped)", l)
	}
	if l := NewLimiter(-1); l != nil {
		t.Fatalf("NewLimiter(-1) = %v, want nil (uncapped)", l)
	}
	var l *Limiter // nil
	var buf bytes.Buffer
	if got := l.Writer(&buf); got != io.Writer(&buf) {
		t.Errorf("nil Limiter.Writer wrapped the destination; want pass-through")
	}
	r := bytes.NewReader(nil)
	if got := l.Reader(r); got != io.Reader(r) {
		t.Errorf("nil Limiter.Reader wrapped the source; want pass-through")
	}
}

// A capped write must take at least the time the rate implies. The bucket starts
// full (up to maxBurst), so the conservative floor is (data-maxBurst)/rate.
func TestLimiterWriteThrottles(t *testing.T) {
	const rate = 1 << 20 // 1 MiB/s
	data := make([]byte, 2<<20)
	var buf bytes.Buffer
	w := NewLimiter(rate).Writer(&buf)

	start := time.Now()
	n, err := w.Write(data)
	elapsed := time.Since(start)

	if err != nil || n != len(data) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(data))
	}
	if buf.Len() != len(data) {
		t.Fatalf("destination got %d bytes, want %d", buf.Len(), len(data))
	}
	if floor := time.Duration(float64(len(data)-maxBurst) / rate * float64(time.Second) * 0.9); elapsed < floor {
		t.Errorf("write took %v; expected throttling to at least %v", elapsed, floor)
	}
}

// A capped read must likewise be paced to the rate.
func TestLimiterReadThrottles(t *testing.T) {
	const rate = 1 << 20 // 1 MiB/s
	data := make([]byte, 2<<20)
	r := NewLimiter(rate).Reader(bytes.NewReader(data))

	start := time.Now()
	n, err := io.Copy(io.Discard, r)
	elapsed := time.Since(start)

	if err != nil || n != int64(len(data)) {
		t.Fatalf("Copy = %d, %v; want %d, nil", n, err, len(data))
	}
	if floor := time.Duration(float64(len(data)-maxBurst) / rate * float64(time.Second) * 0.9); elapsed < floor {
		t.Errorf("read took %v; expected throttling to at least %v", elapsed, floor)
	}
}

// Concurrent writers sharing one limiter must share the budget: two 1 MiB writes
// through a single 1 MiB/s cap take ~the time of one 2 MiB write, not run in
// parallel at full speed. This is the scope-2 (netusage) guarantee.
func TestLimiterSharedBudget(t *testing.T) {
	const rate = 1 << 20 // 1 MiB/s shared
	l := NewLimiter(rate)
	chunk := make([]byte, 1<<20)

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Writer(io.Discard).Write(chunk); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := 2 * len(chunk)
	if floor := time.Duration(float64(total-maxBurst) / rate * float64(time.Second) * 0.9); elapsed < floor {
		t.Errorf("two shared writers took %v; a shared %d B/s budget should need at least %v", elapsed, rate, floor)
	}
}
