package engine

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/slotio"
)

// Verify is NBackup's atomic verification primitive (Amanda's amverify): it checks
// individual slots/archives against the seal and is **stateless** — it writes
// nothing, keeps no ledger, and makes no selection or scheduling decision. Those
// belong to the drill layer, which consumes the structured VerifyReport this
// returns. Two checks compose:
//
//   - checksum (CheckChecksum, the default): re-hash the stored payload and compare
//     to the seal's SHA256. Keyless — it reads the ciphertext as it lies on the
//     volume, the same bytes a copy/sync carries.
//   - structural (CheckStructural, `nb verify --deep`): stream the archive through
//     the real read pipeline — decrypt → decompress → `tar -t` (LIST, not extract) —
//     and assert both that the pipeline completes cleanly and that the listed members
//     match the seal's recorded member list. It proves the bytes are a valid
//     *restorable stream* and exercises the key + codec end-to-end, while writing
//     nothing.
//
// VerifyChecks is a bitmask so a deep verify can request both. VerifyOptions.Medium,
// when set, restricts verification to that one medium's copy (an offsite drill);
// empty verifies every copy, so a corrupt copy is caught even when another is fine.

// VerifyChecks selects which atomic checks a verify performs.
type VerifyChecks int

const (
	// CheckChecksum re-hashes the payload against the seal (today's default).
	CheckChecksum VerifyChecks = 1 << iota
	// CheckStructural streams through decrypt→decompress→tar -t and compares members.
	CheckStructural
)

func (c VerifyChecks) has(x VerifyChecks) bool { return c&x != 0 }

// VerifyOptions controls a verify pass.
type VerifyOptions struct {
	Checks VerifyChecks // zero is treated as CheckChecksum
	Medium string       // "" = every placement; else only the copy on this medium
}

// ArchiveVerdict is the machine-readable result of verifying one archive on one
// placement — the unit the drill layer consumes.
type ArchiveVerdict struct {
	Slot   string
	DLE    string
	Level  int
	Medium string
	OK     bool
	Class  drill.Class // ClassNone when OK
	Detail string      // human-readable reason when not OK
}

// SlotVerdict aggregates the per-archive verdicts for one slot.
type SlotVerdict struct {
	Slot     string
	Archives []ArchiveVerdict
	OK       bool
}

// VerifyReport is the structured outcome of a Verify call.
type VerifyReport struct {
	Slots    []SlotVerdict
	Failures int // slots with at least one failed archive
}

// Verify checks the given slots (all cached slots when none are given) under opts,
// returning a structured report. It never writes.
func (e *Engine) Verify(slotIDs []string, opts VerifyOptions, logf Logf) (*VerifyReport, error) {
	if opts.Checks == 0 {
		opts.Checks = CheckChecksum
	}
	if len(slotIDs) == 0 {
		for _, s := range e.cat.Slots() {
			slotIDs = append(slotIDs, s.ID)
		}
	}
	rep := &VerifyReport{}
	for _, id := range slotIDs {
		sv, err := e.verifySlot(id, opts, logf)
		if err != nil {
			// Slot metadata unreadable (not in catalog): a failed slot verdict rather
			// than aborting the whole pass, so one bad id doesn't mask the rest.
			logf.log("%s: ERROR %v", id, err)
			rep.Slots = append(rep.Slots, SlotVerdict{
				Slot: id, OK: false,
				Archives: []ArchiveVerdict{{Slot: id, OK: false, Class: drill.ClassMissing, Detail: err.Error()}},
			})
			rep.Failures++
			continue
		}
		rep.Slots = append(rep.Slots, *sv)
		if !sv.OK {
			rep.Failures++
		}
	}
	return rep, nil
}

