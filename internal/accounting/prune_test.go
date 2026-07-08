package accounting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
	_ "github.com/Niloen/nbackup/internal/media/tape" // register the tape spec: its whole-volume profile is what makes Reclaim a no-op there
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
func (f *fakeVol) ReadFile(int, media.Range) (record.Header, io.ReadCloser, error) {
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
			Run: "run-2026-06-30.001", Kind: record.KindArchive, DLE: "app", CreatedAt: createdAt,
		}}}
	}
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	acct := func(v media.Volume) *Accountant {
		return New(Deps{
			// An empty catalog: knownPositions finds nothing to exclude, so the sweep
			// scans the whole fake volume (every fixed file is a "surprise"), exercising
			// the orphan classifier exactly as before the catalog-diff optimization.
			Cat:        cat,
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

// The crash-safe removal order (commit footer first, then index, then parts) is the
// fs's invariant now that prune deletes through archivefs.Session.ReclaimAt — it is
// pinned by archivefs's TestReclaimStagedFooterFirst.

// fakeReclaimer records the archives Prune deletes and mirrors the real
// Session.ReclaimAt's catalog side (drop the archive from its placement), so the
// post-prune catalog reflects the deletion the way an applied prune would.
type fakeReclaimer struct {
	cat  *catalog.Catalog
	refs []archiveio.Ref
}

func (f *fakeReclaimer) ReclaimAt(ref archiveio.Ref, pos archiveio.ArchivePos) error {
	f.refs = append(f.refs, ref)
	_, _, err := f.cat.RemoveArchive(ref.Run, "disk", ref.DLE)
	return err
}

// TestPruneReclaimsStrandedArchives: an incremental whose base chain is broken
// catalog-wide (the historical L0-pruned-but-not-L1 defect) is worthless, so prune
// reclaims it even with no capacity pressure at all, and warns — while an
// incremental whose base survives is untouched.
func TestPruneReclaimsStrandedArchives(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pos := archiveio.ArchivePos{Parts: []archiveio.FilePos{{Pos: 1}}, Commit: archiveio.FilePos{Pos: 2}}
	add := func(run, dle string, level int, base string) {
		t.Helper()
		a := record.Archive{Run: run, DLE: dle, Level: level, BaseRun: base, Compressed: 100, CreatedAt: old}
		if err := cat.AddArchive(a, "disk", pos); err != nil {
			t.Fatal(err)
		}
	}
	// stranded's chain is broken (its base run was never recorded — the historical
	// bug); intact has its full and a live chain.
	add("run-2026-06-02.000001", "stranded", 1, "run-2026-06-01.000001")
	add("run-2026-06-10.000001", "intact", 0, "")
	add("run-2026-06-11.000001", "intact", 1, "run-2026-06-10.000001")

	rec := &fakeReclaimer{cat: cat}
	cfg := &config.Config{Media: map[string]config.Media{"disk": {Type: "disk"}}}
	acct := New(Deps{
		Cat:           cat,
		Cfg:           cfg,
		OpenVolume:    func(string) (media.Volume, error) { return &fakeVol{}, nil },
		OpenReclaimer: func(string) (Reclaimer, error) { return rec, nil },
		DisplayDLE:    func(s string) string { return s },
	})

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	eligible, _, freed, err := acct.Prune("disk", now, true, logf)
	if err != nil {
		t.Fatal(err)
	}
	// Only the stranded archive dies — the disk is unbounded, so nothing else is
	// capacity-eligible.
	if eligible != 1 || freed != 100 {
		t.Fatalf("eligible=%d freed=%d, want 1/100 (the stranded archive alone): %v", eligible, freed, logged)
	}
	if len(rec.refs) != 1 || rec.refs[0].Run != "run-2026-06-02.000001" || rec.refs[0].DLE != "stranded" {
		t.Fatalf("reclaimed %+v, want the stranded archive", rec.refs)
	}
	warned := false
	for _, l := range logged {
		if strings.Contains(l, "WARN") && strings.Contains(l, "unrestorable") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("expected a WARN unrestorable line, got: %v", logged)
	}

	// A second prune is a clean no-op: the stranded archive is gone from the catalog.
	eligible, _, _, err = acct.Prune("disk", now, true, logf)
	if err != nil || eligible != 0 {
		t.Fatalf("second prune: eligible=%d err=%v, want 0/nil", eligible, err)
	}
}

// TestPruneKeepsYoungStrandedArchive: a stranded archive still within minimum_age is
// left alone (the WORM/Object-Lock guard, exactly like the crash-orphan sweep) — its
// keep line, rendered by the floor's age pin, says why it lingers.
func TestPruneKeepsYoungStrandedArchive(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pos := archiveio.ArchivePos{Parts: []archiveio.FilePos{{Pos: 1}}, Commit: archiveio.FilePos{Pos: 2}}
	young := record.Archive{Run: "run-2026-07-08.000001", DLE: "app", Level: 1, BaseRun: "run-2026-07-01.000001", Compressed: 100, CreatedAt: now.Add(-time.Hour)}
	if err := cat.AddArchive(young, "disk", pos); err != nil {
		t.Fatal(err)
	}

	rec := &fakeReclaimer{cat: cat}
	cfg := &config.Config{Media: map[string]config.Media{"disk": {Type: "disk", MinimumAge: "1d"}}}
	acct := New(Deps{
		Cat:           cat,
		Cfg:           cfg,
		OpenVolume:    func(string) (media.Volume, error) { return &fakeVol{}, nil },
		OpenReclaimer: func(string) (Reclaimer, error) { return rec, nil },
		DisplayDLE:    func(s string) string { return s },
	})

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	eligible, _, _, err := acct.Prune("disk", now, true, logf)
	if err != nil {
		t.Fatal(err)
	}
	if eligible != 0 || len(rec.refs) != 0 {
		t.Fatalf("eligible=%d reclaimed=%+v, want nothing deleted (within minimum_age): %v", eligible, rec.refs, logged)
	}
	explained := false
	for _, l := range logged {
		if strings.Contains(l, "unrestorable") && strings.Contains(l, "minimum age") {
			explained = true
		}
	}
	if !explained {
		t.Fatalf("expected a keep line saying unrestorable-within-minimum-age, got: %v", logged)
	}
}

// TestPruneLeavesTapeStrandedAlone: a labeled copy (Placement.Labeled — the
// positions record volume labels) is out of per-archive reach. A superseded chain
// necessarily loses its base tape first (rotation recycles the oldest volume
// whole), so its stranded incrementals are by-design rotation debris — a prune must
// not attempt per-archive deletion there (impossible mid-reel); they die with their
// own volume at relabel.
func TestPruneLeavesTapeStrandedAlone(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pos := archiveio.ArchivePos{Parts: []archiveio.FilePos{{Label: "vault-1", Pos: 1}}, Commit: archiveio.FilePos{Label: "vault-1", Pos: 2}}
	strandedL1 := record.Archive{Run: "run-2026-06-02.000001", DLE: "app", Level: 1, BaseRun: "run-2026-06-01.000001", Compressed: 100, CreatedAt: now.Add(-30 * 24 * time.Hour)}
	if err := cat.AddArchive(strandedL1, "vault", pos); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Media: map[string]config.Media{"vault": {Type: "tape", Params: map[string]string{"volume_size": "1MB"}}}}
	acct := New(Deps{
		Cat: cat,
		Cfg: cfg,
		OpenReclaimer: func(string) (Reclaimer, error) {
			t.Fatal("prune must not open a per-archive reclaimer on tape")
			return nil, nil
		},
		DisplayDLE: func(s string) string { return s },
	})

	eligible, swept, freed, err := acct.Prune("vault", now, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if eligible != 0 || swept != 0 || freed != 0 {
		t.Fatalf("Prune(vault) = %d/%d/%d, want all zero (rotation debris is relabel's business)", eligible, swept, freed)
	}
}
