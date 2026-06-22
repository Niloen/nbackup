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

	// A fresh tape is labeled at file 0.
	files, err := v.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Pos != 0 || files[0].Header.Kind != media.KindLabel {
		t.Fatalf("fresh tape should have a label at file 0, got %+v", files)
	}

	// Appends get consecutive file numbers.
	p1, err := v.AppendFile(media.Header{Slot: "slot-x", Kind: media.KindArchive, DLE: "h-data"},
		func(w io.Writer) error { _, e := w.Write([]byte("one")); return e })
	if err != nil {
		t.Fatal(err)
	}
	p2, err := v.AppendFile(media.Header{Slot: "slot-x", Kind: media.KindSeal},
		func(w io.Writer) error { _, e := w.Write([]byte("seal")); return e })
	if err != nil {
		t.Fatal(err)
	}
	if p1 != 1 || p2 != 2 {
		t.Fatalf("expected file numbers 1,2 got %d,%d", p1, p2)
	}

	// Fast-forward read.
	h, rc, err := v.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if h.DLE != "h-data" || string(data) != "one" {
		t.Errorf("read file 1: header %+v data %q", h, data)
	}

	// Per-slot removal is unsupported on tape.
	if err := v.RemoveSlot("slot-x"); err == nil {
		t.Error("expected RemoveSlot to be unsupported on tape")
	}

	// Reopen: scan recovers the file numbers and does not relabel.
	v2 := openTape(t, dir)
	files2, _ := v2.Files()
	if len(files2) != 3 {
		t.Fatalf("after reopen expected 3 files, got %d", len(files2))
	}
	p3, _ := v2.AppendFile(media.Header{Slot: "slot-y", Kind: media.KindSeal},
		func(w io.Writer) error { return nil })
	if p3 != 3 {
		t.Errorf("append after reopen got file %d, want 3", p3)
	}
}
