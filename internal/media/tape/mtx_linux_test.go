//go:build linux

package tape

import (
	"errors"
	"strings"
	"testing"
)

// fakeMtxRunner scripts a changer's responses without any mtx binary. responder is
// consulted for every subcommand (indexed by call order) so a test can vary the
// response as the sequence progresses.
type fakeMtxRunner struct {
	calls     [][]string
	responder func(nCall int, args []string) (string, error)
}

func (f *fakeMtxRunner) run(args ...string) (string, error) {
	n := len(f.calls)
	f.calls = append(f.calls, append([]string(nil), args...))
	return f.responder(n, args)
}

// newFakeLoader builds an mtxLoader wired to a fake runner and one drive, bypassing
// openMtxLoader (which requires mtx(1) on PATH). devs[0] is a valid mtDevice so the
// returned device handle is non-nil.
func newFakeLoader(t *testing.T, f *fakeMtxRunner) *mtxLoader {
	t.Helper()
	dev, err := openMT("/dev/nst0", 0)
	if err != nil {
		t.Fatal(err)
	}
	return &mtxLoader{nodes: []string{"/dev/nst0"}, devs: []*mtDevice{dev}, runner: f}
}

// callNames returns the subcommand verb of each recorded call (args[0]).
func callNames(f *fakeMtxRunner) []string {
	var out []string
	for _, c := range f.calls {
		if len(c) > 0 {
			out = append(out, c[0])
		}
	}
	return out
}

const statusEmptyDrive = `  Storage Changer /dev/sg0:1 Drives, 3 Slots ( 0 Import/Export )
Data Transfer Element 0:Empty
      Storage Element 1:Full :VolumeTag=E01001L8
      Storage Element 2:Full :VolumeTag=E01002L8
      Storage Element 3:Empty
`

// TestMtxLoadIntoEmptyDrive: loading slot 1 into an empty drive issues `load 1 0`
// (no prior unload) and reports the barcode the pre-load inventory read in slot 1 —
// no second status round-trip.
func TestMtxLoadIntoEmptyDrive(t *testing.T) {
	f := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		switch args[0] {
		case "status":
			return statusEmptyDrive, nil // pre-load: drive empty
		case "load":
			return "", nil
		}
		return "", errors.New("unexpected " + strings.Join(args, " "))
	}}
	ld := newFakeLoader(t, f)

	dev, bc, err := ld.load(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if dev != ld.devs[0] {
		t.Fatal("load should return the drive's persistent device handle")
	}
	if bc != "E01001L8" {
		t.Fatalf("barcode = %q, want E01001L8 (from the pre-load slot inventory)", bc)
	}
	// Sequence: status (probe), load 1 0. No unload, no barcode re-read.
	if got := callNames(f); strings.Join(got, ",") != "status,load" {
		t.Fatalf("call sequence = %v, want status,load", got)
	}
	if lc := f.calls[1]; lc[1] != "1" || lc[2] != "0" {
		t.Fatalf("load args = %v, want [load 1 0]", lc)
	}
}

// TestMtxLoadAlreadyLoaded: when the requested slot is already in the drive, load is
// a no-op that returns the current barcode — no `load`/`unload` moves.
func TestMtxLoadAlreadyLoaded(t *testing.T) {
	status := `  Storage Changer /dev/sg0:1 Drives, 3 Slots ( 0 Import/Export )
Data Transfer Element 0:Full (Storage Element 1 Loaded):VolumeTag = E01001L8
      Storage Element 2:Full :VolumeTag=E01002L8
`
	f := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		if args[0] == "status" {
			return status, nil
		}
		return "", errors.New("no move expected: " + strings.Join(args, " "))
	}}
	ld := newFakeLoader(t, f)

	_, bc, err := ld.load(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bc != "E01001L8" {
		t.Fatalf("barcode = %q, want E01001L8", bc)
	}
	if got := callNames(f); strings.Join(got, ",") != "status" {
		t.Fatalf("already-loaded should only probe status, got %v", got)
	}
}

