package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/crypt"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/format"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/restore"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/slotio"
)

// Drill is NBackup's recovery-drill orchestration — the recoverability ("0 errors")
// proof that checksum verification alone cannot give. It selects a risk-biased
// subset of DLEs (package drill), exercises each end-to-end at the requested tier
// against a chosen source copy, records the outcome in the inspectable ledger, runs
// a WORM/immutability probe, and produces a 3-2-1-1-0 posture audit. It is dry-run
// unless DrillOptions.Apply is set.
//
// It supports two run modes. Attended (the default, when the CLI has set an
// operator) may prompt to swap a tape for a target. Unattended (Options.Unattended,
// for cron) attaches no operator and pre-filters out any target whose source copy
// would need a human to load a tape, marking it Skipped (a non-failing SLO warning)
// rather than blocking or failing — so a nightly `nb drill --unattended`
// never needs a person.

// DrillOptions controls a drill run.
type DrillOptions struct {
	AsOf       string        // point-in-time to drill (YYYY-MM-DD); "" = today
	Window     time.Duration // each DLE should be drilled within this window
	Sample     int           // max DLEs to drill this run (<=0 = every due DLE)
	Medium     string        // source medium to read from; "" = the landing medium
	Tier       drill.Tier    // how deeply to exercise each target
	Worm       bool          // run the WORM/immutability probe (apply only)
	Unattended bool          // cron mode: never prompt; skip swap-needing targets
	Apply      bool          // false = dry-run (no media writes, no ledger update)
	Now        time.Time     // reference time; zero = time.Now().UTC()
}

// DrillResult is one target's outcome (or, in a dry-run, what would run).
type DrillResult struct {
	DLE    string
	SlotID string
	AsOf   string
	Medium string
	Tier   drill.Tier
	OK     bool
	Class  drill.Class
	Detail string
	Bytes  int64 // chain payload bytes — the egress/cost of drilling this target
}

// DrillReport is the structured outcome of a drill, rendered by the CLI and the
// basis of its exit code (non-zero on any real failure).
type DrillReport struct {
	AsOf          string
	Window        time.Duration
	Medium        string
	Tier          drill.Tier
	Apply         bool
	Unattended    bool
	Targets       []DrillResult
	Ledger        *drill.Ledger
	Posture       Posture
	Worm          WormResult
	Failures      int      // outcomes that count as failures (Class.IsFailure)
	Skipped       int      // targets skipped (needs operator, unattended)
	ForecastBytes int64    // total egress (bytes) of the selected targets
	Priced        bool     // the drill medium has a cost model (cloud); false for local media
	Provider      string   // the drill medium's rate table (e.g. "aws-s3")
	ForecastCost  float64  // total egress cost ($) of reading the selected targets off the medium
	NeverDrilled  []string // configured DLEs never drilled (cold spots)
	Overdue       int      // DLEs not covered within the window
}

// SLOMet reports the drill SLO: zero failures this run. Coverage gaps (never-drilled
// / overdue DLEs) are warnings that rotation closes over successive runs, not SLO
// failures — so a sampled nightly cron stays green while it works through the fleet.
func (r *DrillReport) SLOMet() bool { return r.Failures == 0 }

