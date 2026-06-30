package accounting

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// fakeVol is a minimal media.Volume for exercising sweepOrphans: it serves a fixed file
// list (so OrphanFiles sees footer-less parts — no commit footer means none are
// referenced) and records or refuses RemoveFile. It does not implement
// IncompleteEnumerator, so the torn-file path is skipped here.
type fakeVol struct {
	files      []record.FileInfo
	removed    []int
	failRemove bool
}

func (f *fakeVol) AppendFile(context.Context, record.Header) (media.FileWriter, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeVol) ReadFile(int) (record.Header, io.ReadCloser, error) {
	return record.Header{}, nil, errors.New("not implemented")
}
func (f *fakeVol) Files() ([]record.FileInfo, error) { return f.files, nil }
func (f *fakeVol) RemoveFile(pos int) error {
	if f.failRemove {
		return errors.New("object lock: deletion refused")
	}
	f.removed = append(f.removed, pos)
	return nil
}

// TestSweepOrphansMinimumAgeAndWORM covers the two robustness guards: an orphan younger
// than minimum_age is left alone (so the sweep never fights an immutable medium whose
// Object-Lock retention the operator mirrored into minimum_age), and a refused delete is
// tolerated (best-effort) rather than failing the run.
func TestSweepOrphansMinimumAgeAndWORM(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	orphan := func(createdAt time.Time) []record.FileInfo {
		return []record.FileInfo{{Pos: 5, Header: record.Header{
			Slot: "slot-2026-06-30.001", Kind: record.KindArchive, DLE: "app", CreatedAt: createdAt,
		}}}
	}
	acct := func(v media.Volume) *Accountant {
		return New(Deps{
			OpenVolume: func(string) (media.Volume, error) { return v, nil },
			DisplayDLE: func(s string) string { return s },
		})
	}

	t.Run("within minimum_age is kept", func(t *testing.T) {
		v := &fakeVol{files: orphan(now.Add(-30 * time.Minute))}
		swept, err := acct(v).sweepOrphans("disk", time.Hour, now, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if swept != 0 || len(v.removed) != 0 {
			t.Fatalf("swept=%d removed=%v, want 0 / none (orphan younger than minimum_age)", swept, v.removed)
		}
	})

	t.Run("past minimum_age is swept", func(t *testing.T) {
		v := &fakeVol{files: orphan(now.Add(-2 * time.Hour))}
		swept, err := acct(v).sweepOrphans("disk", time.Hour, now, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if swept != 1 || len(v.removed) != 1 || v.removed[0] != 5 {
			t.Fatalf("swept=%d removed=%v, want 1 / [5]", swept, v.removed)
		}
	})

	t.Run("refused delete (WORM) is tolerated", func(t *testing.T) {
		v := &fakeVol{files: orphan(now.Add(-2 * time.Hour)), failRemove: true}
		swept, err := acct(v).sweepOrphans("disk", time.Hour, now, true, nil)
		if err != nil {
			t.Fatalf("a refused delete must not fail prune, got %v", err)
		}
		if swept != 0 {
			t.Fatalf("swept=%d, want 0 (delete refused, left for a later prune)", swept)
		}
	})
}

// TestArchivePositionsCommitFirst pins the crash-safe removal order: the commit
// footer (the archive's marker) is reclaimed FIRST, then the index, then the parts —
// so a crash mid-prune leaves footer-less orphans a rebuild ignores, never a footer
// whose parts are already gone (which would rebuild into a phantom committed archive).
func TestArchivePositionsCommitFirst(t *testing.T) {
	ps := []catalog.Placement{{
		Medium: "disk",
		Archives: []catalog.ArchivePos{{
			DLE:    "app",
			Level:  0,
			Parts:  []record.FilePos{{Pos: 0}, {Pos: 1}},
			Index:  record.FilePos{Pos: 2},
			Commit: record.FilePos{Pos: 3},
		}},
	}}
	got := archivePositions(ps, "disk", "app")
	want := []int{3, 2, 0, 1} // commit, index, parts
	if len(got) != len(want) {
		t.Fatalf("positions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position[%d] = %d, want %d (full order %v, want %v)", i, got[i], want[i], got, want)
		}
	}
	// An archive with no member index omits that slot, still commit-first.
	ps[0].Archives[0].Index = record.FilePos{}
	if got := archivePositions(ps, "disk", "app"); len(got) != 3 || got[0] != 3 {
		t.Fatalf("without index, order = %v, want [3 0 1] (commit first)", got)
	}
}