// TestMtxLoadEvictsOccupant: loading a new slot into a drive that holds a different
// cartridge unloads the occupant to its home slot FIRST (a real drive cannot load
// while full), then loads.
func TestMtxLoadEvictsOccupant(t *testing.T) {
	occupied := `  Storage Changer /dev/sg0:1 Drives, 3 Slots ( 0 Import/Export )
Data Transfer Element 0:Full (Storage Element 2 Loaded):VolumeTag = E01002L8
      Storage Element 1:Full :VolumeTag=E01001L8
      Storage Element 2:Empty
`
	f := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		switch args[0] {
		case "status":
			return occupied, nil
		case "unload", "load":
			return "", nil
		}
		return "", errors.New("unexpected " + strings.Join(args, " "))
	}}
	ld := newFakeLoader(t, f)

	_, bc, err := ld.load(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bc != "E01001L8" {
		t.Fatalf("barcode = %q, want E01001L8", bc)
	}
	// status (probe, drive holds slot 2), unload 2 0 (evict to home), load 1 0.
	if got := callNames(f); strings.Join(got, ",") != "status,unload,load" {
		t.Fatalf("call sequence = %v, want status,unload,load", got)
	}
	if uc := f.calls[1]; uc[0] != "unload" || uc[1] != "2" || uc[2] != "0" {
		t.Fatalf("unload args = %v, want [unload 2 0] (evict to home slot 2)", uc)
	}
}

// TestMtxLoadBadDrive: a drive index outside the configured range is rejected before
// any changer move.
func TestMtxLoadBadDrive(t *testing.T) {
	f := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		return "", errors.New("no call expected")
	}}
	ld := newFakeLoader(t, f)
	if _, _, err := ld.load(1, 5); err == nil {
		t.Fatal("load into a nonexistent drive should error")
	}
	if len(f.calls) != 0 {
		t.Fatalf("a bad-drive load should not touch the changer, got %v", f.calls)
	}
}

// TestMtxUnload: unload returns a full drive's cartridge to its home slot; an already
// empty drive is a no-op (no move).
func TestMtxUnload(t *testing.T) {
	full := `Data Transfer Element 0:Full (Storage Element 3 Loaded):VolumeTag = E03L8
      Storage Element 1:Full :VolumeTag=E01L8
`
	f := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		switch args[0] {
		case "status":
			return full, nil
		case "unload":
			return "", nil
		}
		return "", errors.New("unexpected " + strings.Join(args, " "))
	}}
	ld := newFakeLoader(t, f)
	if err := ld.unload(0); err != nil {
		t.Fatal(err)
	}
	if uc := f.calls[1]; uc[0] != "unload" || uc[1] != "3" || uc[2] != "0" {
		t.Fatalf("unload args = %v, want [unload 3 0] (home slot 3)", uc)
	}

	// Empty drive: no unload move.
	fe := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		if args[0] == "status" {
			return "Data Transfer Element 0:Empty\n", nil
		}
		return "", errors.New("no move expected")
	}}
	le := newFakeLoader(t, fe)
	if err := le.unload(0); err != nil {
		t.Fatal(err)
	}
	if got := callNames(fe); strings.Join(got, ",") != "status" {
		t.Fatalf("empty-drive unload should be a no-op, got %v", got)
	}
}

// TestMtxUnloadToUnknownHome: when the drive's home slot is unknown (srcSlot -1),
// unloadTo picks the first empty non-mailslot storage slot.
func TestMtxUnloadToUnknownHome(t *testing.T) {
	// Drive full but with no "Storage Element N Loaded" clause, so srcSlot stays -1.
	status := `Data Transfer Element 0:Full :VolumeTag = E09L8
      Storage Element 1:Full :VolumeTag=E01L8
      Storage Element 2:Empty
      Storage Element 3:Empty
`
	f := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		switch args[0] {
		case "status":
			return status, nil
		case "unload":
			return "", nil
		}
		return "", errors.New("unexpected " + strings.Join(args, " "))
	}}
	ld := newFakeLoader(t, f)
	if err := ld.unloadTo(-1, 0); err != nil {
		t.Fatal(err)
	}
	// firstEmptySlot -> slot 2, so unload targets 2.
	if uc := f.calls[len(f.calls)-1]; uc[0] != "unload" || uc[1] != "2" {
		t.Fatalf("unload args = %v, want unload into first empty slot 2", uc)
	}
}

