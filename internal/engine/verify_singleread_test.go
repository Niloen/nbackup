package engine

import (
	"io"
	"os"
	"sync/atomic"
	"testing"

	"github.com/Niloen/nbackup/internal/archiveio"
)

// TestDeepVerifySingleRead proves a deep verify (checksum + structural) reads the
// archive off the medium exactly once — the fold that halves a deep drill's egress.
func TestDeepVerifySingleRead(t *testing.T) {
	f := newDrillFixture(t, "none")
	runID := "run-2026-06-21.000000"
	run, err := f.eng.cat.ReadRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := run.Archive(f.dle, 0)
	if !ok {
		t.Fatal("no L0 archive")
	}
	payload := payloadFile(t, f.diskDir, runID, 0)

	var opens int64
	open := func() (io.ReadCloser, error) {
		atomic.AddInt64(&opens, 1)
		return os.Open(payload)
	}
	ref := archiveio.Ref{Run: runID, DLE: f.dle, Level: 0}
	vd := f.eng.ver.verifyArchive(a, ref, "disk", VerifyOptions{Checks: CheckChecksum | CheckStructural}, open, nil)
	if !vd.OK {
		t.Fatalf("deep verify not OK: %s (%s)", vd.Detail, vd.Class)
	}
	if n := atomic.LoadInt64(&opens); n != 1 {
		t.Fatalf("deep verify opened the archive %d times, want 1 (single-pass fold)", n)
	}
}
