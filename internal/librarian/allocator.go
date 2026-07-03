// allocator.go — the drive-bound Allocator implementing archiveio.PartAllocator: part sizing to the loaded volume plus the volume roll — ARCHITECTURE.md's "spanning is proactive" bullet.
package librarian

import (
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// Remaining reports the writable bytes left on the volume currently in the drive,
// when that is knowable: a finite bay/reel. ok is false for an unbounded volume, a
// medium whose remaining capacity software cannot see (a real drive reports EOT only
// by hitting it), or a non-changer medium — the caller then relies on the reactive
// media.ErrVolumeFull path instead of pre-checking.
func (l *Librarian) Remaining() (int64, bool) {
	st, ok := l.loaded()
	if !ok || st.Capacity <= 0 {
		return 0, false
	}
	if st.Used >= st.Capacity {
		return 0, true
	}
	return st.Capacity - st.Used, true
}

// Allocator drives a multi-part, possibly multi-volume write for the archiveio writer:
// it sizes each part to the loaded volume's remaining capacity (capped by part_size),
// rolls onto a fresh volume when the loaded one fills, and places the whole-file commit records.
//
// An allocator is bound to its librarian handle's drive (l.drive), so LazyDriveAllocators vends one
// per drive for concurrent writing. It starts either eagerly — over the volume PrepareWrite
// already accepted (the serial path) — or lazily: started=false, so its first NextPart runs
// PrepareWrite itself, loading a writable tape into its drive. The lazy start is what lets
// the initial per-drive load cross the spool's orchestrator on the same path as a roll,
// keeping the robot single-writer without a separate mount step.
type Allocator struct {
	l          *Librarian
	appendable bool
	partSize   int64
	now        time.Time
	logf       Logf
	tried      map[string]bool
	volume     string
	epoch      int
	started    bool   // the first volume has been accepted (PrepareWrite has run)
	expect     string // the label the run expects to (re)use, for a lazy first load
}

// Allocator builds an eager allocator starting on the volume PrepareWrite accepted (its label
// and epoch). partSize (0 = none) caps each part for media whose remaining capacity is
// unknowable or to bound part size deliberately.
func (l *Librarian) Allocator(volume string, epoch int, appendable bool, partSize int64, now time.Time, logf Logf) *Allocator {
	s := &Allocator{l: l, appendable: appendable, partSize: partSize, now: now, logf: logf,
		tried: map[string]bool{}, volume: volume, epoch: epoch, started: true}
	s.seed(volume)
	return s
}

// LazyDriveAllocators vends one lazy Allocator per drive, each bound to its own drive and
// sharing the run reservation set — the concurrent multi-drive write source. len == Drives().
// Each allocator loads its first tape on its first NextPart, so the spool can lease a drive per
// writer and the initial loads serialise on its orchestrator like any roll.
func (l *Librarian) LazyDriveAllocators(appendable bool, expect string, partSize int64, now time.Time, logf Logf) []*Allocator {
	allocs := make([]*Allocator, l.Drives())
	for i := range allocs {
		li := l.forDrive(i)
		allocs[i] = &Allocator{l: li, appendable: appendable, partSize: partSize, now: now, logf: logf,
			tried: map[string]bool{}, expect: expect}
	}
	return allocs
}

// seed records the starting volume as tried and reserved so a spanning roll never recycles
// the tape this write is already on (its fresh content is not yet in the catalog) and a
// concurrent drive never selects it.
func (s *Allocator) seed(volume string) {
	if volume != "" {
		s.tried[volume] = true
		s.l.reserve(volume)
	}
}

// ensureStarted runs the lazy first load: PrepareWrite selects and loads a writable tape
// into this allocator's drive, then the starting volume is seeded. A no-op once started.
func (s *Allocator) ensureStarted() error {
	if s.started {
		return nil
	}
	name, epoch, err := s.l.PrepareWrite(s.appendable, s.expect, s.now, s.logf)
	if err != nil {
		return err
	}
	s.volume, s.epoch, s.started = name, epoch, true
	s.seed(name)
	return nil
}

// maxPart is the payload bytes the next part may carry on the loaded volume: its
// remaining capacity minus a header, capped by part_size; -1 when unbounded.
func (s *Allocator) maxPart() int64 {
	room, known := s.l.Remaining()
	if !known {
		if s.partSize > 0 {
			return s.partSize - record.HeaderBlock
		}
		return -1
	}
	avail := room - record.HeaderBlock
	if avail < 0 {
		avail = 0
	}
	if s.partSize > 0 {
		if cap := s.partSize - record.HeaderBlock; cap < avail {
			avail = cap
		}
	}
	return avail
}

func (s *Allocator) advance() error {
	volName, epoch, _, err := s.l.Advance(s.appendable, s.tried, "", s.now, s.logf)
	if err != nil {
		// A failed roll can leave an unverified cartridge in the drive (the scan's last
		// candidate — possibly a blank reel). Drop back to unstarted so any further write
		// on this allocator re-runs PrepareWrite's label check instead of trusting the drive:
		// writing unverified would stamp archive data onto an unlabeled reel (poisoning
		// it as foreign) while the placement claims the previous volume.
		s.started = false
		return err
	}
	s.volume, s.epoch = volName, epoch
	return nil
}

// NextPart implements archiveio.PartAllocator: it rolls onto a fresh volume if the loaded
// one cannot hold a header plus a byte, then returns the volume and the part's byte cap.
func (s *Allocator) NextPart() (media.Volume, int64, string, int, error) {
	if err := s.ensureStarted(); err != nil {
		return nil, 0, "", 0, err
	}
	for max := s.maxPart(); max >= 0 && max < 1; max = s.maxPart() {
		if err := s.advance(); err != nil {
			return nil, 0, "", 0, err
		}
	}
	return s.l.driveVol(), s.maxPart(), s.volume, s.epoch, nil
}

// PlaceFile implements archiveio.PartAllocator: it rolls first if the record (one whole file
// of the given payload size — an archive's index or commit footer) will not fit the loaded
// volume.
func (s *Allocator) PlaceFile(size int64) (media.Volume, string, int, error) {
	if err := s.ensureStarted(); err != nil {
		return nil, "", 0, err
	}
	if room, known := s.l.Remaining(); known && room-record.HeaderBlock < size {
		if err := s.advance(); err != nil {
			return nil, "", 0, err
		}
	}
	return s.l.driveVol(), s.volume, s.epoch, nil
}

// Bounded implements archiveio.PartAllocator: it reports whether a part's size is ever capped —
// by a configured part_size or by a finite volume's knowable remaining capacity — so an archive
// may land as several parts (cloud splitting, or a reel spanning). The writer marks each such
// part a slice (Header.Split). False only for an unbounded medium (disk: no part_size, no
// software-visible capacity), where every archive is a single standalone part.
func (s *Allocator) Bounded() bool {
	if s.partSize > 0 {
		return true
	}
	_, known := s.l.Remaining()
	return known
}