// TestMtxFirstEmptySlot: firstEmptySlot skips full slots and import/export mailslots,
// returning the first empty storage slot; it errors when none is free.
func TestMtxFirstEmptySlot(t *testing.T) {
	status := `      Storage Element 1:Full :VolumeTag=E01L8
      Storage Element 2 IMPORT/EXPORT:Empty
      Storage Element 3:Empty
`
	f := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		return status, nil
	}}
	ld := newFakeLoader(t, f)
	s, err := ld.firstEmptySlot()
	if err != nil {
		t.Fatal(err)
	}
	if s != 3 {
		t.Fatalf("firstEmptySlot = %d, want 3 (skip full 1 and mailslot 2)", s)
	}

	// No empty storage slot -> error.
	fn := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		return "      Storage Element 1:Full :VolumeTag=E01L8\n", nil
	}}
	if _, err := newFakeLoader(t, fn).firstEmptySlot(); err == nil {
		t.Fatal("firstEmptySlot with no free slot should error")
	}
}

// TestMtxSlotsCleaningFilter: a cleaning cartridge (CLN…) is reported as an empty
// slot so the librarian never mounts it, while ordinary cartridges pass through.
func TestMtxSlotsCleaningFilter(t *testing.T) {
	status := `      Storage Element 1:Full :VolumeTag=E01001L8
      Storage Element 2:Full :VolumeTag=CLN101L8
`
	f := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		return status, nil
	}}
	ld := newFakeLoader(t, f)
	slots, err := ld.slots()
	if err != nil {
		t.Fatal(err)
	}
	byNum := map[int]struct {
		full bool
		bc   string
	}{}
	for _, s := range slots {
		byNum[s.Slot] = struct {
			full bool
			bc   string
		}{s.Full, s.Barcode}
	}
	if s := byNum[1]; !s.full || s.bc != "E01001L8" {
		t.Errorf("slot 1 = %+v, want full data cartridge", s)
	}
	if s := byNum[2]; s.full || s.bc != "" {
		t.Errorf("cleaning slot 2 = %+v, want reported empty (barcode hidden)", s)
	}
}

// TestMtxLoaded: loaded reports the drive's current binding (device, barcode, home
// slot); an out-of-range drive and an empty drive both report not-loaded.
func TestMtxLoaded(t *testing.T) {
	status := `Data Transfer Element 0:Full (Storage Element 4 Loaded):VolumeTag = E04L8
`
	f := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		return status, nil
	}}
	ld := newFakeLoader(t, f)
	dev, bc, slot, ok := ld.loaded(0)
	if !ok || dev != ld.devs[0] || bc != "E04L8" || slot != 4 {
		t.Fatalf("loaded = (%v,%q,%d,%v), want device/E04L8/4/true", dev, bc, slot, ok)
	}
	if _, _, _, ok := ld.loaded(9); ok {
		t.Fatal("loaded on a nonexistent drive should be false")
	}

	fe := &fakeMtxRunner{responder: func(n int, args []string) (string, error) {
		return "Data Transfer Element 0:Empty\n", nil
	}}
	if _, _, _, ok := newFakeLoader(t, fe).loaded(0); ok {
		t.Fatal("loaded on an empty drive should be false")
	}
}

// TestMtxDriveCountManual: the mtx loader reports its drive count and is never manual
// (a robot, not a hand-loaded drive).
func TestMtxDriveCountManual(t *testing.T) {
	ld := newFakeLoader(t, &fakeMtxRunner{responder: func(int, []string) (string, error) { return "", nil }})
	if ld.driveCount() != 1 {
		t.Fatalf("driveCount = %d, want 1", ld.driveCount())
	}
	if ld.manual() {
		t.Fatal("an mtx robot is not manual")
	}
}
