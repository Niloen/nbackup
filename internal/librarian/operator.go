// operator.go — the manual-station seam: prompting a human Operator to swap reels when the changer is Manual — ARCHITECTURE.md's "the librarian asks a librarian.Operator (CLI: stdin) to swap".
package librarian

import (
	"fmt"
	"strconv"

	"github.com/Niloen/nbackup/internal/media"
)

// promptSwap asks the operator (via l.op) to pick a reel to load on a single-drive
// station. need is the specific volume label wanted (reads) or "" (writes); expect
// is the volume a write would prefer (the oldest reusable tape) or "".
func (l *Librarian) promptSwap(need, expect string, cause error) (string, bool) {
	room, err := l.room()
	if err != nil {
		return "", false
	}
	var loaded media.VolumeStatus
	if st, ok := l.loaded(); ok {
		loaded = st
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	return l.op.Swap(SwapRequest{Medium: l.medium, Reason: reason, Need: need, Expect: expect, Loaded: loaded, Shelf: room})
}

// room lists the cartridges available to load into the drive — every occupied slot
// not currently in a drive — each by slot id and its barcode, for the operator
// prompt. A real drive has no addressable slots, so the room is empty (the operator
// loads from their own physical shelf and the librarian re-reads the drive).
func (l *Librarian) room() ([]media.VolumeStatus, error) {
	// The scan reads each slot's label so the operator can choose by label or
	// blankness — a file-backed changer can (it loads the slot to read it); a real
	// drive has no addressable slots, so the room is empty and this never runs.
	var out []media.VolumeStatus
	err := l.eachLoadableSlot(0, nil,
		func(s media.SlotStatus, loadErr error, name string, labeled bool, lerr error) bool {
			st := media.VolumeStatus{ID: strconv.Itoa(s.Slot), Barcode: s.Barcode}
			if loadErr == nil {
				if labeled {
					st.Label = name
				} else if lerr == nil {
					st.Blank = true
				}
			}
			out = append(out, st)
			return false
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// insertChoice effects the operator's swap choice: on a file-backed changer it loads
// the chosen slot (the simulated hands); on a real drive (no addressable slots) it is
// a no-op — the human already inserted the cartridge and the caller re-reads it.
func (l *Librarian) insertChoice(choice string) error {
	slots, err := l.changer.Slots()
	if err != nil {
		return err
	}
	if len(slots) == 0 {
		return nil
	}
	slot, err := strconv.Atoi(choice)
	if err != nil {
		return fmt.Errorf("invalid slot %q", choice)
	}
	return l.changer.Load(slot, 0)
}

// swapOutcome is the result of one single-drive swap step.
type swapOutcome int

const (
	swapInserted   swapOutcome = iota // a reel was loaded into the drive
	swapUnattended                    // no operator is attached (l.op == nil)
	swapAborted                       // the operator declined to load a reel
)

// requestSwap runs the single-drive swap step shared by every prompt loop: with no
// operator it reports swapUnattended; otherwise it asks the operator to load a reel for
// the stated need and, on a choice, inserts it (logging the load) and returns the reel.
// It centralizes the nil-op guard, the prompt, the abort, and the Insert; callers map
// swapUnattended/swapAborted to their own actionable message and re-check their own
// writable/mounted condition after swapInserted.
func (l *Librarian) requestSwap(need, expect string, cause error, logf Logf) (reel string, out swapOutcome, err error) {
	if l.op == nil {
		return "", swapUnattended, nil
	}
	reel, ok := l.promptSwap(need, expect, cause)
	if !ok {
		return "", swapAborted, nil
	}
	logf.log("loading %s into the %q drive", reel, l.medium)
	if err := l.insertChoice(reel); err != nil {
		return "", swapInserted, err
	}
	return reel, swapInserted, nil
}
