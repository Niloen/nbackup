package tape

import (
	"io"
	"testing"

	"github.com/Niloen/nbackup/internal/media"
)

func openTape(t *testing.T, dir string) media.Volume {
	t.Helper()
	v, err := media.OpenVolume("tape", media.Options{"dir": dir})
	if err != nil {
		t.Fatal(err)
	}
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

	p0, err := v.AppendFile(media.Header{Slot: "slot-x", Kind: media.KindArchive, DLE: "h-data"},
		func(w io.Writer) error { _, e := w.Write([]byte("one")); return e })
	if err != nil {
		t.Fatal(err)
	}
	p1, err := v.AppendFile(media.Header{Slot: "slot-x", Kind: media.KindSeal},
		func(w io.Writer) error { _, e := w.Write([]byte("seal")); return e })
	if err != nil {
		t.Fatal(err)
	}
	if p0 != 0 || p1 != 1 {
		t.Fatalf("expected file numbers 0,1 got %d,%d", p0, p1)
	}

	// Fast-forward read.
	h, rc, err := v.ReadFile(p0)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if h.DLE != "h-data" || string(data) != "one" {
		t.Errorf("read file 0: header %+v data %q", h, data)
	}

	// Per-slot removal is unsupported on tape.
	if err := v.RemoveSlot("slot-x"); err == nil {
		t.Error("expected RemoveSlot to be unsupported on tape")
	}

	// Reopen: scan recovers the file numbers.
	v2 := openTape(t, dir)
	files2, _ := v2.Files()
	if len(files2) != 2 {
		t.Fatalf("after reopen expected 2 files, got %d", len(files2))
	}
	p2, _ := v2.AppendFile(media.Header{Slot: "slot-y", Kind: media.KindSeal},
		func(w io.Writer) error { return nil })
	if p2 != 2 {
		t.Errorf("append after reopen got file %d, want 2", p2)
	}
}

// TestTapeLabel covers the label lifecycle: blank, write, read-back, relabel
// (which resets the volume and bumps nothing here), and foreign detection.
func TestTapeLabel(t *testing.T) {
	dir := t.TempDir()
	v := openTape(t, dir)
	lv := v.(media.Labeled)

	// Blank tape: no label.
	if _, ok, err := lv.ReadLabel(); ok || err != nil {
		t.Fatalf("blank tape: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	// Write a label; it lands at file 0 and reads back.
	want := media.Label{Name: "lto-0007", Pool: "lto", Epoch: 1}
	if err := lv.WriteLabel(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := lv.ReadLabel()
	if err != nil || !ok {
		t.Fatalf("read label: ok=%v err=%v", ok, err)
	}
	if got.Name != "lto-0007" || got.Pool != "lto" || got.Magic != media.LabelMagic {
		t.Fatalf("label round trip wrong: %+v", got)
	}

	// Append an archive after the label (file 1), then relabel: reset must discard
	// it, leaving only the new label at file 0.
	if _, err := v.AppendFile(media.Header{Slot: "s", Kind: media.KindArchive, DLE: "d"},
		func(w io.Writer) error { _, e := w.Write([]byte("x")); return e }); err != nil {
		t.Fatal(err)
	}
	if err := lv.WriteLabel(media.Label{Name: "lto-0007", Pool: "lto", Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	files, _ := v.Files()
	if len(files) != 1 || files[0].Header.Kind != media.KindLabel {
		t.Fatalf("after relabel expected only a label, got %+v", files)
	}
	if got, _, _ := lv.ReadLabel(); got.Epoch != 2 {
		t.Fatalf("after relabel epoch = %d, want 2", got.Epoch)
	}
}

// TestTapeForeignVolume: a non-empty tape whose file 0 is not our label reports
// ErrForeignVolume, so it is never silently overwritten.
func TestTapeForeignVolume(t *testing.T) {
	dir := t.TempDir()
	v := openTape(t, dir)

	// File 0 is an archive, not a label.
	if _, err := v.AppendFile(media.Header{Slot: "s", Kind: media.KindArchive, DLE: "d"},
		func(w io.Writer) error { _, e := w.Write([]byte("data")); return e }); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := v.(media.Labeled).ReadLabel(); ok || err != media.ErrForeignVolume {
		t.Fatalf("foreign volume: ok=%v err=%v, want ok=false err=ErrForeignVolume", ok, err)
	}
}
