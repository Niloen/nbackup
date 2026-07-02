//go:build linux

package tape

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Niloen/nbackup/internal/media"
)

// mtxLoader is the real robotic-library backend: it drives a SCSI media changer via
// mtx(1) over the changer's control device (an sg node), moving cartridges between
// storage slots and tape drives. It is the loader half of a tapeChanger; the byte I/O
// of each drive is an mtDevice over its /dev/nstN node. This is Amanda's chg-robot in
// shape: inventory comes from `mtx status` (barcodes read without loading), and a load
// is `mtx load <slot> <drive>`.
//
// The librarian schedules drive 0 today, but the loader models every configured drive
// so multi-drive scheduling is a librarian change, not a backend one.
type mtxLoader struct {
	control string      // the changer control device (sg node) passed to `mtx -f`
	nodes   []string    // drive device nodes (/dev/nstN); index = drive number
	devs    []*mtDevice // persistent byte handle per drive (the cartridge swaps under it)
	runner  mtxRunner   // runs mtx subcommands (execMtxRunner in production; a fake in tests)
}

// mtxRunner runs one mtx(1) subcommand against a changer and returns its combined
// output. It is the exec seam: production shells out (execMtxRunner) while tests
// script the changer's responses without any mtx binary or hardware.
type mtxRunner interface {
	run(args ...string) (string, error)
}

// execMtxRunner runs mtx(1) against a control device via os/exec.
type execMtxRunner struct {
	control string
}

func (r execMtxRunner) run(args ...string) (string, error) {
	out, err := exec.Command("mtx", append([]string{"-f", r.control}, args...)...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("mtx %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// openMtxLoader builds an mtx-backed loader: one mtDevice per drive node, validated
// against the configured block size, and a check that mtx(1) is on PATH.
func openMtxLoader(control string, nodes []string, block int) (loader, error) {
	if _, err := exec.LookPath("mtx"); err != nil {
		return nil, fmt.Errorf("tape changer %q needs mtx(1) on PATH: %w", control, err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("a tape changer needs at least one drive device (device: /dev/nst0)")
	}
	devs := make([]*mtDevice, len(nodes))
	for i, n := range nodes {
		d, err := openMT(n, block)
		if err != nil {
			return nil, err
		}
		devs[i] = d
	}
	return &mtxLoader{control: control, nodes: nodes, devs: devs, runner: execMtxRunner{control: control}}, nil
}

func (m *mtxLoader) driveCount() int { return len(m.nodes) }
func (m *mtxLoader) manual() bool    { return false }

// mtx runs one mtx subcommand against the control device.
func (m *mtxLoader) mtx(args ...string) (string, error) {
	return m.runner.run(args...)
}

func (m *mtxLoader) status() (mtxStatus, error) {
	out, err := m.mtx("status")
	if err != nil {
		return mtxStatus{}, err
	}
	return parseMtxStatus(out), nil
}

// slots inventories the storage elements (by barcode), as the library scanner reports
// them without loading anything. Cleaning cartridges (barcode "CLN…") are reported as
// empty slots so the librarian never loads one into a drive (a load would start a
// drive-cleaning cycle, not a mount) — the chg-robot cleaning-tape convention.
func (m *mtxLoader) slots() ([]media.SlotStatus, error) {
	st, err := m.status()
	if err != nil {
		return nil, err
	}
	for i := range st.slots {
		if isCleaningBarcode(st.slots[i].Barcode) {
			st.slots[i].Full = false
			st.slots[i].Barcode = ""
		}
	}
	return st.slots, nil
}

// isCleaningBarcode reports the LTO cleaning-cartridge barcode convention (CLNxxxLx).
func isCleaningBarcode(bc string) bool { return strings.HasPrefix(bc, "CLN") }

// load moves the cartridge in slot into drive (a robot move). A real drive cannot be
// loaded while full, so a cartridge already in the drive is returned to its home slot
// first. It returns the drive's persistent byte handle and the loaded barcode.
func (m *mtxLoader) load(slot, drive int) (device, string, error) {
	if drive < 0 || drive >= len(m.nodes) {
		return nil, "", fmt.Errorf("no drive %d (the changer has %d)", drive, len(m.nodes))
	}
	st, err := m.status()
	if err != nil {
		return nil, "", err
	}
	if e, ok := st.drives[drive]; ok && e.full {
		if e.srcSlot == slot {
			return m.devs[drive], e.barcode, nil // already loaded
		}
		if err := m.unloadTo(e.srcSlot, drive); err != nil {
			return nil, "", err
		}
	}
	if _, err := m.mtx("load", strconv.Itoa(slot), strconv.Itoa(drive)); err != nil {
		return nil, "", err
	}
	bc := ""
	if st2, err := m.status(); err == nil {
		bc = st2.drives[drive].barcode
	}
	return m.devs[drive], bc, nil
}

// unload returns the cartridge in drive to its home slot. A drive that is already
// empty is a no-op.
func (m *mtxLoader) unload(drive int) error {
	st, err := m.status()
	if err != nil {
		return err
	}
	e, ok := st.drives[drive]
	if !ok || !e.full {
		return nil
	}
	return m.unloadTo(e.srcSlot, drive)
}

// unloadTo returns drive's cartridge to a storage slot. When the home slot is unknown
// (the changer did not report one), it picks any empty non-mailslot slot.
func (m *mtxLoader) unloadTo(slot, drive int) error {
	if slot < 0 {
		s, err := m.firstEmptySlot()
		if err != nil {
			return err
		}
		slot = s
	}
	_, err := m.mtx("unload", strconv.Itoa(slot), strconv.Itoa(drive))
	return err
}

func (m *mtxLoader) firstEmptySlot() (int, error) {
	st, err := m.status()
	if err != nil {
		return 0, err
	}
	for _, s := range st.slots {
		if !s.Full && !s.ImportExport {
			return s.Slot, nil
		}
	}
	return 0, fmt.Errorf("no empty storage slot to unload a drive into")
}

// loaded reports the cartridge currently in drive: the persistent byte handle, its
// barcode, and the slot it came from.
func (m *mtxLoader) loaded(drive int) (device, string, int, bool) {
	if drive < 0 || drive >= len(m.nodes) {
		return nil, "", -1, false
	}
	st, err := m.status()
	if err != nil {
		return nil, "", -1, false
	}
	e, ok := st.drives[drive]
	if !ok || !e.full {
		return nil, "", -1, false
	}
	return m.devs[drive], e.barcode, e.srcSlot, true
}
