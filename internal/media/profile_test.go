package media

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// keepSet is a fake Retention floor keyed by "slot|dle".
type keepSet map[string]bool

func (k keepSet) KeepsArchive(slot, dle string) bool { return k[slot+"|"+dle] }

// TestSizeProfileReclaimsPerArchive: reclamation walks archives (slot+DLE), not whole
// slots — oldest-first, skipping protected archives, stopping once under capacity. So
// an old slot loses its reclaimable DLE while a protected slot-mate stays.
func TestSizeProfileReclaimsPerArchive(t *testing.T) {
	archives := []record.Archive{
		{Slot: "slot-2026-01-01", DLE: "app", Level: 0, Compressed: 100},
		{Slot: "slot-2026-01-01", DLE: "db", Level: 0, Compressed: 100},
		{Slot: "slot-2026-02-01", DLE: "app", Level: 1, Compressed: 100},
		{Slot: "slot-2026-02-01", DLE: "db", Level: 1, Compressed: 100},
	}
	// db is protected in both slots (its live chain); app's archives are reclaimable.
	keep := keepSet{"slot-2026-01-01|db": true, "slot-2026-02-01|db": true}
	p := sizeProfile{capacity: 250} // total 400 → free 150 (two archives)

	got := p.Reclaim(archives, keep, time.Time{})
	if len(got) != 2 {
		t.Fatalf("reclaimed %d archives, want 2: %+v", len(got), got)
	}
	// Oldest first, by slot then DLE; db skipped as protected.
	want := []Reclamation{
		{SlotID: "slot-2026-01-01", DLE: "app", Bytes: 100, Note: "over capacity"},
		{SlotID: "slot-2026-02-01", DLE: "app", Bytes: 100, Note: "over capacity"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("reclaim[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestVolumeProfileShapeAware: the volume profile reads the same keys the tape
// changer does, so the planner's capacity never disagrees with the medium. A
// robotic library counts "bays"; a manual ShelfStation (mode: manual) counts
// "reels"; a bare drive ("device") has an unbounded pool but a finite reel; and
// an unsized reel is unbounded. The count defaults to one, matching the changer.
func TestVolumeProfileShapeAware(t *testing.T) {
	cases := []struct {
		name      string
		opts      Options
		wantTotal int64 // retainable pool (TotalBytes)
		wantReel  int64 // per-run reel ceiling (VolumeSize)
	}{
		{"library counts bays", Options{"dir": "/x", "bays": "3", "volume_size": "100"}, 300, 100},
		{"manual station counts reels", Options{"dir": "/x", "mode": "manual", "reels": "4", "volume_size": "100"}, 400, 100},
		{"bare drive: unbounded pool, finite reel", Options{"device": "/dev/nst0", "volume_size": "100"}, 0, 100},
		{"count defaults to one", Options{"dir": "/x", "volume_size": "100"}, 100, 100},
		{"unsized reel is unbounded", Options{"dir": "/x", "bays": "3"}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewVolumeProfile(tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := p.TotalBytes(); got != tc.wantTotal {
				t.Errorf("TotalBytes = %d, want %d", got, tc.wantTotal)
			}
			if got := p.VolumeSize(); got != tc.wantReel {
				t.Errorf("VolumeSize = %d, want %d", got, tc.wantReel)
			}
		})
	}
}