func (e *Engine) verifySlot(id string, opts VerifyOptions, logf Logf) (*SlotVerdict, error) {
	s, err := e.cat.ReadSlot(id)
	if err != nil {
		return nil, err
	}
	placements := e.placementsFor(id)
	if opts.Medium != "" {
		placements = placementsOnMedium(placements, opts.Medium)
	}
	sv := &SlotVerdict{Slot: id, OK: true}
	if len(placements) == 0 {
		where := "any medium"
		if opts.Medium != "" {
			where = fmt.Sprintf("medium %q", opts.Medium)
		}
		logf.log("%s: NO COPIES on %s", id, where)
		sv.OK = false
		sv.Archives = append(sv.Archives, ArchiveVerdict{
			Slot: id, Medium: opts.Medium, OK: false,
			Class: drill.ClassMissing, Detail: "no copy on " + where,
		})
		return sv, nil
	}
	// Track which whole copies passed so a failure can still reassure the operator
	// that an intact copy remains (redundancy is the point of more than one).
	var goodCopies, badCopies, skippedCopies []string
	for _, p := range placements {
		copyOK := true
		opener, err := e.aio.PartOpener(p.Medium)
		if err != nil {
			// A copy on a medium this config does not define is out of scope, not
			// damaged: skip it (with a note) rather than reporting a false integrity
			// failure. Other errors (a configured medium that won't open) still fail.
			if errors.Is(err, ErrUnknownMedium) {
				logf.log("%s [%s]: skipped — medium not defined in this config", id, p.Medium)
				skippedCopies = append(skippedCopies, p.Medium)
				continue
			}
			logf.log("%s [%s]: ERROR %v", id, p.Medium, err)
			sv.OK = false
			badCopies = append(badCopies, p.Medium)
			sv.Archives = append(sv.Archives, ArchiveVerdict{
				Slot: id, Medium: p.Medium, OK: false,
				Class: drill.ClassPipeline, Detail: err.Error(),
			})
			continue
		}
		for _, a := range s.Archives {
			v := e.verifyArchive(id, a, p, opts, opener, logf)
			sv.Archives = append(sv.Archives, v)
			if !v.OK {
				sv.OK = false
				copyOK = false
			}
		}
		if copyOK {
			goodCopies = append(goodCopies, p.Medium)
		} else {
			badCopies = append(badCopies, p.Medium)
		}
	}
	switch {
	case sv.OK && len(goodCopies) == 0 && len(skippedCopies) > 0:
		// Every copy lives on a medium this config does not define — nothing was
		// actually checked, so say so rather than reporting a misleading "OK".
		logf.log("%s: SKIPPED — copies only on media not in this config: %s", id, strings.Join(skippedCopies, ", "))
	case sv.OK:
		logf.log("%s: OK (%d archive(s), %d cop(ies))", id, len(s.Archives), len(goodCopies))
	case len(goodCopies) > 0:
		// Surface that an intact copy remains, and which medium to re-copy from.
		logf.log("%s: FAILED on %s, but an intact copy remains on %s (re-copy to repair)",
			id, strings.Join(badCopies, ", "), strings.Join(goodCopies, ", "))
	default:
		logf.log("%s: FAILED on all cop(ies): %s", id, strings.Join(badCopies, ", "))
	}
	return sv, nil
}

// verifyArchive runs the requested checks against one archive on one placement.
func (e *Engine) verifyArchive(id string, a record.Archive, p catalog.Placement, opts VerifyOptions, opener slotio.PartOpener, logf Logf) ArchiveVerdict {
	v := ArchiveVerdict{Slot: id, DLE: a.DLE, Level: a.Level, Medium: p.Medium, OK: true}
	parts, found := p.Parts(a.DLE, a.Level)
	if !found {
		logf.log("%s [%s]: %s L%d POSITION MISSING", id, p.Medium, a.DLEID(), a.Level)
		v.OK, v.Class, v.Detail = false, drill.ClassMissing, "archive position missing on this copy"
		return v
	}
	want := slotio.Expect{Slot: id, DLE: a.DLE, Level: a.Level}

	if opts.Checks.has(CheckChecksum) {
		good, err := e.aio.VerifyParts(parts, want, a.SHA256, opener)
		if err != nil {
			logf.log("%s [%s]: %s L%d ERROR %v", id, p.Medium, a.DLEID(), a.Level, err)
			v.OK, v.Class, v.Detail = false, drill.ClassPipeline, err.Error()
			return v
		}
		if !good {
			logf.log("%s [%s]: %s L%d CHECKSUM MISMATCH", id, p.Medium, a.DLEID(), a.Level)
			v.OK, v.Class, v.Detail = false, drill.ClassIntegrity, "checksum mismatch vs seal"
			return v
		}
	}
	if opts.Checks.has(CheckStructural) {
		if cls, detail := e.structuralCheck(a, parts, want, opener); cls != drill.ClassNone {
			logf.log("%s [%s]: %s L%d STRUCTURAL %s: %s", id, p.Medium, a.DLEID(), a.Level, cls, detail)
			v.OK, v.Class, v.Detail = false, cls, detail
			return v
		}
	}
	return v
}