// Drill runs (or, without Apply, previews) a recovery drill.
func (e *Engine) Drill(opts DrillOptions, logf Logf) (*DrillReport, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.AsOf == "" {
		opts.AsOf = format.DateString(opts.Now)
	}
	medium := opts.Medium
	if medium == "" {
		medium = e.mediumName
	}
	if _, ok := e.cfg.Media[medium]; !ok {
		return nil, fmt.Errorf("unknown drill medium %q", medium)
	}

	dles := e.DLENames()
	slots := e.cat.Slots()
	ledger, err := drill.Load(e.cfg.WorkdirPath())
	if err != nil {
		return nil, err
	}
	targets := drill.Select(dles, slots, opts.AsOf, ledger, opts.Window, opts.Sample, opts.Now)

	rep := &DrillReport{
		AsOf: opts.AsOf, Window: opts.Window, Medium: medium, Tier: opts.Tier,
		Apply: opts.Apply, Unattended: opts.Unattended, Ledger: ledger,
	}
	rep.NeverDrilled, rep.Overdue = coverage(dles, ledger, opts.Window, opts.Now)

	// Price the egress of reading the selected chains off the drill medium — the
	// honest cost of an offsite drill (an encrypted+compressed archive is all-or-
	// nothing, so a structural/chain drill spends the full bytes).
	if cm := e.costModelFor(medium); cm.Priced() {
		var refs []archiveRef
		for _, t := range targets {
			for _, s := range t.Steps {
				refs = append(refs, archiveRef{s.SlotID, s.DLE, s.Level})
			}
		}
		est := e.estimateRead(refs, medium)
		rep.Priced = true
		rep.Provider = est.Provider
		rep.ForecastCost = est.Cost
	}

	if !opts.Apply {
		// Dry-run: show what would run and its forecast cost; touch no media, write
		// no ledger. The WORM probe is detect-only here (no probe object written).
		for _, t := range targets {
			b := e.chainBytes(t.Steps)
			rep.Targets = append(rep.Targets, DrillResult{
				DLE: t.DLE, SlotID: t.SlotID, AsOf: t.AsOf, Medium: medium, Tier: opts.Tier, Bytes: b,
			})
			rep.ForecastBytes += b
		}
		rep.Worm = e.wormProbe(medium, false, opts.Now)
		rep.Posture = e.posture(rep.Worm, 0)
		return rep, nil
	}

	for _, t := range targets {
		res := e.drillTarget(t, medium, opts, logf)
		rep.Targets = append(rep.Targets, res)
		rep.ForecastBytes += res.Bytes
		switch {
		case res.Class == drill.ClassSkipped:
			rep.Skipped++
			// A skip did not drill the DLE, so leave its ledger record untouched — it
			// stays "due" for the next (attended) run.
		default:
			if res.Class.IsFailure() {
				rep.Failures++
			}
			ledger.Update(drill.Record{
				DLE: t.DLE, LastDrill: opts.Now, Tier: opts.Tier.String(), Medium: medium,
				AsOf: t.AsOf, SlotID: t.SlotID, OK: res.OK,
				Class: failureToken(res), Detail: res.Detail,
			})
			if err := ledger.Save(e.cfg.WorkdirPath()); err != nil {
				return rep, fmt.Errorf("save drill ledger: %w", err)
			}
		}
	}

	if opts.Worm {
		rep.Worm = e.wormProbe(medium, true, opts.Now)
	} else {
		rep.Worm = e.wormProbe(medium, false, opts.Now)
	}
	rep.Posture = e.posture(rep.Worm, rep.Failures)
	// Recompute coverage against the freshly updated ledger.
	rep.NeverDrilled, rep.Overdue = coverage(dles, ledger, opts.Window, opts.Now)
	return rep, nil
}

// drillTarget exercises one target at the requested tier and classifies the outcome.
// In unattended mode it first skips any target whose source copy a human would have
// to load.
func (e *Engine) drillTarget(t drill.Target, medium string, opts DrillOptions, logf Logf) DrillResult {
	res := DrillResult{DLE: t.DLE, SlotID: t.SlotID, AsOf: t.AsOf, Medium: medium, Tier: opts.Tier, Bytes: e.chainBytes(t.Steps)}
	if opts.Unattended {
		if ok, reason := e.unattendedReachable(medium, t.Steps); !ok {
			res.Class, res.Detail = drill.ClassSkipped, reason
			logf.log("drill %s: SKIPPED — %s", t.DLE, reason)
			return res
		}
	}

	var cls drill.Class
	var detail string
	switch opts.Tier {
	case drill.TierChecksum:
		cls, detail = e.drillVerify(t, medium, CheckChecksum)
	case drill.TierStructural:
		cls, detail = e.drillVerify(t, medium, CheckChecksum|CheckStructural)
	case drill.TierChain:
		cls, detail = e.drillChain(t, medium, logf)
	case drill.TierStock:
		cls, detail = e.drillStock(t, medium, logf)
	default:
		cls, detail = drill.ClassPipeline, fmt.Sprintf("unknown tier %v", opts.Tier)
	}
	res.Class, res.Detail = cls, detail
	res.OK = cls == drill.ClassNone
	if res.OK {
		logf.log("drill %s [%s] as of %s on %q: OK (%s)", t.DLE, opts.Tier, t.AsOf, medium, sizeutil.FormatBytes(res.Bytes))
	} else {
		logf.log("drill %s [%s] as of %s on %q: FAIL [%s] %s", t.DLE, opts.Tier, t.AsOf, medium, cls, detail)
	}
	return res
}

