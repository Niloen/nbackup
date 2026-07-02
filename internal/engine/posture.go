// posture.go is the recoverability audit around the drill: the WORM/immutability
// probe (detected, never configured) and the 3-2-1-1-0 posture checks a drill
// report carries. Pure presentation of engine state — no tier execution here.
package engine

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

// WormResult is the outcome of the WORM/immutability probe against a medium.
type WormResult struct {
	Medium   string
	Tested   bool   // an active write+delete probe ran (apply, address-identified medium)
	Enforced bool   // deletion was refused — the storage enforces immutability
	Detail   string // human-readable explanation
}

// wormProbeRun is the single, fixed probe object the drill reuses every run — so an
// immutable medium accumulates exactly one undeletable probe, not one per drill.
const wormProbeRun = "drill-worm-probe"

// wormProbe tests whether a medium enforces WORM/immutability the way NBackup relies
// on for the 3-2-1-1-0 "1 immutable" digit: it keeps one fixed probe object on the
// medium and, each run, attempts to delete that same object. A refused delete proves
// immutability is enforced (the probe persists — that is the proof); a successful
// delete proves it is not (the probe is recreated next run). Immutability is
// configured operator-side (S3 Object Lock, LTO WORM); NBackup only detects it, never
// sets it. Append-only media (tape) are immutable by construction and are reported
// without writing a probe. The active probe is skipped in --dry-run.
func (e *Engine) wormProbe(medium string, apply bool, now time.Time) WormResult {
	res := WormResult{Medium: medium}
	lib, _, _, err := e.librarianFor(medium)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	if lib.AppendOnly() {
		// Tape and other labeled media are append-only: a file once written cannot be
		// rewritten or individually deleted, so the medium is immutable by construction.
		// Writing a probe would advance/relabel the reel, so report rather than write.
		res.Enforced = true
		res.Detail = "append-only medium: written files are not individually rewritable"
		return res
	}
	vol := lib.Volume()
	if !apply {
		res.Detail = "not probed (dry-run / --worm off); pass --worm (without --dry-run) to test immutability"
		return res
	}
	if err := e.ensureWormProbe(vol, now); err != nil {
		res.Detail = fmt.Sprintf("could not write probe: %v", err)
		return res
	}
	res.Tested = true
	// Delete the probe's file(s) by position — a refused delete proves WORM/Object-Lock.
	files, err := vol.Files()
	if err != nil {
		res.Detail = fmt.Sprintf("could not enumerate probe: %v", err)
		return res
	}
	for _, f := range files {
		if f.Header.Run != wormProbeRun {
			continue
		}
		if err := vol.RemoveFile(f.Pos); err != nil {
			res.Enforced = true
			res.Detail = fmt.Sprintf("delete of probe refused (%v) — immutability ENFORCED", err)
			return res
		}
	}
	res.Detail = "delete of probe succeeded — storage is MUTABLE (no WORM/Object-Lock)"
	return res
}

// ensureWormProbe writes the fixed probe object if it is not already present (an
// unsealed orphan the catalog scanner ignores), so the same object is reused across
// drills.
func (e *Engine) ensureWormProbe(vol media.Volume, now time.Time) error {
	files, err := vol.Files()
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.Header.Run == wormProbeRun {
			return nil // reuse the existing probe
		}
	}
	h := record.Header{Run: wormProbeRun, Kind: record.KindArchive, DLE: "worm-probe", CreatedAt: now}
	fw, err := vol.AppendFile(context.Background(), h)
	if err != nil {
		return err
	}
	_, werr := io.WriteString(fw, "nbackup recovery-drill WORM probe — delete attempts test immutability\n")
	if cerr := fw.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// PostureStatus is a posture check's verdict.
type PostureStatus int

const (
	PostureOK PostureStatus = iota
	PostureWarn
	PostureFail
)

func (s PostureStatus) String() string {
	switch s {
	case PostureOK:
		return "OK"
	case PostureWarn:
		return "WARN"
	case PostureFail:
		return "FAIL"
	default:
		return "?"
	}
}

// PostureCheck is one line of the recoverability audit.
type PostureCheck struct {
	Name   string
	Status PostureStatus
	Detail string
}

// Posture is the 3-2-1-1-0 recoverability audit derived from the catalog, config,
// incremental-state library, capacity, and the WORM probe — the best-practice
// framing around the per-DLE drill outcomes.
type Posture struct {
	Checks    []PostureCheck
	Copies    int // backup copies of the weakest-covered run (the live source is the implicit +1)
	Media     int // distinct media holding copies
	Offsite   bool
	Immutable bool
}

