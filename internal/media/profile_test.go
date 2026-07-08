package media

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// keepSet is a fake Retention floor keyed by "run|dle" (it condemns nothing).
type keepSet map[string]bool

func (k keepSet) KeepsArchive(run, dle string) bool    { return k[run+"|"+dle] }
func (k keepSet) CondemnsArchive(run, dle string) bool { return false }

// verdicts is a fake Retention rendering both verdicts, keyed by "run|dle".
type verdicts struct{ keep, condemn keepSet }

func (v verdicts) KeepsArchive(run, dle string) bool    { return v.keep[run+"|"+dle] }
func (v verdicts) CondemnsArchive(run, dle string) bool { return v.condemn[run+"|"+dle] }

// TestSizeProfileReclaimsPerArchive: reclamation walks archives (run+DLE), not whole
// runs — oldest-first, skipping protected archives, stopping once under capacity. So
// an old run loses its reclaimable DLE while a protected run-mate stays.
func TestSizeProfileReclaimsPerArchive(t *testing.T) {
	archives := []record.Archive{
		{Run: "run-2026-01-01.001", DLE: "app", Level: 0, Compressed: 100},
		{Run: "run-2026-01-01.001", DLE: "db", Level: 0, Compressed: 100},
		{Run: "run-2026-02-01.001", DLE: "app", Level: 1, Compressed: 100},
		{Run: "run-2026-02-01.001", DLE: "db", Level: 1, Compressed: 100},
	}
	// db is protected in both runs (its live chain); app's archives are reclaimable.
	keep := keepSet{"run-2026-01-01.001|db": true, "run-2026-02-01.001|db": true}
	p := sizeProfile{capacity: 250} // total 400 → free 150 (two archives)

	got := p.Reclaim(p.TotalBytes(), archives, keep, time.Time{})
	if len(got) != 2 {
		t.Fatalf("reclaimed %d archives, want 2: %+v", len(got), got)
	}
	// db skipped as protected; app's L1 goes before the L0 it builds on (the
	// chain-safe order — dependents first), derived structurally here since no
	// BaseRun was recorded.
	want := []Reclamation{
		{RunID: "run-2026-02-01.001", DLE: "app", Bytes: 100, Note: "over capacity"},
		{RunID: "run-2026-01-01.001", DLE: "app", Bytes: 100, Note: "over capacity"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("reclaim[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestSizeProfileReclaimChainSafeOrder is the regression for the stranded-L1 defect:
// a superseded chain must lose its incrementals before the full they build on, so a
// reclaim that stops at the capacity target (here: after one archive) never deletes
// an L0 while a dependent L1 survives as unrestorable dead weight. What remains is
// always a restorable (if shorter) chain.
func TestSizeProfileReclaimChainSafeOrder(t *testing.T) {
	old := func(dle string, level int, run, base string) record.Archive {
		return record.Archive{Run: run, DLE: dle, Level: level, BaseRun: base, Compressed: 100}
	}
	// A superseded chain (L0 + two L1s, all reclaimable) beside the live one (pinned).
	archives := []record.Archive{
		old("app", 0, "run-2026-01-01.001", ""),
		old("app", 1, "run-2026-01-02.001", "run-2026-01-01.001"),
		old("app", 1, "run-2026-01-03.001", "run-2026-01-01.001"),
		old("app", 0, "run-2026-02-01.001", ""),
		old("app", 1, "run-2026-02-02.001", "run-2026-02-01.001"),
	}
	keep := keepSet{"run-2026-02-01.001|app": true, "run-2026-02-02.001|app": true}
	p := sizeProfile{capacity: 500}

	t.Run("stopping at the target leaves a restorable chain", func(t *testing.T) {
		got := p.Reclaim(400, archives, keep, time.Time{}) // free one archive
		if len(got) != 1 || got[0].RunID != "run-2026-01-02.001" {
			t.Fatalf("reclaim = %+v, want exactly the oldest dependent L1 (never the L0 it builds on)", got)
		}
	})

	t.Run("a full dies last, after every dependent", func(t *testing.T) {
		got := p.Reclaim(200, archives, keep, time.Time{}) // free the whole old chain
		want := []string{"run-2026-01-02.001", "run-2026-01-03.001", "run-2026-01-01.001"}
		if len(got) != len(want) {
			t.Fatalf("reclaimed %d archives, want %d: %+v", len(got), len(want), got)
		}
		for i, w := range want {
			if got[i].RunID != w {
				t.Errorf("reclaim[%d] = %s, want %s", i, got[i].RunID, w)
			}
		}
	})

	t.Run("a pinned dependent holds its unpinned base", func(t *testing.T) {
		chain := []record.Archive{
			old("app", 0, "run-2026-01-01.001", ""),
			old("app", 1, "run-2026-01-02.001", "run-2026-01-01.001"),
		}
		pinned := keepSet{"run-2026-01-02.001|app": true}
		if got := p.Reclaim(0, chain, pinned, time.Time{}); len(got) != 0 {
			t.Fatalf("reclaim = %+v, want none — deleting the L0 would strand the pinned L1", got)
		}
	})
}

// TestVolumeProfile: the pool is volumes * usable reel bytes (each reel's payload
// net of framing overhead); zero volumes means an unbounded pool (a hand-loaded
// drive) with the reel still the per-run ceiling; an unsized reel is unbounded.
func TestVolumeProfile(t *testing.T) {
	cases := []struct {
		name                string
		volumes, volumeSize int64
		wantTotal, wantReel int64
	}{
		// TotalBytes nets the per-reel framing overhead (label + one part header) from
		// each reel's payload; VolumeSize reports the raw reel ceiling. reel = 1 MiB
		// (1048576) → usable 1048576-65536 = 983040 per reel.
		{"library multiplies out", 3, 1048576, 3 * 983040, 1048576},
		{"unbounded pool, finite reel", 0, 1048576, 0, 1048576},
		{"unsized reel is unbounded", 3, 0, 0, 0},
		{"reel smaller than its framing holds nothing usable", 3, 100, 0, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewVolumeProfile(tc.volumes, tc.volumeSize)
			if got := p.TotalBytes(); got != tc.wantTotal {
				t.Errorf("TotalBytes = %d, want %d", got, tc.wantTotal)
			}
			if got := p.VolumeSize(); got != tc.wantReel {
				t.Errorf("VolumeSize = %d, want %d", got, tc.wantReel)
			}
		})
	}
}

// TestSizeProfileReclaimsCondemnedUnconditionally: a condemned archive (no restore
// can use it) is deleted first whatever the capacity situation — under target, or
// even on an unbounded store — and still in chain-safe order (a condemned L2 before
// the condemned L1 it builds on).
func TestSizeProfileReclaimsCondemnedUnconditionally(t *testing.T) {
	archives := []record.Archive{
		// L0 gone: both incrementals are condemned; L2 builds on the L1.
		{Run: "run-2026-01-02.000001", DLE: "app", Level: 1, BaseRun: "run-2026-01-01.000001", Compressed: 100},
		{Run: "run-2026-01-03.000001", DLE: "app", Level: 2, BaseRun: "run-2026-01-02.000001", Compressed: 100},
		{Run: "run-2026-02-01.000001", DLE: "app", Level: 0, Compressed: 100},
	}
	keep := verdicts{
		keep:    keepSet{"run-2026-02-01.000001|app": true},
		condemn: keepSet{"run-2026-01-02.000001|app": true, "run-2026-01-03.000001|app": true},
	}

	t.Run("unbounded store still deletes them", func(t *testing.T) {
		got := sizeProfile{}.Reclaim(0, archives, keep, time.Time{})
		want := []string{"run-2026-01-03.000001", "run-2026-01-02.000001"} // L2 before its L1
		if len(got) != len(want) {
			t.Fatalf("reclaim = %+v, want the two condemned archives", got)
		}
		for i, w := range want {
			if got[i].RunID != w || got[i].Note != "stranded — unrestorable" {
				t.Errorf("reclaim[%d] = %+v, want %s (stranded — unrestorable)", i, got[i], w)
			}
		}
	})

	t.Run("under capacity deletes them and nothing else", func(t *testing.T) {
		p := sizeProfile{capacity: 1000}
		if got := p.Reclaim(p.TotalBytes(), archives, keep, time.Time{}); len(got) != 2 {
			t.Fatalf("reclaim = %+v, want only the condemned pair", got)
		}
	})
}
