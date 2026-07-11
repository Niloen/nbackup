package archiveio

import (
	"context"
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/xfer"
)

// Tee fans one archive stream into several ArchiveWriters — a multi-landing route's
// direct write, where no holding disk stages the archive for per-landing drains. The
// producer drives it exactly like a bare writer (it is an archivefs.ArchiveSink); each
// lane keeps its own positions, seals, and placement Record.
//
// Every lane cuts parts at the SAME boundaries: NextPart asks each lane for a part and
// returns the minimum of their caps, so the lanes stay in lockstep — no re-parting, no
// goroutines, and every copy carries identical per-part seals (which is exactly the
// aligned-seals condition ranged reads need). The larger-part medium simply gets more,
// smaller parts.
//
// Failure is any-lane-suffices: a lane error drops that lane from the fan mid-stream
// (its writer is closed uncommitted — the spool's close hook releases its permit — and
// onDrop reports it, so the spool trips the lane and warns). The stream keeps flowing
// to the survivors; only the LAST live lane failing fails the transfer, because then
// the archive would land nowhere.
type Tee struct {
	lanes   []*ArchiveWriter // primary first; a dropped lane goes nil
	onDrop  func(i int, err error)
	n       int64 // landed bytes, counted once at the fan-in
	tap     func(landed int64)
	landed  []int64 // per-lane committed bytes, index-aligned with lanes
	laneTap func(i int, landed int64)
}

// NewTee builds a tee over lanes (primary first). onDrop is called once per dropped
// lane with its index and the error that killed it; nil is allowed.
func NewTee(lanes []*ArchiveWriter, onDrop func(i int, err error)) *Tee {
	if onDrop == nil {
		onDrop = func(int, error) {}
	}
	return &Tee{
		lanes:  append([]*ArchiveWriter(nil), lanes...),
		onDrop: onDrop,
		landed: make([]int64, len(lanes)),
	}
}

// drop abandons lane i: its writer is closed uncommitted (releasing whatever it
// leased) and the drop reported. lastErr keeps the caller's error when every lane is
// gone.
func (t *Tee) drop(i int, err error) {
	lw := t.lanes[i]
	t.lanes[i] = nil
	_ = lw.Close()
	t.onDrop(i, err)
}

// live reports whether any lane is still receiving the stream.
func (t *Tee) live() bool {
	for _, lw := range t.lanes {
		if lw != nil {
			return true
		}
	}
	return false
}

// NextPart (xfer.Sink) opens the next part on every live lane and returns one writer
// fanning into all of them, capped at the smallest lane cap (<0 = unbounded). A lane
// whose NextPart fails is dropped; the last live lane failing fails the call.
func (t *Tee) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	fw := &fanPartWriter{t: t, parts: make([]io.WriteCloser, len(t.lanes)), wrote: make([]int64, len(t.lanes))}
	max := int64(-1)
	for i, lw := range t.lanes {
		if lw == nil {
			continue
		}
		w, m, err := lw.NextPart(ctx)
		if err != nil {
			t.drop(i, err)
			if !t.live() {
				return nil, 0, err
			}
			continue
		}
		fw.parts[i] = w
		if m >= 0 && (max < 0 || m < max) {
			max = m
		}
	}
	return fw, max, nil
}

// Commit (xfer.Sink) seals every live lane against the same producer totals, primary
// first — each lane writes its own footer/index and Records its own placement. A lane
// whose Commit fails is dropped; the last live lane failing fails the commit.
func (t *Tee) Commit(ctx context.Context, s xfer.SourceStats) error {
	for i, lw := range t.lanes {
		if lw == nil {
			continue
		}
		if err := lw.Commit(ctx, s); err != nil {
			t.drop(i, err)
			if !t.live() {
				return err
			}
		}
	}
	return nil
}

// Committed returns the first surviving lane's result (the primary's, when it is
// alive) — every lane carries the same bytes, checksum, and sizes; only positions
// differ, and the fan-out's consumers account the primary.
func (t *Tee) Committed() (CommitResult, bool) {
	for _, lw := range t.lanes {
		if lw == nil {
			continue
		}
		if res, ok := lw.Committed(); ok {
			return res, ok
		}
	}
	return CommitResult{}, false
}

// Meter attaches a progress tap, fired once per write at the fan-in — the stream is
// produced once, so its landed count is counted once, not per lane.
func (t *Tee) Meter(tap func(landed int64)) { t.tap = tap }

// MeterLane attaches a per-lane progress tap, fired with a lane's cumulative committed
// bytes each time one of its parts closes successfully — the honest per-landing landed
// count. Because the lanes are written one after another and a buffering medium only
// persists a part at close, this is what lets a direct fan-out's ETA see that the slower
// lane's second copy is still in flight; a bare tap at the fan-in cannot (it counts the
// stream once, as produced). nil is allowed.
func (t *Tee) MeterLane(tap func(i int, landed int64)) { t.laneTap = tap }

// landPart credits lane i with a just-closed part's bytes and reports its new cumulative
// committed total — called only for a part that closed without error.
func (t *Tee) landPart(i int, partBytes int64) {
	t.landed[i] += partBytes
	if t.laneTap != nil {
		t.laneTap(i, t.landed[i])
	}
}

// Close closes every remaining lane writer (dropped lanes closed at drop time),
// running each one's release hook; the first error wins.
func (t *Tee) Close() error {
	var first error
	for i, lw := range t.lanes {
		if lw == nil {
			continue
		}
		t.lanes[i] = nil
		if err := lw.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// fanPartWriter is one lockstep part across the live lanes: Write copies each chunk
// to every lane's part writer, dropping a lane that fails; Close closes each lane's
// part (recording its position there). Either fails only when no lane remains.
type fanPartWriter struct {
	t     *Tee
	parts []io.WriteCloser // index-aligned with t.lanes; nil = lane not in this part
	wrote []int64          // bytes written to each lane's part so far, credited on close
}

func (f *fanPartWriter) Write(p []byte) (int, error) {
	var lastErr error
	wrote := false
	for i, w := range f.parts {
		if w == nil {
			continue
		}
		n, err := w.Write(p)
		if err == nil && n < len(p) {
			err = io.ErrShortWrite
		}
		if err != nil {
			lastErr = err
			f.parts[i] = nil
			_ = w.Close() // best-effort: free the dead lane's file handle; its stray part is scan-ignored, like any faulted transfer's
			f.t.drop(i, err)
			continue
		}
		f.wrote[i] += int64(n)
		wrote = true
	}
	if !wrote {
		if lastErr == nil {
			lastErr = fmt.Errorf("archive fan-out: no landing left to write")
		}
		return 0, lastErr
	}
	if f.t.tap != nil {
		f.t.n += int64(len(p))
		f.t.tap(f.t.n)
	}
	return len(p), nil
}

func (f *fanPartWriter) Close() error {
	var lastErr error
	closed := false
	for i, w := range f.parts {
		if w == nil {
			continue
		}
		f.parts[i] = nil
		if err := w.Close(); err != nil {
			lastErr = err
			f.t.drop(i, err)
			continue
		}
		f.t.landPart(i, f.wrote[i]) // the part is now persisted on lane i — credit its bytes
		closed = true
	}
	if !closed {
		return lastErr
	}
	return nil
}
