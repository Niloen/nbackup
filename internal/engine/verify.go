package engine

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

// Verify is NBackup's atomic verification primitive: it checks
// individual runs/archives against the seal and is **stateless** — it writes
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
//     *restorable stream* and exercises the key + compression end-to-end, while writing
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
	Run    string
	DLE    string
	Level  int
	Medium string
	OK     bool
	Class  drill.Class // ClassNone when OK
	Detail string      // human-readable reason when not OK
}

// RunVerdict aggregates the per-archive verdicts for one run.
type RunVerdict struct {
	Run      string
	Archives []ArchiveVerdict
	OK       bool
}

// VerifyReport is the structured outcome of a Verify call.
type VerifyReport struct {
	Runs     []RunVerdict
	Failures int // runs with at least one failed archive
}

// verifier is NBackup's verification operation: the stateless integrity
// primitive that checks runs/archives against the seal and writes nothing. It depends on a
// narrow slice of the orchestrator — the catalog (run list + metadata), the clerk (byte
// endpoints + recorded member list), the decoder (checksum + structural decode), the
// read-preference placement order, and structural-archiver resolution — not the whole engine,
// so the same path serves `nb verify` and a drill's per-archive check.
type verifier struct {
	cat         *catalog.Catalog                                       // run list + metadata
	clerk       *clerk.Clerk                                           // byte endpoints + member index
	dec         *decoder                                               // checksum + structural decode
	placements  func(runID string) []catalog.Placement                 // copies in read-preference order
	archiverFor func(typeName, host string) (archiver.Archiver, error) // archiver for the structural list
	decryptOpts func(dleName string) crypt.Options                     // per-DLE decrypt key reference (per-dumptype passphrase_file)
}

// newVerifier wires a verifier to the engine's catalog, data path, decoder, and resolution.
func (e *Engine) newVerifier() *verifier {
	return &verifier{
		cat:         e.cat,
		clerk:       e.clerk,
		dec:         e.dec,
		placements:  e.placementsFor,
		archiverFor: e.restoreArchiver,
		decryptOpts: e.decryptOptsFor,
	}
}

// Verify checks the given runs (all cached runs when none are given) under opts,
// returning a structured report. It never writes.
func (e *Engine) Verify(runIDs []string, opts VerifyOptions, logf Logf) (*VerifyReport, error) {
	return e.ver.verify(runIDs, opts, logf)
}

func (v *verifier) verify(runIDs []string, opts VerifyOptions, logf Logf) (*VerifyReport, error) {
	if opts.Checks == 0 {
		opts.Checks = CheckChecksum
	}
	if len(runIDs) == 0 {
		for _, s := range v.cat.Runs() {
			runIDs = append(runIDs, s.ID)
		}
	}
	rep := &VerifyReport{}
	for _, id := range runIDs {
		sv, err := v.verifyRun(id, opts, logf)
		if err != nil {
			// Run metadata unreadable (not in catalog): a failed run verdict rather
			// than aborting the whole pass, so one bad id doesn't mask the rest.
			logf.Log("%s: ERROR %v", id, err)
			rep.Runs = append(rep.Runs, RunVerdict{
				Run: id, OK: false,
				Archives: []ArchiveVerdict{{Run: id, OK: false, Class: drill.ClassMissing, Detail: err.Error()}},
			})
			rep.Failures++
			continue
		}
		rep.Runs = append(rep.Runs, *sv)
		if !sv.OK {
			rep.Failures++
		}
	}
	return rep, nil
}

