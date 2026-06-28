package drain

import (
	"fmt"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// flush.go is the amflush analogue: it drains a crashed run's leftover holding-disk archives to the
// authoritative landing on the next dump. The live drain (Drainer) handles a running dump; this is
// the recovery path for what a crash stranded. A holding-disk run records each archive's holding
// placement before flushing it and removes it after, so a crash leaves the un-flushed archives
// recorded on the holding medium in the catalog — Flush reads those placements (no medium scan) and
// drains them.

// FlushDeps is what Flush needs from the host: the catalog and data path it reads/reclaims through,
// the landing and holding medium names, and three host-bound seams — resolving a holding disk's
// volume, opening a landing session for a slot, and the DLE display id — plus an optional log.
type FlushDeps struct {
	Cat         *catalog.Catalog
	Clerk       *clerk.Clerk
	Landing     string
	Holdings    []string
	HoldVol     func(name string) (media.Volume, error)
	OpenLanding func(spec archiveio.SlotSpec) (*clerk.Session, error)
	DisplayDLE  func(dle string) string
	Logf        func(format string, args ...any)
}

// Flush drains a crashed run's leftover archives from the holding disks to the landing. It reads the
// stranded holding placements from the catalog (no medium scan), copies each archive to the landing,
// removes the holding placement, reclaims the disk, and seals the slot. It is idempotent and a no-op
// when no holding disk is configured or nothing is staged.
func Flush(d FlushDeps, now time.Time) (flushed int, err error) {
	logf := d.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(d.Holdings) == 0 {
		return 0, nil
	}
	// Resolve each holding disk's volume once, and collect the union of slots staged across them —
	// a single crashed slot may have placements spread over several holding disks. Drain each slot
	// once (one landing session, one seal), copying every holding disk's portion of it.
	holdVols := make(map[string]media.Volume, len(d.Holdings))
	slotSet := map[string]*record.Slot{}
	for _, h := range d.Holdings {
		vol, err := d.HoldVol(h)
		if err != nil {
			return 0, err
		}
		holdVols[h] = vol
		for _, s := range d.Cat.SlotsOn(h) {
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
		landSession, err := d.OpenLanding(spec)
		if err != nil {
			return flushed, fmt.Errorf("flush %s: open landing %q: %w", s.ID, d.Landing, err)
		}

		for _, holding := range d.Holdings {
			hp, ok := placementOn(d.Cat, s.ID, holding)
			if !ok {
				continue
			}
			holdVol := holdVols[holding]
			for _, ap := range hp.Archives {
				ref := clerk.Ref{Slot: s.ID, DLE: ap.DLE, Level: ap.Level}
				dleID := d.DisplayDLE(ap.DLE)
				// A crash between recording the landing placement and reclaiming the holding one
				// leaves an archive on both; in that case just reclaim, don't re-copy.
				if !archiveOnLanding(d.Cat, d.Landing, s.ID, ap.DLE, ap.Level) {
					arch, err := catalogArchive(d.Cat, d.Clerk, s.ID, ap.DLE, ap.Level)
					if err != nil {
						return flushed, fmt.Errorf("flush %s %s: %w", s.ID, dleID, err)
					}
					rc, err := d.Clerk.Open(ref, holding)
					if err != nil {
						return flushed, fmt.Errorf("flush %s %s: read holding disk: %w", s.ID, dleID, err)
					}
					// CopyArchive records the landing placement inline.
					if err := landSession.CopyArchive(arch, rc); err != nil {
						rc.Close()
						return flushed, fmt.Errorf("flush %s %s to %q: %w", s.ID, dleID, d.Landing, err)
					}
					rc.Close()
				}
				for _, pos := range archivePosFiles(ap) {
					if err := holdVol.RemoveFile(pos); err != nil {
						return flushed, fmt.Errorf("flush %s %s: reclaim holding disk: %w", s.ID, dleID, err)
					}
				}
				if _, _, err := d.Cat.RemoveArchive(s.ID, holding, ap.DLE); err != nil {
					return flushed, err
				}
				flushed++
				logf("flushed %s %s to %q", s.ID, dleID, d.Landing)
			}
		}
		if err := d.Cat.SealSlot(s.ID, now); err != nil {
			return flushed, fmt.Errorf("flush %s: seal: %w", s.ID, err)
		}
	}
	return flushed, nil
}

// placementOn returns the slot's placement on the named medium, if any.
func placementOn(cat *catalog.Catalog, slotID, medium string) (catalog.Placement, bool) {
	for _, p := range cat.Placements(slotID) {
		if p.Medium == medium {
			return p, true
		}
	}
	return catalog.Placement{}, false
}

// archiveOnLanding reports whether the slot's landing placement already holds (dle, level).
func archiveOnLanding(cat *catalog.Catalog, landing, slotID, dle string, level int) bool {
	p, ok := placementOn(cat, slotID, landing)
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

// catalogArchive returns a holding-disk archive's metadata for a re-copy: the catalogued record
// (checksum, sizes, scheme) plus its member list from the on-medium index.
func catalogArchive(cat *catalog.Catalog, ck *clerk.Clerk, slotID, dle string, level int) (record.Archive, error) {
	s, err := cat.ReadSlot(slotID)
	if err != nil {
		return record.Archive{}, err
	}
	for _, a := range s.Archives {
		if a.DLE == dle && a.Level == level {
			a.Members, _ = ck.Members(clerk.Ref{Slot: slotID, DLE: dle, Level: level})
			return a, nil
		}
	}
	return record.Archive{}, fmt.Errorf("archive %s L%d not in catalog", dle, level)
}
