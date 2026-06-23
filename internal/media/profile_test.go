package media

import "testing"

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
