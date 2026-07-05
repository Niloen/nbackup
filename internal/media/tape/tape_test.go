package tape

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gocloud.dev/blob"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/media/bucket"
	"github.com/Niloen/nbackup/internal/record"
)

// loadSlot loads slot s (1-based) into drive 0 of a changer-medium handle.
func loadSlot(t *testing.T, v media.Volume, s int) {
	t.Helper()
	if err := v.(media.Changer).Load(s, 0); err != nil {
		t.Fatal(err)
	}
}

func openTape(t *testing.T, dir string) media.Volume {
	t.Helper()
	v, err := media.OpenVolume("tape", media.Options{"dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	// A library starts with the drive empty; load the first slot to exercise the
	// single-cartridge Volume semantics.
	loadSlot(t, v, 1)
	return v
}

func TestTapeSequential(t *testing.T) {
	dir := t.TempDir()
	v := openTape(t, dir)

	// A fresh tape is blank — no auto-label. Appends get consecutive file numbers
	// starting at 0.
	files, err := v.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("fresh tape should be blank, got %+v", files)
	}

	p0, err := writeFileT(v, record.Header{Run: "run-x", Kind: record.KindArchive, DLE: "h-data"},
		func(w io.Writer) error { _, e := w.Write([]byte("one")); return e })
	if err != nil {
		t.Fatal(err)
	}
	p1, err := writeFileT(v, record.Header{Run: "run-x", Kind: record.KindCommit},
		func(w io.Writer) error { _, e := w.Write([]byte("seal")); return e })
	if err != nil {
		t.Fatal(err)
	}
	if p0 != 0 || p1 != 1 {
		t.Fatalf("expected file numbers 0,1 got %d,%d", p0, p1)
	}

	// Fast-forward read.
	h, rc, err := v.ReadFile(p0, media.Range{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if h.DLE != "h-data" || string(data) != "one" {
		t.Errorf("read file 0: header %+v data %q", h, data)
	}

	// Per-file removal is unsupported on tape.
	if err := v.RemoveFile(0); err == nil {
		t.Error("expected RemoveFile to be unsupported on tape")
	}

	// Reopen: scan recovers the file numbers.
	v2 := openTape(t, dir)
	files2, _ := v2.Files()
	if len(files2) != 2 {
		t.Fatalf("after reopen expected 2 files, got %d", len(files2))
	}
	p2, _ := writeFileT(v2, record.Header{Run: "run-y", Kind: record.KindCommit},
		func(w io.Writer) error { return nil })
	if p2 != 2 {
		t.Errorf("append after reopen got file %d, want 2", p2)
	}
}

// TestTapeLabel covers the label lifecycle: blank, write, read-back, relabel
// (which resets the volume), and foreign detection.
func TestTapeLabel(t *testing.T) {
	dir := t.TempDir()
	v := openTape(t, dir)
	lv := v.(media.Labeled)

	// Blank tape: no label.
	if _, ok, err := lv.ReadLabel(); ok || err != nil {
		t.Fatalf("blank tape: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	// Write a label; it lands at file 0 and reads back.
	want := record.Label{Name: "lto-0007", Pool: "lto", Epoch: 1}
	if err := lv.WriteLabel(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := lv.ReadLabel()
	if err != nil || !ok {
		t.Fatalf("read label: ok=%v err=%v", ok, err)
	}
	if got.Name != "lto-0007" || got.Pool != "lto" || got.Magic != record.LabelMagic {
		t.Fatalf("label round trip wrong: %+v", got)
	}

	// Append an archive after the label (file 1), then relabel: reset must discard
	// it, leaving only the new label at file 0.
	if _, err := writeFileT(v, record.Header{Run: "s", Kind: record.KindArchive, DLE: "d"},
		func(w io.Writer) error { _, e := w.Write([]byte("x")); return e }); err != nil {
		t.Fatal(err)
	}
	if err := lv.WriteLabel(record.Label{Name: "lto-0007", Pool: "lto", Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	files, _ := v.Files()
	if len(files) != 1 || files[0].Header.Kind != record.KindLabel {
		t.Fatalf("after relabel expected only a label, got %+v", files)
	}
	if got, _, _ := lv.ReadLabel(); got.Epoch != 2 {
		t.Fatalf("after relabel epoch = %d, want 2", got.Epoch)
	}
}

// TestTapeFull: a finite emulated tape (volume_size) refuses a write past its
// capacity with ErrVolumeFull, discards the partial file, and — once relabeled —
// is reusable from the start (the directory analogue of overwriting an old reel).
func TestTapeFull(t *testing.T) {
	dir := t.TempDir()
	// Each file carries a 32 KiB header block; size the tape to hold a label and
	// one small archive, so a second archive's header overflows it.
	capacity := int64(3 * record.HeaderBlock)
	v, err := media.OpenVolume("tape", media.Options{"dir": dir, "volume_size": fmt.Sprintf("%d", capacity)})
	if err != nil {
		t.Fatal(err)
	}
	loadSlot(t, v, 1)
	lv := v.(media.Labeled)
	if err := lv.WriteLabel(record.Label{Name: "t1", Pool: "p", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFileT(v, record.Header{Run: "s", Kind: record.KindArchive, DLE: "d1"},
		func(w io.Writer) error { _, e := w.Write([]byte("x")); return e }); err != nil {
		t.Fatalf("first archive should fit: %v", err)
	}

	before, _ := v.Files()
	_, err = writeFileT(v, record.Header{Run: "s", Kind: record.KindArchive, DLE: "d2"},
		func(w io.Writer) error { _, e := w.Write(make([]byte, record.HeaderBlock)); return e })
	if !errors.Is(err, media.ErrVolumeFull) {
		t.Fatalf("write past capacity: err=%v, want ErrVolumeFull", err)
	}
	after, _ := v.Files()
	if len(after) != len(before) {
		t.Fatalf("partial file not discarded: %d files before, %d after", len(before), len(after))
	}

	// Relabel resets the volume; capacity is free again, so writes succeed.
	if err := lv.WriteLabel(record.Label{Name: "t1", Pool: "p", Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFileT(v, record.Header{Run: "s2", Kind: record.KindArchive, DLE: "d1"},
		func(w io.Writer) error { _, e := w.Write([]byte("y")); return e }); err != nil {
		t.Fatalf("relabeled tape should accept writes again: %v", err)
	}
}

// TestTapeLibrary covers the robotic changer: a library is stocked with N blank slots
// (each reporting a simulated barcode), a slot loads into a drive, the load persists
// across reopen, and a loaded slot reports empty in the inventory.
func TestTapeLibrary(t *testing.T) {
	dir := t.TempDir()
	v, err := media.OpenVolume("tape", media.Options{"dir": dir, "slots": "3"})
	if err != nil {
		t.Fatal(err)
	}
	ch := v.(media.Changer)
	if ch.Manual() {
		t.Fatal("a dir library without manual: true should be a robot (Manual()==false)")
	}

	slots, err := ch.Slots()
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 3 {
		t.Fatalf("expected 3 slots, got %d", len(slots))
	}
	for _, s := range slots {
		if !s.Full || s.Barcode != simBarcode(s.Slot) {
			t.Fatalf("fresh slot %d should be full with a barcode, got %+v", s.Slot, s)
		}
	}
	if drs, _ := ch.Drives(); len(drs) != 1 || drs[0].Loaded {
		t.Fatalf("a fresh library should have one empty drive, got %+v", drs)
	}

	// Load slot 1 and label it, then slot 2 — slots are independent cartridges.
	if err := ch.Load(1, 0); err != nil {
		t.Fatal(err)
	}
	if err := v.(media.Labeled).WriteLabel(record.Label{Name: "VOL-A", Pool: "p", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := ch.Load(2, 0); err != nil {
		t.Fatal(err)
	}
	if err := v.(media.Labeled).WriteLabel(record.Label{Name: "VOL-B", Pool: "p", Epoch: 1}); err != nil {
		t.Fatal(err)
	}

	// A loaded slot (2) reports empty in the slot inventory; the others stay full.
	slots, _ = ch.Slots()
	for _, s := range slots {
		if s.Slot == 2 && s.Full {
			t.Fatalf("slot 2 is in a drive and should report empty: %+v", s)
		}
		if s.Slot != 2 && !s.Full {
			t.Fatalf("slot %d should be full: %+v", s.Slot, s)
		}
	}

	// Reopen: the drive binding persists (slot 2 / VOL-B), and loading slot 1 yields VOL-A.
	v2, err := media.OpenVolume("tape", media.Options{"dir": dir, "slots": "3"})
	if err != nil {
		t.Fatal(err)
	}
	ch2 := v2.(media.Changer)
	if st, ok := ch2.Drive(0).Loaded(); !ok || st.Label != "VOL-B" {
		t.Fatalf("loaded after reopen = %q (ok=%v), want VOL-B", st.Label, ok)
	}
	if err := ch2.Load(1, 0); err != nil {
		t.Fatal(err)
	}
	if name, _, _ := readVolumeName(v2); name != "VOL-A" {
		t.Fatalf("slot 1 holds %q, want VOL-A", name)
	}
}

// TestManualChanger covers the hand-loaded single drive (manual: true): the drive
// starts empty, the cartridges sit in slots the operator picks from, a Load simulates
// the operator's hands, and the loaded cartridge survives reopen. Manual() is true so
// the librarian runs the operator-prompt path.
func TestManualChanger(t *testing.T) {
	dir := t.TempDir()
	v, err := media.OpenVolume("tape", media.Options{"dir": dir, "manual": "true", "slots": "3"})
	if err != nil {
		t.Fatal(err)
	}
	ch, ok := v.(media.Changer)
	if !ok {
		t.Fatal("a manual tape medium is still a media.Changer")
	}
	if !ch.Manual() {
		t.Fatal("manual: true should report Manual()==true")
	}

	// The drive is empty to start; all three cartridges are in slots.
	if st, ok := ch.Drive(0).Loaded(); ok {
		t.Fatalf("a fresh manual drive should be empty, got %+v", st)
	}
	if slots, _ := ch.Slots(); len(slots) != 3 {
		t.Fatalf("expected 3 slots, got %d", len(slots))
	}

	// The operator loads a cartridge and labels it.
	if err := ch.Load(1, 0); err != nil {
		t.Fatal(err)
	}
	if err := v.(media.Labeled).WriteLabel(record.Label{Name: "VOL-A", Pool: "p", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if st, ok := ch.Drive(0).Loaded(); !ok || st.Label != "VOL-A" {
		t.Fatalf("drive should hold VOL-A, got %+v ok=%v", st, ok)
	}

	// Swap to a different cartridge: the one drive changes content.
	if err := ch.Load(2, 0); err != nil {
		t.Fatal(err)
	}
	if err := v.(media.Labeled).WriteLabel(record.Label{Name: "VOL-B", Pool: "p", Epoch: 1}); err != nil {
		t.Fatal(err)
	}

	// Reopen: the loaded cartridge persists.
	v2, err := media.OpenVolume("tape", media.Options{"dir": dir, "manual": "true", "slots": "3"})
	if err != nil {
		t.Fatal(err)
	}
	if name, _, _ := readVolumeName(v2); name != "VOL-B" {
		t.Fatalf("after reopen the drive should hold VOL-B, got %q", name)
	}
}

// readVolumeName reads the loaded volume's label name (test helper).
func readVolumeName(v media.Volume) (string, bool, error) {
	lbl, ok, err := v.(media.Labeled).ReadLabel()
	return lbl.Name, ok, err
}

// TestTapeForeignVolume: a non-empty tape whose file 0 is not our label reports
// ErrForeignVolume, so it is never silently overwritten.
func TestTapeForeignVolume(t *testing.T) {
	dir := t.TempDir()
	v := openTape(t, dir)

	// File 0 is an archive, not a label.
	if _, err := writeFileT(v, record.Header{Run: "s", Kind: record.KindArchive, DLE: "d"},
		func(w io.Writer) error { _, e := w.Write([]byte("data")); return e }); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := v.(media.Labeled).ReadLabel(); ok || err != media.ErrForeignVolume {
		t.Fatalf("foreign volume: ok=%v err=%v, want ok=false err=ErrForeignVolume", ok, err)
	}
}

// TestTapeTornTailSkipped simulates a hard kill mid-append: a numbered record
// exists but its header block was never fully written, so it cannot decode. Writes
// are serialized, so a partial is always the tail; Files() must skip it and still
// enumerate the committed records, never abort the scan.
func TestTapeTornTailSkipped(t *testing.T) {
	dir := t.TempDir()
	v := openTape(t, dir)
	if _, err := writeFileT(v, record.Header{Run: "s", Kind: record.KindArchive, DLE: "d"},
		func(w io.Writer) error { _, e := w.Write([]byte("one")); return e }); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFileT(v, record.Header{Run: "s", Kind: record.KindCommit},
		func(w io.Writer) error { _, e := w.Write([]byte("seal")); return e }); err != nil {
		t.Fatal(err)
	}

	// Plant a torn trailing record: a numbered file too short to hold a header block.
	torn := filepath.Join(dir, slotName(1), "000002")
	if err := os.WriteFile(torn, []byte("torn"), 0o644); err != nil {
		t.Fatal(err)
	}

	v2 := openTape(t, dir)
	files, err := v2.Files()
	if err != nil {
		t.Fatalf("Files() with a torn tail: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("torn tail not skipped: got %d files, want 2", len(files))
	}
}

// TestTapeBucketURL: `dir` also accepts a gocloud bucket URL, so the emulated
// library can live in an object store. mem:// exercises the pure-URL path in one
// handle; a file:// URL (the same driver a cloud bucket would swap in for)
// verifies the full lifecycle including persistence across reopen.
func TestTapeBucketURL(t *testing.T) {
	// mem://: label a slot and read it back through the changer facets.
	v, err := media.OpenVolume("tape", media.Options{"dir": "mem://", "slots": "2"})
	if err != nil {
		t.Fatal(err)
	}
	loadSlot(t, v, 1)
	if err := v.(media.Labeled).WriteLabel(record.Label{Name: "CLD-A", Pool: "p", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if name, ok, err := readVolumeName(v); !ok || err != nil || name != "CLD-A" {
		t.Fatalf("mem bucket label = %q ok=%v err=%v, want CLD-A", name, ok, err)
	}

	// file:// URL: same driver family, but durable — the load and the data survive reopen.
	url := "file://" + t.TempDir() + "?metadata=skip"
	v, err = media.OpenVolume("tape", media.Options{"dir": url, "slots": "2"})
	if err != nil {
		t.Fatal(err)
	}
	loadSlot(t, v, 2)
	if err := v.(media.Labeled).WriteLabel(record.Label{Name: "CLD-B", Pool: "p", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFileT(v, record.Header{Run: "s", Kind: record.KindArchive, DLE: "d"},
		func(w io.Writer) error { _, e := w.Write([]byte("payload")); return e }); err != nil {
		t.Fatal(err)
	}

	v2, err := media.OpenVolume("tape", media.Options{"dir": url, "slots": "2"})
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := v2.(media.Changer).Drive(0).Loaded(); !ok || st.Label != "CLD-B" {
		t.Fatalf("drive after reopen = %+v ok=%v, want CLD-B loaded", st, ok)
	}
	if name, ok, err := readVolumeName(v2); !ok || err != nil || name != "CLD-B" {
		t.Fatalf("after reopen label = %q ok=%v err=%v, want CLD-B", name, ok, err)
	}
	files, err := v2.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[1].Header.DLE != "d" {
		t.Fatalf("after reopen expected label+archive, got %+v", files)
	}
}

// TestTapeCorruptLabelForeign: file 0 exists but is too short to decode a header (a
// torn/truncated LABEL, distinct from the torn-tail test which plants a non-zero
// file). readLabel must surface it as an error and deviceStatus must classify the
// volume foreign — never blank and never crash — so the overwrite guard refuses it
// until a forced relabel.
func TestTapeCorruptLabelForeign(t *testing.T) {
	dir := t.TempDir()

	// Plant a truncated file 0: a numbered key (so it is counted, not "foreign keys")
	// whose bytes cannot decode a header block.
	slotDir := filepath.Join(dir, slotName(1))
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slotDir, "000000"), []byte("torn-label"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Open so the device scans and counts the planted file.
	v2 := openTape(t, dir)
	if _, ok, err := v2.(media.Labeled).ReadLabel(); ok || err == nil {
		t.Fatalf("corrupt file-0 label: ok=%v err=%v, want ok=false with an error", ok, err)
	}
	// The Drive status derives from deviceStatus, which must mark it foreign (not blank).
	st, ok := v2.(media.Changer).Drive(0).Loaded()
	if !ok {
		t.Fatal("drive should report loaded (a cartridge is present, just corrupt)")
	}
	if !st.Foreign || st.Blank {
		t.Fatalf("corrupt label status = %+v, want Foreign && !Blank", st)
	}
}

// fakeLoader is a minimal loader whose per-slot load behaviour a test dictates, so the
// changer's load-failure cleanup and WalkReadable's skip-and-continue can be exercised
// without a real changer backend.
type fakeLoader struct {
	nDrives  int
	slotList []media.SlotStatus
	// loadFn binds slot->drive; returning an error simulates an unloadable cartridge.
	loadFn func(slot, drive int) (device, string, error)
}

func (f *fakeLoader) driveCount() int                    { return f.nDrives }
func (f *fakeLoader) manual() bool                       { return false }
func (f *fakeLoader) slots() ([]media.SlotStatus, error) { return f.slotList, nil }
func (f *fakeLoader) load(slot, drive int) (device, string, error) {
	return f.loadFn(slot, drive)
}
func (f *fakeLoader) unload(int) error     { return nil }
func (f *fakeLoader) driveNode(int) string { return "" }
func (f *fakeLoader) loaded(int) (device, string, int, bool) {
	return nil, "", -1, false
}

// TestChangerLoadFailureClearsBinding: a load that errors (a rejected/unloadable
// cartridge) must leave the drive reporting empty, not a phantom tape whose later
// device open would fail with "no medium".
func TestChangerLoadFailureClearsBinding(t *testing.T) {
	ld := &fakeLoader{
		nDrives: 1,
		loadFn: func(slot, drive int) (device, string, error) {
			return nil, "", errors.New("wrong-generation cartridge")
		},
	}
	c, err := newTapeChanger(ld, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Load(1, 0); err == nil {
		t.Fatal("Load should surface the loader error")
	}
	if _, ok := c.Drive(0).Loaded(); ok {
		t.Fatal("a failed load must leave the drive empty, not a phantom binding")
	}
}

// TestWalkReadableSkipsUnloadable: WalkReadable loads each occupied slot into drive 0
// in turn; a slot whose load fails is skipped (it holds nothing readable), and the
// scan still visits the slots that do load.
func TestWalkReadableSkipsUnloadable(t *testing.T) {
	// A dir-backed device for the slot that loads successfully.
	good, err := openDir(context.Background(), memBucket(t), "ok/", 0)
	if err != nil {
		t.Fatal(err)
	}
	ld := &fakeLoader{
		nDrives: 1,
		slotList: []media.SlotStatus{
			{Slot: 1, Full: true},
			{Slot: 2, Full: true},
			{Slot: 3, Full: false},                    // empty: never attempted
			{Slot: 4, Full: true, ImportExport: true}, // mailslot: skipped
		},
		loadFn: func(slot, drive int) (device, string, error) {
			if slot == 1 {
				return nil, "", errors.New("stuck cartridge")
			}
			return good, "BC", nil
		},
	}
	c, err := newTapeChanger(ld, 0)
	if err != nil {
		t.Fatal(err)
	}

	var visited int
	if err := media.WalkReadable(c, func(media.Volume) error {
		visited++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Only slot 2 loads: slot 1 errors (skipped), 3 is empty, 4 is a mailslot.
	if visited != 1 {
		t.Fatalf("WalkReadable visited %d cartridges, want 1 (slot 2 only)", visited)
	}
}

// memBucket opens a throwaway in-memory bucket for device fixtures.
func memBucket(t *testing.T) *blob.Bucket {
	t.Helper()
	b, err := bucket.Open(context.Background(), "mem://")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// writeFileT bridges tests to the writer-based AppendFile (callback shape kept for brevity).
func writeFileT(v media.Volume, h record.Header, write func(io.Writer) error) (int, error) {
	fw, err := v.AppendFile(context.Background(), h)
	if err != nil {
		return 0, err
	}
	if err := write(fw); err != nil {
		fw.Close()
		return 0, err
	}
	if err := fw.Close(); err != nil {
		return 0, err
	}
	return fw.Pos(), nil
}

// TestVolumeProfileShapeAware: the tape profile factory reads the same option keys
// the changer factory does, so the planner's capacity never disagrees with the
// medium. A file-backed library counts "slots" (robotic or manual); a real drive
// ("device") has an unbounded pool but a finite reel; and an unsized reel is
// unbounded. The count defaults to one, matching the changer.
func TestVolumeProfileShapeAware(t *testing.T) {
	cases := []struct {
		name      string
		opts      media.Options
		wantTotal int64 // retainable pool (TotalBytes)
		wantReel  int64 // per-run reel ceiling (VolumeSize)
	}{
		// TotalBytes nets the per-reel framing overhead (label + one part header) from
		// each reel's payload; VolumeSize reports the raw reel ceiling. reel = 1 MiB
		// (1048576) → usable 1048576-65536 = 983040 per reel.
		{"library counts cartridges", media.Options{"dir": "/x", "slots": "3", "volume_size": "1048576"}, 3 * 983040, 1048576},
		{"manual station counts reels", media.Options{"dir": "/x", "manual": "true", "slots": "4", "volume_size": "1048576"}, 4 * 983040, 1048576},
		{"bare drive: unbounded pool, finite reel", media.Options{"device": "/dev/nst0", "volume_size": "1048576"}, 0, 1048576},
		{"count defaults to one", media.Options{"dir": "/x", "volume_size": "1048576"}, 983040, 1048576},
		{"unsized reel is unbounded", media.Options{"dir": "/x", "slots": "3"}, 0, 0},
		{"reel smaller than its framing holds nothing usable", media.Options{"dir": "/x", "slots": "3", "volume_size": "100"}, 0, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := newVolumeProfile(tc.opts)
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
