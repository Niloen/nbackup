package catalog

import (
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// Slot is a run's grouping of archives, identified by its slot id — the tag every archive in
// the run carries (record.Archive.Slot). It is the catalog's in-memory unit for storage and
// display; it is not a record on the medium (the media carry only the per-archive commit
// footers, which a scan groups back into slots by their shared id). Every fact about a slot
// derives from the id and its archives: the date from the id, the byte total and last-activity
// time from the archives. The policy layer (retention, restore, recovery, drill, reclamation)
// works on the archives directly and never needs this grouping.
type Slot struct {
	ID       string           `json:"id"`
	Archives []record.Archive `json:"archives"`
}

// Date is the run date (YYYY-MM-DD) encoded in the slot id.
func (s *Slot) Date() string { return record.SlotDate(s.ID) }

// TotalBytes sums the compressed sizes of the slot's archives.
func (s *Slot) TotalBytes() int64 {
	var n int64
	for _, a := range s.Archives {
		n += a.Compressed
	}
	return n
}

// LastArchiveAt is when the slot's most recently committed archive landed — the slot's "last
// activity", for display. Zero when the slot has no archives.
func (s *Slot) LastArchiveAt() time.Time {
	var last time.Time
	for _, a := range s.Archives {
		if a.CreatedAt.After(last) {
			last = a.CreatedAt
		}
	}
	return last
}

// addArchive merges a into the slot's content, replacing any prior archive of the same
// (DLE, level). The content is the union of every archive the run produces, independent of
// which medium currently holds each copy.
func (s *Slot) addArchive(a record.Archive) {
	for i := range s.Archives {
		if s.Archives[i].DLE == a.DLE && s.Archives[i].Level == a.Level {
			s.Archives[i] = a
			return
		}
	}
	s.Archives = append(s.Archives, a)
}

// dropArchive removes a DLE's archive, the inverse of addArchive. Used when the last copy of
// that DLE's image has been reclaimed, so the slot's content no longer advertises an image no
// medium holds. Reports whether an archive was removed.
func (s *Slot) dropArchive(dle string) bool {
	kept := s.Archives[:0:0]
	removed := false
	for _, a := range s.Archives {
		if a.DLE == dle {
			removed = true
			continue
		}
		kept = append(kept, a)
	}
	s.Archives = kept
	return removed
}
