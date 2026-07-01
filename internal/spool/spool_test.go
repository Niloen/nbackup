package spool

import (
	"context"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
)

// fakeStore is a no-op archiveio.WriteStore — a landing's write target. The spool builds a writer over it
// but the test never drives it (NextPart/Commit), so the WriteStore calls never fire; the spool's Close
// hook (the run release) is what the test exercises.
type fakeStore struct{}

func (fakeStore) NextPart() (media.Volume, int64, string, int, error)  { return nil, 0, "", 0, nil }
func (fakeStore) PlaceRecord(int64) (media.Volume, string, int, error) { return nil, "", 0, nil }
func (fakeStore) Bounded() bool                                        { return false }
func (fakeStore) Record(archiveio.CommitResult) error                  { return nil }

// TestDirectPermitReleasedOnCloseWithoutCommit is the regression for the landing hang: a direct write
// that faults before Commit (the producer's deferred Close runs, Commit never does) must return its
// backing permit, so the next direct write can acquire the single run instead of blocking forever.
func TestDirectPermitReleasedOnCloseWithoutCommit(t *testing.T) {
	sp := New(context.Background(), Config{
		Backings: []Backing{{Name: "landing", Stores: []archiveio.WriteStore{fakeStore{}}, Writers: 1}},
		Holding:  NewPool(nil), // no holding disks => every write routes direct
	})
	store := sp.Ingest("landing")
	spec := archiveio.ArchiveSpec{DLE: "localhost:/data", Host: "localhost", Path: "/data"}

	// First direct write takes the only permit, then faults before commit — Close (not Commit) must
	// hand the permit back.
	sink1, err := store.NewArchive(spec, 1<<40)
	if err != nil {
		t.Fatalf("first NewArchive: %v", err)
	}
	if err := sink1.Close(); err != nil {
		t.Fatalf("Close after a faulted transfer: %v", err)
	}

	// Second direct write must acquire the freed permit without blocking; without the fix the permit
	// is leaked and this NewArchive never returns.
	done := make(chan error, 1)
	go func() {
		sink2, err := store.NewArchive(spec, 1<<40)
		if err == nil {
			_ = sink2.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second NewArchive: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second direct write blocked: the backing permit leaked on Close without Commit")
	}

	if err := sp.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}