// structuralCheck streams the archive through the real read pipeline and lists its
// members (`tar -t`), asserting the pipeline completes cleanly and the members match
// the seal. It returns ClassNone on success, else the failure class and detail. It
// writes nothing.
func (e *Engine) structuralCheck(a record.Archive, parts []record.FilePos, want slotio.Expect, opener slotio.PartOpener) (drill.Class, string) {
	// Verify is the keyless, server-side integrity primitive: structural decode runs on
	// the server (host ""). The client-side recoverability proof (running the read
	// pipeline on the client for a client-only key) is drill's job — see the design note.
	arch, err := e.restoreArchiver(a.Archiver, "")
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	raw, err := e.aio.OpenParts(parts, want, opener)
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	// Decode server-side (host ""), then list. Keeping decode (the parts pipeline) separate
	// from tar lets a decode fault surface on the pipeline reap and a tar fault on List — the
	// split the classification rests on.
	pipe, err := e.aio.DecodePipeline(a.Encrypt, a.Compress, config.EncryptConfig{}, "")
	if err != nil {
		raw.Close()
		return drill.ClassPipeline, err.Error()
	}
	decoded, wait, derr := programs.RunGrouped(raw, pipe.Reverse()...)
	if derr != nil {
		raw.Close()
		return drill.ClassPipeline, derr.Error()
	}
	members, lerr := arch.List(decoded)
	// Drain any bytes tar left unread (it stops at the archive's EOF marker) so the
	// decrypt/decompress children see EOF and exit cleanly, then reap — this is what makes
	// "the pipeline completes cleanly" a reliable signal rather than a spurious broken-pipe.
	_, _ = io.Copy(io.Discard, decoded)
	decoded.Close()
	werr := wait()        // the decrypt/decompress children — the real cause of a decode fault
	rawErr := raw.Close() // a media-read fault on the ciphertext parts
	// tar sits last in the pipe, so an upstream failure (a missing key, wrong
	// passphrase, codec drift) reaches tar only as truncated input and reports a
	// generic "not a tar archive"; the real cause surfaces on the pipeline reap. Join and
	// add the decrypt hint — the same surfacing the recover path uses — so a
	// lost-key failure is not mislabeled as corruption.
	if lerr != nil || werr != nil || rawErr != nil {
		return drill.ClassPipeline, decryptHint(a.Encrypt, joinPipelineErr(lerr, errors.Join(werr, rawErr))).Error()
	}
	if diff := membersDiff(a.Members, members); diff != "" {
		return drill.ClassIntegrity, diff
	}
	return drill.ClassNone, ""
}

// membersDiff compares the seal's member list to a freshly listed one as sorted
// sets, returning "" when they match or a short human description of the first
// difference otherwise.
func membersDiff(want, got []string) string {
	wc := append([]string(nil), want...)
	gc := append([]string(nil), got...)
	sort.Strings(wc)
	sort.Strings(gc)
	if len(wc) != len(gc) {
		return fmt.Sprintf("member count differs from seal: sealed %d, archive lists %d", len(wc), len(gc))
	}
	for i := range wc {
		if wc[i] != gc[i] {
			return fmt.Sprintf("members differ from seal (e.g. sealed %q vs archive %q)", wc[i], gc[i])
		}
	}
	return ""
}

// placementsOnMedium keeps only the copy on the named medium (for offsite drills /
// medium-scoped verify).
func placementsOnMedium(ps []catalog.Placement, medium string) []catalog.Placement {
	out := ps[:0:0]
	for _, p := range ps {
		if p.Medium == medium {
			out = append(out, p)
		}
	}
	return out
}
