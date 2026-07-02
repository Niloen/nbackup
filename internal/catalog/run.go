package catalog

import (
	"time"

	"github.com/Niloen/nbackup/internal/record"
)

// Run is a run's grouping of archives, identified by its run id — the tag every archive in
// the run carries (record.Archive.Run). It is the catalog's in-memory unit for storage and
// display; it is not a record on the medium (the media carry only the per-archive commit
// footers, which a scan groups back into runs by their shared id). Every fact about a run
// derives from the id and its archives: the date from the id, the byte total and last-activity
// time from the archives. The policy layer (retention, restore, recovery, drill, reclamation)
// works on the archives directly and never needs this grouping.
type Run struct {
	ID       string           `json:"id"`
	Archives []record.Archive `json:"archives"`
}

// Date is the run date (YYYY-MM-DD) encoded in the run id.
func (s *Run) Date() string { return record.RunDate(s.ID) }

// TotalBytes sums the compressed sizes of the run's archives.
func (s *Run) TotalBytes() int64 {
	var n int64
	for _, a := range s.Archives {
		n += a.Compressed
	}
	return n
}

// Partial reports whether any of the run's archives is a PARTIAL dump (omitted
// unreadable source files) — the run committed, but not everything it aimed at.
func (s *Run) Partial() bool {
	for _, a := range s.Archives {
		if a.Partial() {
			return true
		}
	}
	return false
}

// LastArchiveAt is when the run's most recently committed archive landed — the run's "last
// activity", for display. Zero when the run has no archives.
func (s *Run) LastArchiveAt() time.Time {
	var last time.Time
	for _, a := range s.Archives {
		if a.CreatedAt.After(last) {
			last = a.CreatedAt
		}
	}
	return last
}

// addArchive merges a into the run's content, replacing any prior archive of the same
// (DLE, level). The content is the union of every archive the run produces, independent of
// which medium currently holds each copy.
func (s *Run) addArchive(a record.Archive) {
	for i := range s.Archives {
		if s.Archives[i].DLE == a.DLE && s.Archives[i].Level == a.Level {
			s.Archives[i] = a
			return
		}
	}
	s.Archives = append(s.Archives, a)
}

// dropArchive removes a DLE's archive, the inverse of addArchive. Used when the last copy of
// that DLE's image has been reclaimed, so the run's content no longer advertises an image no
// medium holds. Reports whether an archive was removed.
func (s *Run) dropArchive(dle string) bool {
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