// drillVerify exercises a target's chain archives with the verify primitive on the
// chosen medium (checksum, or checksum+structural). It stops at the first fault.
func (e *Engine) drillVerify(t drill.Target, medium string, checks VerifyChecks) (drill.Class, string) {
	lib, _, _, err := e.librarianFor(medium)
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	opener := e.partOpener(lib, medium)
	for _, step := range t.Steps {
		s, err := e.cat.ReadSlot(step.SlotID)
		if err != nil {
			return drill.ClassMissing, err.Error()
		}
		a, ok := findArchive(s, step.DLE, step.Level)
		if !ok {
			return drill.ClassMissing, fmt.Sprintf("%s %s L%d missing from seal", step.SlotID, step.DLE, step.Level)
		}
		ps := placementsOnMedium(e.placementsFor(step.SlotID), medium)
		if len(ps) == 0 {
			return drill.ClassMissing, fmt.Sprintf("no copy of %s on medium %q", step.SlotID, medium)
		}
		v := e.verifyArchive(step.SlotID, a, ps[0], VerifyOptions{Checks: checks, Medium: medium}, opener, nil)
		if !v.OK {
			return v.Class, v.Detail
		}
	}
	return drill.ClassNone, ""
}

// drillChain performs a real point-in-time chain restore of the DLE into a scratch
// dir, then discards it — the strong proof. It uses the deletion-faithful
// listed-incremental path (NOT the recover/member path), reading from the chosen
// medium. A decrypt/decompress fault is Pipeline; a tar composition fault is Chain.
func (e *Engine) drillChain(t drill.Target, medium string, logf Logf) (drill.Class, string) {
	dir, err := os.MkdirTemp("", "nbackup-drill-chain-*")
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	defer os.RemoveAll(dir)
	for _, step := range t.Steps {
		// The chain restores to a server-side scratch dir (decode + extract server-side),
		// so the archiver runs locally (host ""). Driving the recoverability proof on the
		// client for a client-only key is the documented follow-on — see the design note.
		arch, err := e.restoreArchiver(step.Archiver, "")
		if err != nil {
			return drill.ClassPipeline, err.Error()
		}
		rc, err := e.openArchiveFrom(step.SlotID, step.DLE, step.Level, step.Codec, step.Encrypt, medium)
		if err != nil {
			return classifyOpenErr(err), err.Error()
		}
		logf.log("drill-restoring %s %s L%d", step.SlotID, step.DLE, step.Level)
		rerr := arch.Restore(rc, dir, nil)
		cerr := rc.Close()
		if cerr != nil {
			// A decrypt/decompress child exited non-zero — a lost/wrong key or a codec
			// drift, surfaced only on Close (the read side saw a truncated stream).
			return drill.ClassPipeline, cerr.Error()
		}
		if rerr != nil {
			// The bytes decoded but tar could not apply the listed-incremental chain.
			return drill.ClassChain, rerr.Error()
		}
	}
	return drill.ClassNone, ""
}

// drillStock validates the documented "recovery never requires NBackup" one-liner:
// it fetches each chain archive's raw payload (NBackup only moves bytes) and decodes
// it with the stock tools (gpg/zstd/gzip/tar) via `sh -c`, restoring into a scratch
// dir it then discards.
func (e *Engine) drillStock(t drill.Target, medium string, logf Logf) (drill.Class, string) {
	dir, err := os.MkdirTemp("", "nbackup-drill-stock-*")
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	defer os.RemoveAll(dir)
	for _, step := range t.Steps {
		if cls, detail := e.stockExtractStep(step, dir, medium, logf); cls != drill.ClassNone {
			return cls, detail
		}
	}
	return drill.ClassNone, ""
}

