package accounting

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// diskfake is a test-only medium type registered below: a size profile reading its
// "capacity" option, so this test can exercise MediumProtected's capacity arithmetic
// without depending on the real disk package (accounting must stay medium-neutral).
func init() {
	media.Register(media.Spec{Type: "diskfake", Profile: media.NewSizeProfile})
}

// TestMediumProtectedExcludesReclaimableBytes checks that MediumProtected reports
// the residual after a capacity-fitting prune, not raw used bytes: a DLE with a
// superseded old full (reclaimable, per retention.Compute) plus its current full
// (protected, the live recovery path) sees only the current full's bytes counted
// toward the residual, even though the medium's raw Used total is both.
func TestMediumProtectedExcludesReclaimableBytes(t *testing.T) {
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := record.Archive{Run: "run-2026-01-01.010000", DLE: "app", Level: 0, Compressed: 40, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cur := record.Archive{Run: "run-2026-02-01.010000", DLE: "app", Level: 0, Compressed: 60, CreatedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}
	mustAdd(t, cat, old, "disk", archiveio.ArchivePos{Commit: archiveio.FilePos{Pos: 1}})
	mustAdd(t, cat, cur, "disk", archiveio.ArchivePos{Commit: archiveio.FilePos{Pos: 2}})

	cfg := &config.Config{Media: map[string]config.Media{"disk": {Type: "diskfake", Capacity: "90"}}}
	acct := New(Deps{Cat: cat, Cfg: cfg})

	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	residual, capacity, err := acct.MediumProtected("disk", now)
	if err != nil {
		t.Fatal(err)
	}
	if capacity != 90 {
		t.Errorf("capacity = %d, want 90 (the configured figure)", capacity)
	}
	if residual != 60 {
		t.Errorf("residual = %d, want 60 (raw used 100 minus the 40-byte reclaimable old full)", residual)
	}

	// MediumProtectedOverCapacity shares this computation: unlike raw used (100 >
	// 90), the residual (60) is well under capacity, so it must not flag over.
	over, overResidual, overCapacity, err := acct.MediumProtectedOverCapacity("disk", now)
	if err != nil {
		t.Fatal(err)
	}
	if over {
		t.Errorf("MediumProtectedOverCapacity: over = true, want false (residual %d <= capacity %d)", overResidual, overCapacity)
	}
	if overResidual != residual || overCapacity != capacity {
		t.Errorf("MediumProtectedOverCapacity = (%d, %d), want the same figures MediumProtected computed (%d, %d)",
			overResidual, overCapacity, residual, capacity)
	}
}

// TestMediumProtectedUnknownMedium confirms an unconfigured medium name errors
// rather than silently reporting zero, the way ProfileFor already does.
func TestMediumProtectedUnknownMedium(t *testing.T) {
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	acct := New(Deps{Cat: cat, Cfg: &config.Config{}})
	if _, _, err := acct.MediumProtected("nope", time.Now()); err == nil {
		t.Error("MediumProtected(unknown medium) = nil error, want one")
	}
}
