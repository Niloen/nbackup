package librarian

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// fakeReel is a labeled cartridge in a drive, for exercising the librarian's fill
// arithmetic in isolation: no medium reports fill (a tape cannot see its own), so
// Remaining() must come from the catalog-derived snapshot plus counted landings,
// both priced by the medium's own cost rule (media.FileCoster).
type fakeReel struct {
	lbl      record.Label
	labeled  bool
	capacity int64
}

type fakeDrive struct{ r *fakeReel }

// FileCost is the fake medium's pricing rule: framing of 10 per file, unrecorded
// meta payloads (label, commit) bounded at 100 — deliberately unlike tape's, to
// prove the librarian only ever applies the medium's rule.
func (d fakeDrive) FileCost(kind string, payload int64) int64 {
	switch kind {
	case record.KindLabel, record.KindCommit:
		payload = 100
	}
	return 10 + payload
}

func (d fakeDrive) AppendFile(context.Context, record.Header) (media.FileWriter, error) {
	return &fakeWriter{}, nil
}
func (d fakeDrive) ReadFile(int, media.Range) (record.Header, io.ReadCloser, error) {
	return record.Header{}, nil, fmt.Errorf("fakeDrive: no read path")
}
func (d fakeDrive) Files() ([]record.FileInfo, error) { return nil, nil }
func (d fakeDrive) RemoveFile(int) error              { return media.ErrNoFileRemoval }
func (d fakeDrive) ReadLabel() (record.Label, bool, error) {
	return d.r.lbl, d.r.labeled, nil
}
func (d fakeDrive) WriteLabel(lbl record.Label) error {
	d.r.lbl, d.r.labeled = lbl, true
	return nil
}
func (d fakeDrive) Loaded() (media.VolumeStatus, bool) {
	st := media.VolumeStatus{Capacity: d.r.capacity}
	if d.r.labeled {
		st.Label, st.Pool = d.r.lbl.Name, d.r.lbl.Pool
	}
	return st, true
}

// fakeWriter discards the payload; the countedWriter wrapping it does the metering.
type fakeWriter struct{}

func (w *fakeWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *fakeWriter) Close() error                { return nil }
func (w *fakeWriter) Pos() int                    { return 0 }

// fakeChanger is a one-drive manual station holding the fakeReel.
type fakeChanger struct{ fakeDrive }

func (c fakeChanger) Slots() ([]media.SlotStatus, error) { return nil, nil }
func (c fakeChanger) Drives() ([]media.DriveStatus, error) {
	st, _ := c.Loaded()
	return []media.DriveStatus{{Drive: 0, Loaded: true, FromSlot: -1, Volume: st}}, nil
}
func (c fakeChanger) Drive(int) media.Drive { return c.fakeDrive }
func (c fakeChanger) Load(int, int) error   { return media.ErrManualLoad }
func (c fakeChanger) Unload(int) error      { return media.ErrManualLoad }
func (c fakeChanger) Manual() bool          { return true }

// TestRemainingFromStoredFill locks the fill arithmetic behind proactive
// spanning: with a declared volume_size (Capacity), Remaining() is unknowable
// until the label protocol accepts the reel, then equals declared capacity minus
// the reel's stored fill at accept (VolumeRecord.Used, maintained by the catalog
// at record time with the medium's FileCost), minus each file the allocator
// lands after — priced by the SAME rule, so the record-time figure, the landing,
// and the next accept's snapshot can never disagree.
func TestRemainingFromStoredFill(t *testing.T) {
	now := time.Now()
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Wire the stored fill's pricing to the fake medium's rule, as the engine
	// wires the real resolver — the pool name is the medium name.
	cat.PriceWith(func(medium string) (func(kind string, payload int64) int64, bool) {
		if medium != "pool" {
			return nil, false
		}
		return fakeDrive{}.FileCost, true
	})
	lbl := record.Label{Name: "T1", Pool: "pool", Epoch: 1, WrittenAt: now.Add(-time.Hour)}
	if err := cat.RecordVolume(lbl); err != nil {
		t.Fatal(err)
	}
	// A prior run's archive already on T1: one 50000-byte sealed part + commit.
	prior := record.Archive{Run: "r1", DLE: "h:/d", Level: 0, Compressed: 50000,
		PartSeals: []record.PartSeal{{Size: 50000}}}
	if err := cat.AddArchive(prior, "pool", archiveio.ArchivePos{
		Parts:  []archiveio.FilePos{{Label: "T1", Epoch: 1, Pos: 1}},
		Commit: archiveio.FilePos{Label: "T1", Epoch: 1, Pos: 2},
	}); err != nil {
		t.Fatal(err)
	}

	const capacity = 300000
	drive := fakeDrive{&fakeReel{lbl: lbl, labeled: true, capacity: capacity}}
	l := New(fakeChanger{drive}, "pool", cat, nil, false, 0)

	// Before the write accepts the reel its fill is unknowable — a capacity alone
	// must not be trusted.
	if _, known := l.Remaining(); known {
		t.Fatal("Remaining should be unknown before the reel is accepted for writing")
	}

	name, epoch, err := l.PrepareWrite(true, "", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if name != "T1" {
		t.Fatalf("accepted %q, want T1", name)
	}
	v, _ := cat.Volume("T1")
	base := v.Used // label + part + commit, priced at record time by the fake's rule
	wantRoom := int64(capacity) - base
	if room, known := l.Remaining(); !known || room != wantRoom {
		t.Fatalf("Remaining after accept = %d (known=%v), want %d", room, known, wantRoom)
	}

	// Land one part through the allocator's volume: it costs the medium's price
	// for its kind and payload, the moment the file commits.
	alloc := l.Allocator(name, epoch, true, 0, now, nil)
	vol, max, _, _, err := alloc.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if wantMax := wantRoom - drive.FileCost(record.KindArchive, 0); max != wantMax {
		t.Fatalf("part cap = %d, want %d", max, wantMax)
	}
	fw, err := vol.AppendFile(context.Background(), record.Header{Kind: record.KindArchive})
	if err != nil {
		t.Fatal(err)
	}
	const payload = 10000
	if _, err := fw.Write(make([]byte, payload)); err != nil {
		t.Fatal(err)
	}
	if room, known := l.Remaining(); !known || room != wantRoom {
		t.Fatalf("Remaining mid-file = %d (known=%v), want %d (files land on Close)", room, known, wantRoom)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	want := wantRoom - drive.FileCost(record.KindArchive, payload)
	if room, known := l.Remaining(); !known || room != want {
		t.Fatalf("Remaining after landing = %d (known=%v), want %d", room, known, want)
	}

	// Re-accepting the SAME reel (same label and epoch — e.g. an operator who
	// pressed Enter at a swap prompt without swapping) must KEEP the running
	// count: the stored figure cannot include the in-flight archive's parts, so a
	// re-snapshot here would forget them and let the write overshoot the reel.
	if _, _, err := l.PrepareWrite(true, "", now, nil); err != nil {
		t.Fatal(err)
	}
	if room, known := l.Remaining(); !known || room != want {
		t.Fatalf("Remaining after same-reel re-accept = %d (known=%v), want %d (landed kept)", room, known, want)
	}
}