func (e *Engine) stockExtractStep(step restore.Step, dest, medium string, logf Logf) (drill.Class, string) {
	ps := placementsOnMedium(e.placementsFor(step.SlotID), medium)
	if len(ps) == 0 {
		return drill.ClassMissing, fmt.Sprintf("no copy of %s on medium %q", step.SlotID, medium)
	}
	parts, ok := ps[0].Parts(step.DLE, step.Level)
	if !ok {
		return drill.ClassMissing, fmt.Sprintf("%s %s L%d position missing on %q", step.SlotID, step.DLE, step.Level, medium)
	}
	lib, _, _, err := e.librarianFor(medium)
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	// Fetch the raw (still-encrypted/compressed) payload to a temp file. NBackup is
	// used only to move bytes off the medium (unavoidable for tape/cloud); the decode
	// is done entirely by the documented stock tools below.
	raw := e.reader.OpenRawParts(parts, slotio.Expect{Slot: step.SlotID, DLE: step.DLE, Level: step.Level}, e.partOpener(lib, medium))
	tmp, err := os.CreateTemp("", "nbackup-drill-raw-*")
	if err != nil {
		raw.Close()
		return drill.ClassPipeline, err.Error()
	}
	_, copyErr := io.Copy(tmp, raw)
	raw.Close()
	tmp.Close()
	defer os.Remove(tmp.Name())
	if copyErr != nil {
		return classifyOpenErr(copyErr), copyErr.Error()
	}

	script, err := stockPipeline(step.Encrypt, step.Codec)
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	in, err := os.Open(tmp.Name())
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	defer in.Close()
	// `sh -c <script> sh <dest>` makes $1 == dest inside the pipeline.
	cmd := exec.Command("/bin/sh", "-c", script, "sh", dest)
	cmd.Stdin = in
	var stderr strings.Builder
	cmd.Stderr = &stderr
	logf.log("stock-restoring %s %s L%d via documented one-liner: %s", step.SlotID, step.DLE, step.Level, script)
	if err := cmd.Run(); err != nil {
		return drill.ClassPipeline, fmt.Sprintf("stock one-liner failed: %v\n%s", err, strings.TrimSpace(stderr.String()))
	}
	return drill.ClassNone, ""
}

// stockPipeline builds the documented restore one-liner for an archive's
// (encrypt, codec): decrypt (gpg) then decompress (zstd/gzip) then untar with
// listed-incremental, reading stdin and extracting into "$1". It is deliberately the
// README's stock command, not NBackup's own filter/crypt/method code.
func stockPipeline(encrypt, codec string) (string, error) {
	var stages []string
	switch encrypt {
	case "", "none":
	case "gpg":
		stages = append(stages, "gpg -d --batch --yes --no-tty")
	default:
		return "", fmt.Errorf("stock drill: unknown encryption scheme %q", encrypt)
	}
	switch codec {
	case "", "none":
	case "gzip":
		stages = append(stages, "gzip -dc")
	case "zstd":
		stages = append(stages, "zstd -dc")
	default:
		return "", fmt.Errorf("stock drill: unknown codec %q", codec)
	}
	stages = append(stages, `tar --extract --listed-incremental=/dev/null --numeric-owner -C "$1" -f -`)
	return strings.Join(stages, " | "), nil
}

// unattendedReachable reports whether a target's source copy can be read without a
// human loading a tape. Address-identified media (disk/cloud) and robotic libraries
// (which auto-mount) are always reachable; a single-drive station is reachable only
// when every needed volume is already the one loaded in its drive.
func (e *Engine) unattendedReachable(medium string, steps []restore.Step) (bool, string) {
	view, err := e.ChangerView(medium)
	if err != nil {
		return true, "" // address-identified: nothing to mount
	}
	if view.Library {
		return true, "" // robotic library mounts the right bay itself
	}
	loaded := ""
	if view.DriveOK {
		loaded = view.Drive.Label
	}
	for _, v := range e.chainLabels(steps, medium) {
		if v != loaded {
			return false, fmt.Sprintf("needs tape %q in the %q drive (a human-only swap); loaded: %s", v, medium, reelOrEmpty(loaded))
		}
	}
	return true, ""
}

// chainLabels is the distinct set of volume labels a chain's copies occupy on the
// medium — the tapes an unattended drill would need mounted.
func (e *Engine) chainLabels(steps []restore.Step, medium string) []string {
	seen := map[string]bool{}
	var out []string
	for _, step := range steps {
		for _, p := range placementsOnMedium(e.placementsFor(step.SlotID), medium) {
			for _, v := range p.Labels() {
				if !seen[v] {
					seen[v] = true
					out = append(out, v)
				}
			}
		}
	}
	return out
}