// posture computes the recoverability audit. failures is this run's drill failures.
func (e *Engine) posture(worm WormResult, failures int) Posture {
	runs := e.cat.Runs()
	mediaSet := map[string]bool{}
	minCopies := -1
	for _, s := range runs {
		ps := e.cat.Placements(s.ID)
		if len(ps) == 0 {
			continue
		}
		if minCopies < 0 || len(ps) < minCopies {
			minCopies = len(ps)
		}
		for _, p := range ps {
			mediaSet[p.Medium] = true
		}
	}
	if minCopies < 0 {
		minCopies = 0
	}
	offsite := false
	for m := range mediaSet {
		if m != e.mediumName {
			offsite = true
		}
	}
	p := Posture{Copies: minCopies, Media: len(mediaSet), Offsite: offsite, Immutable: worm.Enforced}
	add := func(name string, st PostureStatus, detail string) {
		p.Checks = append(p.Checks, PostureCheck{Name: name, Status: st, Detail: detail})
	}

	// The live dump source is copy #1 in the canonical 3-2-1 rule (production data
	// + 2 backups = 3), so a run is compliant once it has 2 backup copies. We count
	// catalog placements — the verifiable backup copies; the source is the implicit
	// third NBackup can never drill, so it is never enough on its own.
	switch {
	case minCopies >= 2:
		add("3 copies", PostureOK, fmt.Sprintf("source + %d backup copies (3-2-1 satisfied)", minCopies))
	case minCopies == 1:
		add("3 copies", PostureWarn, "source + 1 backup copy; 3-2-1 wants 2 backups")
	default:
		add("3 copies", PostureFail, "only the live source — no backup copy recorded for some run")
	}
	if len(mediaSet) >= 2 {
		add("2 media", PostureOK, fmt.Sprintf("%d media hold copies", len(mediaSet)))
	} else {
		add("2 media", PostureWarn, "only one medium holds copies")
	}
	if offsite {
		add("1 offsite", PostureOK, "a non-landing medium holds copies")
	} else {
		add("1 offsite", PostureWarn, "no offsite copy (only the landing medium)")
	}
	switch {
	case worm.Enforced:
		add("1 immutable", PostureOK, worm.Detail)
	default:
		add("1 immutable", PostureWarn, worm.Detail)
	}
	if failures == 0 {
		add("0 errors", PostureOK, "no drill failures this run")
	} else {
		add("0 errors", PostureFail, fmt.Sprintf("%d drill failure(s) this run", failures))
	}

	// Extras beyond the 3-2-1-1-0 core.
	add(e.postureKey())
	add(e.postureIncrementalState())
	add(e.postureCapacity())
	return p
}

// postureKey checks that, where encryption is configured, the decryptor binary and
// key reference are present — the lost-key failure mode checksum verification can't
// see. (A real end-to-end key test happens when a structural/chain drill of an
// encrypted archive runs.)
func (e *Engine) postureKey() (string, PostureStatus, string) {
	names := []string{config.DefaultDumpType}
	for n := range e.cfg.DumpTypes {
		names = append(names, n)
	}
	configured := false
	for _, n := range names {
		scheme, opts := e.encryptionFor(n)
		if scheme == "" || scheme == "none" {
			continue
		}
		configured = true
		if err := crypt.Check(scheme, opts); err != nil {
			return "key reachable", PostureWarn, fmt.Sprintf("encryption %q configured but not ready: %v", scheme, err)
		}
	}
	if !configured {
		return "key reachable", PostureOK, "no encryption configured"
	}
	return "key reachable", PostureOK, "encryptor + key reference present"
}

// postureIncrementalState checks the precious, non-derivable incremental-state
// library each archiver owns: a DLE missing the base state its next incremental
// builds on will be forced to a full (recoverable, but a signal). The archiver
// answers whether the base is present (HasBase), so this stays archiver-neutral.
func (e *Engine) postureIncrementalState() (string, PostureStatus, string) {
	hist := e.cat.History()
	missing := 0
	for _, d := range e.cfg.DLEs() {
		name := d.Name()
		st := hist.DLE(name)
		if st.LastFullDate == "" {
			continue // never fulled yet; nothing relied upon
		}
		arch, err := e.archiverFor(d.DumpTypeName(), d.Host)
		if err != nil {
			continue // unresolvable archiver surfaces elsewhere (pre-flight / estimate)
		}
		// The next incremental sits at level L (1 right after a full, else the last
		// level) and builds on the L-1 state; without it the DLE is forced to a full.
		lvl := st.LastLevel()
		if lvl < 1 {
			lvl = 1
		}
		if !arch.HasBase(name, lvl-1) {
			missing++
		}
	}
	if missing == 0 {
		return "incremental state present", PostureOK, "incremental-state library intact"
	}
	return "incremental state present", PostureWarn, fmt.Sprintf("%d DLE(s) missing base incremental state (next backup forces a full)", missing)
}

// postureCapacity reflects whether the landing medium is within its capacity budget.
func (e *Engine) postureCapacity() (string, PostureStatus, string) {
	if e.Capacity() <= 0 {
		return "capacity OK", PostureOK, "unbounded"
	}
	over, pct := e.CapacityStatus(e.StoredBytes())
	if over {
		return "capacity OK", PostureWarn, fmt.Sprintf("over capacity (%.0f%% used); run `nb prune`", pct)
	}
	return "capacity OK", PostureOK, fmt.Sprintf("%.0f%% used", pct)
}

func reelOrEmpty(label string) string {
	if label == "" {
		return "(empty)"
	}
	return fmt.Sprintf("%q", label)
}
