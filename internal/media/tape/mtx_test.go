package tape

import "testing"

// sample is real `mtx status` output (mtx 1.x against an LTO-8 library): one loaded
// drive (with source slot + spaced VolumeTag), empty drives, full/empty storage slots
// (unspaced VolumeTag), and import/export mailslots.
const sample = `  Storage Changer /dev/sg0:4 Drives, 43 Slots ( 4 Import/Export )
Data Transfer Element 0:Empty
Data Transfer Element 1:Full (Storage Element 1 Loaded):VolumeTag = E01001L8
Data Transfer Element 2:Empty
      Storage Element 1:Empty
      Storage Element 2:Full :VolumeTag=E01002L8
      Storage Element 22:Full :VolumeTag=CLN101L8
      Storage Element 40 IMPORT/EXPORT:Empty
      Storage Element 41 IMPORT/EXPORT:Full :VolumeTag=E01041L8
`

func TestParseMtxStatus(t *testing.T) {
	st := parseMtxStatus(sample)

	// Drives: drive 1 holds E01001L8 from slot 1; drives 0 and 2 are empty.
	if e := st.drives[1]; !e.full || e.srcSlot != 1 || e.barcode != "E01001L8" {
		t.Errorf("drive 1 = %+v, want full from slot 1 / E01001L8", e)
	}
	if st.drives[0].full || st.drives[2].full {
		t.Errorf("drives 0,2 should be empty: %+v %+v", st.drives[0], st.drives[2])
	}

	// Slots, keyed for assertions.
	byNum := map[int]struct {
		full bool
		bc   string
		ie   bool
	}{}
	for _, s := range st.slots {
		byNum[s.Slot] = struct {
			full bool
			bc   string
			ie   bool
		}{s.Full, s.Barcode, s.ImportExport}
	}
	if s := byNum[1]; s.full {
		t.Errorf("slot 1 should be empty (its cartridge is in a drive): %+v", s)
	}
	if s := byNum[2]; !s.full || s.bc != "E01002L8" || s.ie {
		t.Errorf("slot 2 = %+v, want full E01002L8, not a mailslot", s)
	}
	if s := byNum[22]; !s.full || s.bc != "CLN101L8" {
		t.Errorf("slot 22 (cleaning) = %+v, want full CLN101L8 (filtering is the loader's job)", s)
	}
	if s := byNum[40]; !s.ie || s.full {
		t.Errorf("slot 40 = %+v, want an empty import/export mailslot", s)
	}
	if s := byNum[41]; !s.ie || !s.full || s.bc != "E01041L8" {
		t.Errorf("slot 41 = %+v, want a full import/export mailslot E01041L8", s)
	}
}

func TestSplitDevices(t *testing.T) {
	got := splitDevices("/dev/nst0, /dev/nst1 ,, /dev/nst2")
	want := []string{"/dev/nst0", "/dev/nst1", "/dev/nst2"}
	if len(got) != len(want) {
		t.Fatalf("splitDevices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitDevices[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
