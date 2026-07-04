package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/Niloen/nbackup/internal/archiveio"
	"io"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/depot"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/restorer"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

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

// verifier is NBackup's atomic verification operation: it checks individual
// runs/archives against the seal and is **stateless** — it writes nothing, keeps no
// ledger, and makes no selection or scheduling decision. Those belong to the drill
// layer, which consumes the structured VerifyReport this returns. Two checks compose:
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
//
// The verifier depends on a narrow slice of the orchestrator — the catalog (run list +
// metadata), the fs (byte endpoints + recorded member list), the decoder (checksum +
// structural decode), the read-preference placement order, and structural-archiver
// resolution — not the whole engine, so the same path serves `nb verify` and a
// drill's per-archive check.
type verifier struct {
	cat         *catalog.Catalog                                       // run list + metadata
	store       archivefs.ReadStore                                    // byte endpoints + member index (the read face of the archive fs)
	rst         *restorer.Restorer                                     // checksum + structural decode primitives
	placements  func(runID string) []catalog.Placement                 // copies in read-preference order
	archiverFor func(typeName, host string) (archiver.Archiver, error) // archiver for the structural list
	decryptOpts func(dleName string) crypt.Options                     // per-DLE decrypt key reference (per-dumptype passphrase_file)
}

// newVerifier wires a verifier to the engine's catalog, data path, decoder, and resolution.
func (e *Engine) newVerifier() *verifier {
	return &verifier{
		cat:         e.cat,
		store:       e.fs,
		rst:         e.rst,
		placements:  e.placementsFor,
		archiverFor: e.tc.restoreArchiver,
		decryptOpts: e.rst.DecryptOptsFor,
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
	checked := map[archiveio.Ref]bool{} // distinct archives verified on at least one copy
	for _, p := range placements {
		switch copyOK, skipped := v.verifyCopy(s, p, opts, sv, checked, logf); {
		case skipped:
			skippedCopies = append(skippedCopies, p.Medium)
		case copyOK:
			goodCopies = append(goodCopies, p.Medium)
		default:
			badCopies = append(badCopies, p.Medium)
		}
	}
	switch {
	case sv.OK && len(goodCopies) == 0 && len(skippedCopies) > 0:
		// Every copy lives on a medium this config does not define — nothing was
		// actually checked, so say so rather than reporting a misleading "OK".
		logf.Log("%s: SKIPPED — copies only on media not in this config: %s", id, strings.Join(skippedCopies, ", "))
	case sv.OK:
		// checked counts the distinct archives verified on at least one copy — with a
		// medium pin (or a partially-pruned copy) that may be fewer than the run holds.
		logf.Log("%s: OK (%d archive(s), %d cop(ies))", id, len(checked), len(goodCopies))
	case len(goodCopies) > 0:
		// Surface that an intact copy remains, and which medium to re-copy from.
		logf.Log("%s: FAILED on %s, but an intact copy remains on %s (re-copy to repair)",
			id, strings.Join(badCopies, ", "), strings.Join(goodCopies, ", "))
	default:
		logf.Log("%s: FAILED on all cop(ies): %s", id, strings.Join(badCopies, ", "))
	}
	return sv, nil
}

// verifyCopy checks one placement — the run's copy on one medium — appending its
// per-archive verdicts to sv (and marking each verified archive in checked). It
// reports whether the whole copy passed, or that it was skipped because its medium is
// not defined in this config (out of scope, not damaged).
func (v *verifier) verifyCopy(s *catalog.Run, p catalog.Placement, opts VerifyOptions, sv *RunVerdict, checked map[archiveio.Ref]bool, logf Logf) (copyOK, skipped bool) {
	id := sv.Run
	copyOK = true
	// A copy is archive-granular: a per-archive prune may have reclaimed some of
	// the run's archives from this medium (the placement then simply doesn't hold
	// them). Judge each copy against what it records, not the run's whole content
	// — an absent archive here is by design, not a missing position; its surviving
	// copies on other media are verified on their own placements.
	expected := make([]record.Archive, 0, len(s.Archives))
	for _, a := range s.Archives {
		if p.Holds(a.DLE, a.Level) {
			expected = append(expected, a)
		}
	}
	archByRef := make(map[archiveio.Ref]record.Archive, len(expected))
	refs := make([]archiveio.Ref, len(expected))
	for i, a := range expected {
		ref := archiveio.Ref{Run: id, DLE: a.DLE, Level: a.Level}
		refs[i] = ref
		archByRef[ref] = a
	}
	// The fs drives the one-pass read of this copy, calling back per archive; verify
	// every one (never stop early), collecting verdicts.
	verdicts := make(map[archiveio.Ref]ArchiveVerdict, len(refs))
	_, err := v.store.OpenArchives(refs, p.Medium, func(ref archiveio.Ref, open func() (io.ReadCloser, error)) error {
		verdicts[ref] = v.verifyArchive(archByRef[ref], ref, p.Medium, opts, open, logf)
		return nil
	})
	if err != nil {
		// A copy on a medium this config does not define is out of scope, not
		// damaged: skip it (with a note) rather than reporting a false integrity
		// failure. Other errors (a configured medium that won't open) still fail.
		if errors.Is(err, depot.ErrUnknownMedium) {
			logf.Log("%s [%s]: skipped — medium not defined in this config", id, p.Medium)
			return true, true
		}
		logf.Log("%s [%s]: ERROR %v", id, p.Medium, err)
		sv.OK = false
		sv.Archives = append(sv.Archives, ArchiveVerdict{
			Run: id, Medium: p.Medium, OK: false,
			Class: drill.ClassPipeline, Detail: err.Error(),
		})
		return false, false
	}
	for _, a := range expected {
		ref := archiveio.Ref{Run: id, DLE: a.DLE, Level: a.Level}
		vd, ok := verdicts[ref]
		if !ok {
			logf.Log("%s [%s]: %s L%d POSITION MISSING", id, p.Medium, a.DLEID(), a.Level)
			vd = ArchiveVerdict{Run: id, DLE: a.DLE, Level: a.Level, Medium: p.Medium, OK: false,
				Class: drill.ClassMissing, Detail: "archive position missing on this copy"}
		}
		checked[ref] = true
		sv.Archives = append(sv.Archives, vd)
		if !vd.OK {
			sv.OK = false
			copyOK = false
		}
	}
	return copyOK, false
}

// verifyArchive runs the requested checks against one archive, opening its stream via open.
// A checksum-only or structural-only check reads once. A deep verify (both) also reads
// once: the structural decode drains the whole raw payload, so hashing it inline through a
// tee yields the checksum for free — no second pass off the medium, halving the egress a
// deep offsite drill would otherwise pay.
func (v *verifier) verifyArchive(a record.Archive, ref archiveio.Ref, medium string, opts VerifyOptions, open func() (io.ReadCloser, error), logf Logf) ArchiveVerdict {
	id := ref.Run
	vd := ArchiveVerdict{Run: id, DLE: a.DLE, Level: a.Level, Medium: medium, OK: true}

	if opts.Checks.has(CheckChecksum) && opts.Checks.has(CheckStructural) {
		return v.verifyDeep(a, ref, medium, open, logf)
	}
	if opts.Checks.has(CheckChecksum) {
		rc, serr := open()
		if serr != nil {
			logf.Log("%s [%s]: %s L%d ERROR %v", id, medium, a.DLEID(), a.Level, serr)
			vd.OK, vd.Class, vd.Detail = false, drill.ClassPipeline, serr.Error()
			return vd
		}
		good, err := v.rst.VerifyChecksum(rc, a.SHA256)
		if err != nil {
			logf.Log("%s [%s]: %s L%d ERROR %v", id, medium, a.DLEID(), a.Level, err)
			// An inline seal mismatch on the stream is corruption, not a read fault.
			vd.OK, vd.Class, vd.Detail = false, classifyReadErr(err), err.Error()
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

// verifyDeep runs the checksum + structural checks in a single read. It opens the raw
// stream once and tees it into a hash while the structural decode consumes it; because
// the decode drains the whole payload (a filter reads to EOF; `tar -t` reads the
// record-aligned tar in full), the tee sees every stored byte, so the accumulated hash is
// the payload's true checksum. Structural runs first: a decode/list fault (a broken key or
// scheme, a not-a-tar) is reported as the structural failure it is; only once the stream
// decoded cleanly is the hash compared, catching a corruption that still parsed as an
// integrity fault.
func (v *verifier) verifyDeep(a record.Archive, ref archiveio.Ref, medium string, open func() (io.ReadCloser, error), logf Logf) ArchiveVerdict {
	id := ref.Run
	vd := ArchiveVerdict{Run: id, DLE: a.DLE, Level: a.Level, Medium: medium, OK: true}

	rc, serr := open()
	if serr != nil {
		logf.Log("%s [%s]: %s L%d ERROR %v", id, medium, a.DLEID(), a.Level, serr)
		vd.OK, vd.Class, vd.Detail = false, drill.ClassPipeline, serr.Error()
		return vd
	}
	h := sha256.New()
	tee := &teeReadCloser{r: io.TeeReader(rc, h), c: rc}
	if cls, detail := v.structuralCheck(id, a, func() (io.ReadCloser, error) { return tee, nil }); cls != drill.ClassNone {
		logf.Log("%s [%s]: %s L%d STRUCTURAL %s: %s", id, medium, a.DLEID(), a.Level, cls, detail)
		vd.OK, vd.Class, vd.Detail = false, cls, detail
		return vd
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		logf.Log("%s [%s]: %s L%d CHECKSUM MISMATCH", id, medium, a.DLEID(), a.Level)
		vd.OK, vd.Class, vd.Detail = false, drill.ClassIntegrity, "checksum mismatch vs commit footer"
		return vd
	}
	return vd
}

// teeReadCloser adapts an io.Reader (a tee) plus the underlying closer into an
// io.ReadCloser, so the decode pipeline reads bytes that also flow into the deep-verify
// hash while Close still releases the real stream.
type teeReadCloser struct {
	r io.Reader
	c io.Closer
}

func (t *teeReadCloser) Read(p []byte) (int, error) { return t.r.Read(p) }
func (t *teeReadCloser) Close() error               { return t.c.Close() }

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
	// The fs reads the parts → decodes (server-side Filters, per the recorded shape:
	// an atomic archive runs the per-atom decrypt loop) → lists members (`tar -t`).
	// Any fault — a media read, a decode child, or a not-a-tar List — is a Pipeline failure; a
	// clean stream whose members differ from the seal is an Integrity failure. The decrypt
	// hint keeps a lost-key failure from being mislabeled as corruption.
	members, terr := v.rst.ListMembers(rc, a, v.decryptOpts(a.DLE), arch)
	if terr != nil {
		return classifyReadErr(terr), restorer.DecryptHint(a.Encrypt, terr).Error()
	}
	// A zero-change incremental records no member index by design (it writes just the
	// payload and commit; recover reads the base full's index for it — see README). Its
	// payload is still a valid tar carrying GNU tar's directory census (./, ./sub/), so
	// there is nothing to compare members against — the pipeline decoding cleanly and
	// `tar -t` completing above IS the whole structural proof. Comparing that census to
	// the empty recorded index would falsely flag a healthy archive as corrupt. An
	// archive writes an index only when it has members, so FileCount==0 is exactly the
	// no-recorded-index case.
	if a.FileCount == 0 {
		return drill.ClassNone, ""
	}
	// The recorded member list (the catalog is member-free) is loaded via the archivefs.
	recorded, err := v.store.Members(archiveio.Ref{Run: id, DLE: a.DLE, Level: a.Level})
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	// No loadable index for an archive that has members: a pre-shapes archive whose
	// on-medium index is the old format (undecodable by design — greenfield, no
	// migration). The clean decode + `tar -t` above already proved the stream
	// restorable, so degrade to that proof rather than inventing a member-count
	// mismatch — a healthy old archive must never be reported as corrupt.
	if len(recorded) == 0 {
		return drill.ClassNone, ""
	}
	if diff := membersDiff(recorded, members); diff != "" {
		return drill.ClassIntegrity, diff
	}
	return drill.ClassNone, ""
}

// classifyReadErr maps a failed read-side operation to its drill class: an inline part-seal
// mismatch (archiveio.ErrSealMismatch, caught on the stream itself) is corruption —
// ClassIntegrity — while any other fault (media read, decode child, not-a-tar) stays the
// pipeline class. errors.Is sees through the xfer role wrapping.
func classifyReadErr(err error) drill.Class {
	if errors.Is(err, archiveio.ErrSealMismatch) {
		return drill.ClassIntegrity
	}
	return drill.ClassPipeline
}

// membersDiff compares the seal's member list to a freshly listed one as sorted
// sets — paths AND stream offsets — returning "" when they match or a short human
// description of the first difference otherwise. Offsets are compared only when both
// sides report one (>= 0): an archiver that cannot report offsets still gets the
// name-set check.
func membersDiff(want, got []record.Member) string {
	wc := append([]record.Member(nil), want...)
	gc := append([]record.Member(nil), got...)
	byPathOff := func(ms []record.Member) func(i, j int) bool {
		return func(i, j int) bool {
			if ms[i].Path != ms[j].Path {
				return ms[i].Path < ms[j].Path
			}
			return ms[i].Off < ms[j].Off
		}
	}
	sort.Slice(wc, byPathOff(wc))
	sort.Slice(gc, byPathOff(gc))
	if len(wc) != len(gc) {
		return fmt.Sprintf("member count differs from the recorded index: recorded %d, archive lists %d", len(wc), len(gc))
	}
	for i := range wc {
		if wc[i].Path != gc[i].Path {
			return fmt.Sprintf("members differ from the recorded index (e.g. recorded %q vs archive %q)", wc[i].Path, gc[i].Path)
		}
		if wc[i].Off >= 0 && gc[i].Off >= 0 && wc[i].Off != gc[i].Off {
			return fmt.Sprintf("member %q moved in the stream: recorded offset %d, archive lists %d", wc[i].Path, wc[i].Off, gc[i].Off)
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
