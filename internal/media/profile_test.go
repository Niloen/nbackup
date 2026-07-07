package media

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// keepSet is a fake Retention floor keyed by "run|dle".
type keepSet map[string]bool

func (k keepSet) KeepsArchive(run, dle string) bool { return k[run+"|"+dle] }

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
	// Oldest first, by run then DLE; db skipped as protected.
	want := []Reclamation{
		{RunID: "run-2026-01-01.001", DLE: "app", Bytes: 100, Note: "over capacity"},
		{RunID: "run-2026-02-01.001", DLE: "app", Bytes: 100, Note: "over capacity"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("reclaim[%d] = %+v, want %+v", i, got[i], w)
		}
	}
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
