package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// flush.go is the amflush analogue: it drains a crashed run's leftover holding-disk archives to the
// authoritative landing on the next dump. The live drain (a running dump's producer → holding disk →
// landing) lives in package drain; this is the recovery path for what a crash stranded. A
// holding-disk run records each archive's holding placement before flushing it and removes it after,
// so a crash leaves the un-flushed archives recorded on the holding medium in the catalog — Flush
// reads those placements (no medium scan needed) and drains them.

// archivePosFiles lists an archive's file positions for reclamation, the commit footer (the
// marker) first so an interrupted reclaim un-commits before dropping parts.
func archivePosFiles(a record.ArchivePos) []int {
	pos := make([]int, 0, len(a.Parts)+2)
	pos = append(pos, a.Commit.Pos)
	if a.Index != (record.FilePos{}) {
		pos = append(pos, a.Index.Pos)
	}
	for _, pt := range a.Parts {
		pos = append(pos, pt.Pos)
	}
	return pos
}

// Flush drains a crashed run's leftover archives from the holding disk to the landing — the
// amflush analogue. A holding-disk run records each archive's holding placement before
// flushing it and removes it after, so a crash leaves the un-flushed archives recorded on the
// holding medium in the catalog. Flush reads those placements (no medium scan needed), copies
// each archive to the landing, removes the holding placement, reclaims the disk, and seals the
// slot. It is idempotent and a no-op when no holding disk is configured or nothing is staged.
func (e *Engine) Flush(now time.Time, logf Logf) (flushed int, err error) {
	holdings := e.cfg.HoldingMedia()
	if len(holdings) == 0 {
		return 0, nil
	}
	// Resolve each holding disk's volume once, and collect the union of slots staged across them —
	// a single crashed slot may have placements spread over several holding disks. Drain each slot
	// once (one landing session, one seal), copying every holding disk's portion of it.
	holdVols := make(map[string]media.Volume, len(holdings))
	slotSet := map[string]*record.Slot{}
	for _, h := range holdings {
		vol, _, _, err := e.mediumVolume(h)
		if err != nil {
			return 0, err
		}
		holdVols[h] = vol
		for _, s := range e.cat.SlotsOn(h) {
			slotSet[s.ID] = s
		}
	}
	if len(slotSet) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(slotSet))
	for id := range slotSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		s := slotSet[id]
		spec := archiveio.SlotSpec{ID: s.ID, Date: s.Date, Sequence: s.Sequence, Generator: s.Generator, CreatedAt: s.CreatedAt}
		landWT, err := e.prepareWriter(e.mediumName, spec, now, logf)
		if err != nil {
			return flushed, fmt.Errorf("flush %s: open landing %q: %w", s.ID, e.mediumName, err)
		}
		landSession := e.clerk.OpenSlot(landWT.w, e.mediumName)

		for _, holding := range holdings {
			hp, ok := e.placementOn(s.ID, holding)
			if !ok {
				continue
			}
			holdVol := holdVols[holding]
			for _, ap := range hp.Archives {
				ref := clerk.Ref{Slot: s.ID, DLE: ap.DLE, Level: ap.Level}
				dleID := e.DisplayDLE(ap.DLE)
				// A crash between recording the landing placement and reclaiming the holding one
				// leaves an archive on both; in that case just reclaim, don't re-copy.
				if !e.archiveOnLanding(s.ID, ap.DLE, ap.Level) {
					arch, err := e.catalogArchive(s.ID, ap.DLE, ap.Level)
					if err != nil {
						return flushed, fmt.Errorf("flush %s %s: %w", s.ID, dleID, err)
					}
					rc, err := e.clerk.Open(ref, holding)
					if err != nil {
						return flushed, fmt.Errorf("flush %s %s: read holding disk: %w", s.ID, dleID, err)
					}
					// CopyArchive records the landing placement inline.
					if err := landSession.CopyArchive(arch, rc); err != nil {
						rc.Close()
						return flushed, fmt.Errorf("flush %s %s to %q: %w", s.ID, dleID, e.mediumName, err)
					}
					rc.Close()
				}
				for _, pos := range archivePosFiles(ap) {
					if err := holdVol.RemoveFile(pos); err != nil {
						return flushed, fmt.Errorf("flush %s %s: reclaim holding disk: %w", s.ID, dleID, err)
					}
				}
				if _, _, err := e.cat.RemoveArchive(s.ID, holding, ap.DLE); err != nil {
					return flushed, err
				}
				flushed++
				logf.log("flushed %s %s to %q", s.ID, dleID, e.mediumName)
			}
		}
		if err := e.cat.SealSlot(s.ID, now); err != nil {
			return flushed, fmt.Errorf("flush %s: seal: %w", s.ID, err)
		}
	}
	return flushed, nil
}

// catalogArchive returns a holding-disk archive's metadata for a re-copy: the catalogued
// record (checksum, sizes, scheme) plus its member list from the on-medium index.
func (e *Engine) catalogArchive(slotID, dle string, level int) (record.Archive, error) {
	s, err := e.cat.ReadSlot(slotID)
	if err != nil {
		return record.Archive{}, err
	}
	for _, a := range s.Archives {
		if a.DLE == dle && a.Level == level {
			a.Members, _ = e.clerk.Members(clerk.Ref{Slot: slotID, DLE: dle, Level: level})
			return a, nil
		}
	}
	return record.Archive{}, fmt.Errorf("archive %s L%d not in catalog", dle, level)
}

// archiveOnLanding reports whether the slot's landing placement already holds (dle, level).
func (e *Engine) archiveOnLanding(slotID, dle string, level int) bool {
	p, ok := e.placementOn(slotID, e.mediumName)
	if !ok {
		return false
	}
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return true
		}
	}
	return false
}
