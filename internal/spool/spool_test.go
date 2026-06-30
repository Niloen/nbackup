package spool

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// fakeStore is a backing archiveio.ArchiveStore that hands out no-op writers — enough to exercise the
// spool's permit accounting without any real medium I/O.
type fakeStore struct{}

func (fakeStore) NewArchive(archiveio.ArchiveSpec, int64) (archiveio.ArchiveWriter, error) {
	return &fakeWriter{}, nil
}
func (fakeStore) NewCopy(record.Archive) (archiveio.ArchiveWriter, error) { return &fakeWriter{}, nil }
func (fakeStore) OpenArchive(record.Archive, record.ArchivePos) (io.ReadCloser, error) {
	return nil, nil
}
func (fakeStore) Reclaim(record.Archive, record.ArchivePos) error { return nil }

type fakeWriter struct{}

func (*fakeWriter) NextPart(context.Context) (io.WriteCloser, int64, error) { return nil, 0, nil }
func (*fakeWriter) Commit(context.Context, xfer.SourceStats) error          { return nil }
func (*fakeWriter) Result() (record.Archive, record.ArchivePos) {
	return record.Archive{}, record.ArchivePos{}
}
func (*fakeWriter) Close() error { return nil }

// TestDirectPermitReleasedOnCloseWithoutCommit is the regression for the landing hang: a direct write
// that faults before Commit (the producer's deferred Close runs, Commit never does) must return its
// backing permit, so the next direct write can acquire the single slot instead of blocking forever.
func TestDirectPermitReleasedOnCloseWithoutCommit(t *testing.T) {
	sp := New(context.Background(), Config{
		Backing: Backing{Name: "landing", Storage: fakeStore{}, Slots: 1},
		Holding: NewPool(nil), // no holding disks => every write routes direct
	})
	spec := archiveio.ArchiveSpec{DLE: "localhost:/data", Host: "localhost", Path: "/data"}

	// First direct write takes the only permit, then faults before commit — Close (not Commit) must
	// hand the permit back.
	sink1, err := sp.NewArchive(spec, 1<<40)
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
		sink2, err := sp.NewArchive(spec, 1<<40)
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
