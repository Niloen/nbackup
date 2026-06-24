package engine

import (
	"fmt"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/slot"
)

// SyncSelection bounds which landing slots a sync considers. The zero value
// selects every slot on the landing medium.
type SyncSelection struct {
	Last  int       // 0 = all; else only the N most recent landing slots
	Since time.Time // zero = no lower bound; else only slots created at/after this
}

// SyncItem is one slot in a sync's backlog: a copy that the target is missing.
type SyncItem struct {
	SlotID   string
	Archives int
	Bytes    int64 // compressed size on the volume
	Copied   bool  // set true once a real run copies it
}

// SyncReport is the backlog of one sync target (and, after a real run, what was
// copied). It is what the CLI renders for both dry-run and apply.
type SyncReport struct {
	From  string
	To    string
	Items []SyncItem // slots on From not yet on To, oldest-first, after selection
}

// Bytes is the total size of the backlog.
func (r *SyncReport) Bytes() int64 {
	var n int64
	for _, it := range r.Items {
		n += it.Bytes
	}
	return n
}

// Copied counts the items actually copied (after a real run).
func (r *SyncReport) Copied() int {
	n := 0
	for _, it := range r.Items {
		if it.Copied {
			n++
		}
	}
	return n
}

// SyncTo mirrors a source medium's sealed slots onto target: every slot with a
// copy on the source but not yet recorded on target, oldest-first. Oldest first
// means an interrupted sync makes contiguous, replayable progress and a slot's
// full lands before the incrementals that build on it. The source defaults to the
// landing medium when from is ""; any other medium is allowed (e.g. tape -> disk).
//
// With apply==false it only computes the backlog (a dry run). With apply==true it
// copies each slot via CopySlot — the same label-verified, placement-recording
// path as `nb copy` — stopping at the first error and returning the report so far
// alongside it (a full or offline target won't fix itself by continuing). Each
// slot is atomic, so re-running resumes where an interrupted sync left off. With
// force==true already-present slots are re-copied (CopySlot --force).
func (e *Engine) SyncTo(from, target string, sel SyncSelection, apply, force bool, logf Logf) (*SyncReport, error) {
	if from == "" {
		from = e.mediumName
	}
	if from == target {
		return nil, fmt.Errorf("sync source and target are the same medium %q", target)
	}
	if _, ok := e.cfg.Media[from]; !ok {
		return nil, fmt.Errorf("unknown source medium %q", from)
	}
	if _, ok := e.cfg.Media[target]; !ok {
		return nil, fmt.Errorf("unknown medium %q", target)
	}

	report := &SyncReport{From: from, To: target}
	for _, s := range applySelection(e.cat.SlotsOn(from), sel) {
		if !force && e.placedOn(s.ID, target) {
			continue // idempotent: already on the target
		}
		report.Items = append(report.Items, SyncItem{
			SlotID:   s.ID,
			Archives: len(s.Archives),
			Bytes:    s.TotalBytes,
		})
	}
	if !apply {
		return report, nil
	}
	for i := range report.Items {
		it := &report.Items[i]
		if err := e.CopySlot(it.SlotID, from, target, force, logf); err != nil {
			return report, fmt.Errorf("sync %s -> %q: %w", it.SlotID, target, err)
		}
		it.Copied = true
	}
	return report, nil
}

// SyncRules returns the configured replication rules, for the CLI to run when
// `nb sync` is invoked without an explicit --to.
func (e *Engine) SyncRules() []config.SyncRule { return e.cfg.Sync }

// placedOn reports whether a slot already has a copy recorded on the medium.
func (e *Engine) placedOn(slotID, medium string) bool {
	for _, p := range e.cat.Placements(slotID) {
		if p.Medium == medium {
			return true
		}
	}
	return false
}

// applySelection narrows landing slots (oldest-first) to the selection window.
func applySelection(slots []*slot.Slot, sel SyncSelection) []*slot.Slot {
	if !sel.Since.IsZero() {
		kept := slots[:0:0]
		for _, s := range slots {
			// Filter on the slot's logical date (the day it backs up), not its
			// physical CreatedAt seal time — otherwise back-dated or imported slots,
			// whose CreatedAt is "now", all slip past any --since bound.
			d, _ := slot.ParseDateField(s.Date)
			if !d.Before(sel.Since) {
				kept = append(kept, s)
			}
		}
		slots = kept
	}
	if sel.Last > 0 && len(slots) > sel.Last {
		slots = slots[len(slots)-sel.Last:] // most recent N (slots are oldest-first)
	}
	return slots
}
