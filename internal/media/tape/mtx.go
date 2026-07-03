package tape

import (
	"bufio"
	"strconv"
	"strings"

	"github.com/Niloen/nbackup/internal/media"
)

// mtxStatus is the parsed inventory of `mtx status`: which cartridge each drive
// holds (by source slot + barcode) and the storage/mailslot elements.
type mtxStatus struct {
	drives map[int]mtxDrive
	slots  []media.SlotStatus
}

// mtxDrive is one data-transfer element's state from `mtx status`.
type mtxDrive struct {
	full    bool
	srcSlot int // the storage slot the cartridge came from (for unload-home); -1 if unknown
	barcode string
}

// parseMtxStatus parses `mtx status` output. The format (verified against mtx 1.x):
//
//	  Storage Changer /dev/sg0:4 Drives, 43 Slots ( 4 Import/Export )
//	Data Transfer Element 0:Empty
//	Data Transfer Element 1:Full (Storage Element 1 Loaded):VolumeTag = E01001L8
//	      Storage Element 2:Full :VolumeTag=E01002L8
//	      Storage Element 40 IMPORT/EXPORT:Empty
//
// Barcodes (VolumeTag) appear with spaces around `=` on drive lines and without on
// storage lines, and are right-padded with spaces — both handled.
func parseMtxStatus(out string) mtxStatus {
	st := mtxStatus{drives: map[int]mtxDrive{}}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Data Transfer Element"):
			if d, e, ok := parseMtxDriveLine(line); ok {
				st.drives[d] = e
			}
		case strings.HasPrefix(line, "Storage Element"):
			if s, ok := parseMtxSlotLine(line); ok {
				st.slots = append(st.slots, s)
			}
		}
	}
	return st
}

// parseMtxDriveLine parses "Data Transfer Element <d>:<state>".
func parseMtxDriveLine(line string) (int, mtxDrive, bool) {
	head := strings.TrimPrefix(line, "Data Transfer Element ")
	colon := strings.IndexByte(head, ':')
	if colon < 0 {
		return 0, mtxDrive{}, false
	}
	d, err := strconv.Atoi(strings.TrimSpace(head[:colon]))
	if err != nil {
		return 0, mtxDrive{}, false
	}
	rest := head[colon+1:]
	if !strings.HasPrefix(rest, "Full") {
		return d, mtxDrive{srcSlot: -1}, true // Empty
	}
	e := mtxDrive{full: true, srcSlot: -1, barcode: parseMtxBarcode(rest)}
	// "Full (Storage Element <S> Loaded):VolumeTag = …"
	if j := strings.Index(rest, "Storage Element "); j >= 0 {
		if f := strings.Fields(rest[j+len("Storage Element "):]); len(f) > 0 {
			if n, err := strconv.Atoi(f[0]); err == nil {
				e.srcSlot = n
			}
		}
	}
	return d, e, true
}

// parseMtxSlotLine parses "Storage Element <S>[ IMPORT/EXPORT]:<state>".
func parseMtxSlotLine(line string) (media.SlotStatus, bool) {
	head := strings.TrimPrefix(line, "Storage Element ")
	colon := strings.IndexByte(head, ':')
	if colon < 0 {
		return media.SlotStatus{}, false
	}
	label, rest := head[:colon], head[colon+1:]
	fields := strings.Fields(label)
	if len(fields) == 0 {
		return media.SlotStatus{}, false
	}
	s, err := strconv.Atoi(fields[0])
	if err != nil {
		return media.SlotStatus{}, false
	}
	return media.SlotStatus{
		Slot:         s,
		Full:         strings.HasPrefix(rest, "Full"),
		ImportExport: strings.Contains(label, "IMPORT/EXPORT"),
		Barcode:      parseMtxBarcode(rest),
	}, true
}

// parseMtxBarcode extracts the VolumeTag from a status line, tolerating the
// `VolumeTag = X` and `VolumeTag=X` forms and trailing padding.
func parseMtxBarcode(s string) string {
	i := strings.Index(s, "VolumeTag")
	if i < 0 {
		return ""
	}
	rest := strings.TrimLeft(s[i+len("VolumeTag"):], " =")
	if f := strings.Fields(rest); len(f) > 0 {
		return f[0]
	}
	return ""
}

// splitDevices parses a comma-separated list of drive device nodes.
func splitDevices(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