func (v *verifier) verifyRun(id string, opts VerifyOptions, logf Logf) (*RunVerdict, error) {
	s, err := v.cat.ReadRun(id)
	if err != nil {
		return nil, err
	}
	placements := v.placements(id)
	if opts.Medium != "" {
		placements = placementsOnMedium(placements, opts.Medium)
	}
	sv := &RunVerdict{Run: id, OK: true}
	if len(placements) == 0 {
		where := "any medium"
		if opts.Medium != "" {
			where = fmt.Sprintf("medium %q", opts.Medium)
		}
		logf.Log("%s: NO COPIES on %s", id, where)
		sv.OK = false
		sv.Archives = append(sv.Archives, ArchiveVerdict{
			Run: id, Medium: opts.Medium, OK: false,
			Class: drill.ClassMissing, Detail: "no copy on " + where,
		})
		return sv, nil
	}
	// Track which whole copies passed so a failure can still reassure the operator
	// that an intact copy remains (redundancy is the point of more than one).
	var goodCopies, badCopies, skippedCopies []string
	for _, p := range placements {
		copyOK := true
		archByRef := make(map[clerk.Ref]record.Archive, len(s.Archives))
		refs := make([]clerk.Ref, len(s.Archives))
		for i, a := range s.Archives {
			ref := clerk.Ref{Run: id, DLE: a.DLE, Level: a.Level}
			refs[i] = ref
			archByRef[ref] = a
		}
		// The clerk drives the one-pass read of this copy, calling back per archive; verify
		// every one (never stop early), collecting verdicts.
		verdicts := make(map[clerk.Ref]ArchiveVerdict, len(refs))
		_, err := v.clerk.ReadArchives(refs, p.Medium, func(ref clerk.Ref, open func() (io.ReadCloser, error)) error {
			verdicts[ref] = v.verifyArchive(archByRef[ref], ref, p.Medium, opts, open, logf)
			return nil
		})
		if err != nil {
			// A copy on a medium this config does not define is out of scope, not
			// damaged: skip it (with a note) rather than reporting a false integrity
			// failure. Other errors (a configured medium that won't open) still fail.
			if errors.Is(err, ErrUnknownMedium) {
				logf.Log("%s [%s]: skipped — medium not defined in this config", id, p.Medium)
				skippedCopies = append(skippedCopies, p.Medium)
				continue
			}
			logf.Log("%s [%s]: ERROR %v", id, p.Medium, err)
			sv.OK = false
			badCopies = append(badCopies, p.Medium)
			sv.Archives = append(sv.Archives, ArchiveVerdict{
				Run: id, Medium: p.Medium, OK: false,
				Class: drill.ClassPipeline, Detail: err.Error(),
			})
			continue
		}
		for _, a := range s.Archives {
			v, ok := verdicts[clerk.Ref{Run: id, DLE: a.DLE, Level: a.Level}]
			if !ok {
				logf.Log("%s [%s]: %s L%d POSITION MISSING", id, p.Medium, a.DLEID(), a.Level)
				v = ArchiveVerdict{Run: id, DLE: a.DLE, Level: a.Level, Medium: p.Medium, OK: false,
					Class: drill.ClassMissing, Detail: "archive position missing on this copy"}
			}
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
		logf.Log("%s: SKIPPED — copies only on media not in this config: %s", id, strings.Join(skippedCopies, ", "))
	case sv.OK:
		logf.Log("%s: OK (%d archive(s), %d cop(ies))", id, len(s.Archives), len(goodCopies))
	case len(goodCopies) > 0:
		// Surface that an intact copy remains, and which medium to re-copy from.
		logf.Log("%s: FAILED on %s, but an intact copy remains on %s (re-copy to repair)",
			id, strings.Join(badCopies, ", "), strings.Join(goodCopies, ", "))
	default:
		logf.Log("%s: FAILED on all cop(ies): %s", id, strings.Join(badCopies, ", "))
	}
	return sv, nil
}

// verifyArchive runs the requested checks against one archive, opening its stream via open
// (each check reads it afresh).
func (v *verifier) verifyArchive(a record.Archive, ref clerk.Ref, medium string, opts VerifyOptions, open func() (io.ReadCloser, error), logf Logf) ArchiveVerdict {
	id := ref.Run
	vd := ArchiveVerdict{Run: id, DLE: a.DLE, Level: a.Level, Medium: medium, OK: true}

	if opts.Checks.has(CheckChecksum) {
		rc, serr := open()
		if serr != nil {
			logf.Log("%s [%s]: %s L%d ERROR %v", id, medium, a.DLEID(), a.Level, serr)
			vd.OK, vd.Class, vd.Detail = false, drill.ClassPipeline, serr.Error()
			return vd
		}
		good, err := v.dec.verifyChecksum(rc, a.SHA256)
		if err != nil {
			logf.Log("%s [%s]: %s L%d ERROR %v", id, medium, a.DLEID(), a.Level, err)
			vd.OK, vd.Class, vd.Detail = false, drill.ClassPipeline, err.Error()
			return vd
		}
		if !good {
			logf.Log("%s [%s]: %s L%d CHECKSUM MISMATCH", id, medium, a.DLEID(), a.Level)
			vd.OK, vd.Class, vd.Detail = false, drill.ClassIntegrity, "checksum mismatch vs commit footer"
			return vd
		}
	}
	if opts.Checks.has(CheckStructural) {
		if cls, detail := v.structuralCheck(id, a, open); cls != drill.ClassNone {
			logf.Log("%s [%s]: %s L%d STRUCTURAL %s: %s", id, medium, a.DLEID(), a.Level, cls, detail)
			vd.OK, vd.Class, vd.Detail = false, cls, detail
			return vd
		}
	}
	return vd
}

// structuralCheck streams the archive through the real read pipeline and lists its
// members (`tar -t`), asserting the pipeline completes cleanly and the members match
// the recorded list. It returns ClassNone on success, else the failure class and detail.
func (v *verifier) structuralCheck(id string, a record.Archive, open func() (io.ReadCloser, error)) (drill.Class, string) {
	// Verify is the keyless, server-side integrity primitive: structural decode runs on
	// the server (host ""). The client-side recoverability proof (running the read
	// pipeline on the client for a client-only key) is drill's job — see the design note.
	arch, err := v.archiverFor(a.Archiver, "")
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	rc, err := open()
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	// The clerk reads the parts → decodes (server-side Filters) → lists members (`tar -t`).
	// Any fault — a media read, a decode child, or a not-a-tar List — is a Pipeline failure; a
	// clean stream whose members differ from the seal is an Integrity failure. The decrypt
	// hint keeps a lost-key failure from being mislabeled as corruption.
	members, terr := v.dec.listMembers(rc, a.Compress, a.Encrypt, v.decryptOpts(a.DLE), arch)
	if terr != nil {
		return drill.ClassPipeline, decryptHint(a.Encrypt, terr).Error()
	}
	// The recorded member list (the catalog is member-free) is loaded via the clerk.
	recorded, err := v.clerk.Members(clerk.Ref{Run: id, DLE: a.DLE, Level: a.Level})
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	if diff := membersDiff(recorded, members); diff != "" {
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
		return fmt.Sprintf("member count differs from the recorded index: recorded %d, archive lists %d", len(wc), len(gc))
	}
	for i := range wc {
		if wc[i] != gc[i] {
			return fmt.Sprintf("members differ from the recorded index (e.g. recorded %q vs archive %q)", wc[i], gc[i])
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