// chainBytes sums a chain's stored (compressed) payload bytes — the egress a drill of
// this target would read off the medium, the basis of the offsite cost forecast.
func (e *Engine) chainBytes(steps []restore.Step) int64 {
	var n int64
	for _, step := range steps {
		s, err := e.cat.ReadSlot(step.SlotID)
		if err != nil {
			continue
		}
		if a, ok := findArchive(s, step.DLE, step.Level); ok {
			n += a.Compressed
		}
	}
	return n
}

func findArchive(s *format.Slot, dle string, level int) (format.Archive, bool) {
	for _, a := range s.Archives {
		if a.DLE == dle && a.Level == level {
			return a, true
		}
	}
	return format.Archive{}, false
}

// classifyOpenErr maps an archive-open failure to a class: a missing copy or an
// unavailable volume is ClassMissing, anything else (decrypt setup, read error) is
// ClassPipeline. It matches the producers' sentinel errors (errMissingCopy from the
// catalog read path, librarian.ErrVolumeUnavailable from the mount path) via errors.Is,
// so reclassification does not silently follow a reworded message.
func classifyOpenErr(err error) drill.Class {
	if errors.Is(err, errMissingCopy) || errors.Is(err, librarian.ErrVolumeUnavailable) {
		return drill.ClassMissing
	}
	return drill.ClassPipeline
}

// failureToken is the ledger's class token for a result: empty when it passed.
func failureToken(r DrillResult) string {
	if r.OK {
		return ""
	}
	return r.Class.String()
}

// coverage reports the configured DLEs that have never been drilled and how many are
// not covered within the window. It delegates to drill.Coverage — the pure
// computation lives in the leaf with the ledger, so `nb report` reuses it too.
func coverage(dles []string, ledger *drill.Ledger, window time.Duration, now time.Time) (never []string, overdue int) {
	return ledger.Coverage(dles, window, now)
}

// WormResult is the outcome of the WORM/immutability probe against a medium.
type WormResult struct {
	Medium   string
	Tested   bool   // an active write+delete probe ran (apply, address-identified medium)
	Enforced bool   // deletion was refused — the storage enforces immutability
	Detail   string // human-readable explanation
}

// wormProbeSlot is the single, fixed probe object the drill reuses every run — so an
// immutable medium accumulates exactly one undeletable probe, not one per drill.
const wormProbeSlot = "drill-worm-probe"

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
	if err := vol.RemoveSlot(wormProbeSlot); err != nil {
		res.Enforced = true
		res.Detail = fmt.Sprintf("delete of probe refused (%v) — immutability ENFORCED", err)
		return res
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
		if f.Header.Slot == wormProbeSlot {
			return nil // reuse the existing probe
		}
	}
	h := format.Header{Slot: wormProbeSlot, Kind: format.KindArchive, DLE: "worm-probe", CreatedAt: now}
	_, err = vol.AppendFile(h, func(w io.Writer) error {
		_, werr := io.WriteString(w, "nbackup recovery-drill WORM probe — delete attempts test immutability\n")
		return werr
	})
	return err
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
	Copies    int // backup copies of the weakest-covered slot (the live source is the implicit +1)
	Media     int // distinct media holding copies
	Offsite   bool
	Immutable bool
}

// posture computes the recoverability audit. failures is this run's drill failures.
func (e *Engine) posture(worm WormResult, failures int) Posture {
	slots := e.cat.Slots()
	mediaSet := map[string]bool{}
	minCopies := -1
	for _, s := range slots {
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
	// + 2 backups = 3), so a slot is compliant once it has 2 backup copies. We count
	// catalog placements — the verifiable backup copies; the source is the implicit
	// third NBackup can never drill, so it is never enough on its own.
	switch {
	case minCopies >= 2:
		add("3 copies", PostureOK, fmt.Sprintf("source + %d backup copies (3-2-1 satisfied)", minCopies))
	case minCopies == 1:
		add("3 copies", PostureWarn, "source + 1 backup copy; 3-2-1 wants 2 backups")
	default:
		add("3 copies", PostureFail, "only the live source — no backup copy recorded for some slot")
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
		if scheme == "" {
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
