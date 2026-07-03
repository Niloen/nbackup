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

	got := p.Reclaim(archives, keep, time.Time{})
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

// TestVolumeProfile: the pool is volumes * volume size; zero volumes means an
// unbounded pool (a hand-loaded drive) with the reel still the per-run ceiling;
// an unsized reel is unbounded.
func TestVolumeProfile(t *testing.T) {
	cases := []struct {
		name                string
		volumes, volumeSize int64
		wantTotal, wantReel int64
	}{
		{"library multiplies out", 3, 100, 300, 100},
		{"unbounded pool, finite reel", 0, 100, 0, 100},
		{"unsized reel is unbounded", 3, 0, 0, 0},
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
