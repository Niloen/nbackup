package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/Niloen/nbackup/internal/archiveio"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/accounting"
	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/depot"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/restorer"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/xfer"
)

// The driller is NBackup's recovery-drill orchestration — the recoverability ("0
// errors") proof that checksum verification alone cannot give. It selects a
// risk-biased subset of DLEs (package drill), exercises each end-to-end at the
// requested tier against a chosen source copy, records the outcome in the
// inspectable ledger, runs a WORM/immutability probe, and produces a 3-2-1-1-0
// posture audit. It is dry-run unless DrillOptions.Apply is set.
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
	DLE        string // internal slug (ledger key)
	DLEDisplay string // host:path identity for display
	RunID      string
	AsOf       string
	Medium     string
	Tier       drill.Tier
	OK         bool
	Class      drill.Class
	Detail     string
	Bytes      int64 // chain payload bytes — the egress/cost of drilling this target
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

// driller is the drill operation. Like the verifier and copier it depends on a
// narrow slice of the orchestrator — the catalog and ledger workdir, the fs (the
// read data path), the verifier (per-archive checks), the restorer (the chain tier
// rehearses the real restore path), the accountant (egress pricing + the posture's
// capacity digit), the depot (source-medium librarians for the WORM probe and the
// unattended reachability check), and the toolchain (the posture's key/state
// probes) — not the whole engine.
type driller struct {
	cfg           *config.Config
	cat           *catalog.Catalog
	tc            *toolchain
	dep           *depot.Depot
	fs            *archivefs.FS
	ver           *verifier
	rst           *restorer.Restorer
	acct          *accounting.Accountant
	dles          *dleDirectory
	placementsFor func(runID string) []catalog.Placement // copies in read-preference order
}

// newDriller wires a driller to the engine's lanes and resolution services.
func (e *Engine) newDriller() *driller {
	return &driller{
		cfg:           e.cfg,
		cat:           e.cat,
		tc:            e.tc,
		dep:           e.dep,
		fs:            e.fs,
		ver:           e.ver,
		rst:           e.rst,
		acct:          e.acct,
		dles:          e.dles,
		placementsFor: e.placementsFor,
	}
}

// Drill runs (or, without Apply, previews) a recovery drill; see driller.
func (e *Engine) Drill(opts DrillOptions, logf Logf) (*DrillReport, error) {
	return e.drl.Drill(opts, logf)
}

// Drill runs (or, without Apply, previews) a recovery drill.
func (d *driller) Drill(opts DrillOptions, logf Logf) (*DrillReport, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.AsOf == "" {
		opts.AsOf = record.DateString(opts.Now)
	}
	medium := opts.Medium
	if medium == "" {
		medium = d.dep.LandingName()
	}
	if _, ok := d.cfg.Media[medium]; !ok {
		return nil, fmt.Errorf("unknown drill medium %q %s", medium, mediaNamesHint(d.cfg))
	}

	dles := d.dles.names()
	ledger, err := drill.Load(d.cfg.WorkdirPath())
	if err != nil {
		return nil, err
	}
	targets := drill.Select(dles, d.cat.Archives(), opts.AsOf, ledger, opts.Window, opts.Sample, opts.Now)

	rep := &DrillReport{
		AsOf: opts.AsOf, Window: opts.Window, Medium: medium, Tier: opts.Tier,
		Apply: opts.Apply, Unattended: opts.Unattended, Ledger: ledger,
	}
	rep.NeverDrilled, rep.Overdue = ledger.Coverage(dles, opts.Window, opts.Now)

	// Price the egress of reading the selected chains off the drill medium — the
	// honest cost of an offsite drill (an encrypted+compressed archive is all-or-
	// nothing, so a structural/chain drill spends the full bytes; the sample tier
	// reads one sealed part per archive, so it is priced on those parts alone).
	if cm := d.acct.CostModelFor(medium); cm.Priced() {
		rep.Priced = true
		rep.Provider = cm.Provider
		if opts.Tier == drill.TierSample {
			var bytes, gets int64
			for _, t := range targets {
				rec, _ := ledger.Get(t.DLE)
				if choices, ok := d.samplePlan(t.Steps, medium, rec.Drills); ok {
					for _, c := range choices {
						bytes += c.size
						gets++
					}
				} else { // sealless copy: sampling falls back to the full checksum read
					bytes += d.chainBytes(t.Steps)
					gets += int64(len(t.Steps))
				}
			}
			rep.ForecastCost = cm.ReadCost(bytes, gets)
		} else {
			var refs []archiveio.Ref
			for _, t := range targets {
				for _, s := range t.Steps {
					refs = append(refs, archiveio.Ref{Run: s.RunID, DLE: s.DLE, Level: s.Level})
				}
			}
			est := d.acct.EstimateRead(refs, medium)
			rep.Provider = est.Provider
			rep.ForecastCost = est.Cost
		}
	}

	if !opts.Apply {
		// Dry-run: show what would run and its forecast cost; touch no media, write
		// no ledger. The WORM probe is detect-only here (no probe object written).
		for _, t := range targets {
			rec, _ := ledger.Get(t.DLE)
			b := d.targetBytes(t, medium, opts.Tier, rec.Drills)
			rep.Targets = append(rep.Targets, DrillResult{
				DLE: t.DLE, DLEDisplay: d.dles.display(t.DLE), RunID: t.RunID, AsOf: t.AsOf, Medium: medium, Tier: opts.Tier, Bytes: b,
			})
			rep.ForecastBytes += b
		}
		rep.Worm = d.wormProbe(medium, false, opts.Now)
		rep.Posture = d.posture(rep.Worm, 0)
		return rep, nil
	}

	for _, t := range targets {
		prev, _ := ledger.Get(t.DLE)
		res := d.drillTarget(t, medium, prev.Drills, opts, logf)
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
				AsOf: t.AsOf, RunID: t.RunID, OK: res.OK,
				Class: failureToken(res), Detail: res.Detail,
				Drills: prev.Drills + 1, // advances the sample tier's part rotation
			})
			if err := ledger.Save(d.cfg.WorkdirPath()); err != nil {
				return rep, fmt.Errorf("save drill ledger: %w", err)
			}
		}
	}

	rep.Worm = d.wormProbe(medium, opts.Worm, opts.Now)
	rep.Posture = d.posture(rep.Worm, rep.Failures)
	// Recompute coverage against the freshly updated ledger.
	rep.NeverDrilled, rep.Overdue = ledger.Coverage(dles, opts.Window, opts.Now)
	return rep, nil
}

// drillTarget exercises one target at the requested tier and classifies the outcome.
// In unattended mode it first skips any target whose source copy a human would have
// to load. drills is the DLE's ledger drill count — the sample tier's part rotation.
func (d *driller) drillTarget(t drill.Target, medium string, drills int, opts DrillOptions, logf Logf) DrillResult {
	res := DrillResult{DLE: t.DLE, DLEDisplay: d.dles.display(t.DLE), RunID: t.RunID, AsOf: t.AsOf, Medium: medium, Tier: opts.Tier, Bytes: d.targetBytes(t, medium, opts.Tier, drills)}
	if opts.Unattended {
		if ok, reason := d.unattendedReachable(medium, t.Steps); !ok {
			res.Class, res.Detail = drill.ClassSkipped, reason
			logf.Log("drill %s: SKIPPED — %s", res.DLEDisplay, reason)
			return res
		}
	}

	var cls drill.Class
	var detail string
	switch opts.Tier {
	case drill.TierSample:
		cls, detail = d.drillSample(t, medium, drills, logf)
	case drill.TierChecksum:
		cls, detail = d.drillVerify(t, medium, CheckChecksum)
	case drill.TierStructural:
		cls, detail = d.drillVerify(t, medium, CheckChecksum|CheckStructural)
	case drill.TierChain:
		cls, detail = d.drillChain(t, medium, logf)
	case drill.TierStock:
		cls, detail = d.drillStock(t, medium, logf)
	default:
		cls, detail = drill.ClassPipeline, fmt.Sprintf("unknown tier %v", opts.Tier)
	}
	res.Class, res.Detail = cls, detail
	res.OK = cls == drill.ClassNone
	if res.OK {
		logf.Log("drill %s [%s] as of %s on %q: OK (%s)", res.DLEDisplay, opts.Tier, t.AsOf, medium, sizeutil.FormatBytes(res.Bytes))
	} else {
		logf.Log("drill %s [%s] as of %s on %q: FAIL [%s] %s", res.DLEDisplay, opts.Tier, t.AsOf, medium, cls, detail)
	}
	return res
}

// drillVerify exercises a target's chain archives with the verify primitive on the
// chosen medium (checksum, or checksum+structural). It stops at the first fault.
func (d *driller) drillVerify(t drill.Target, medium string, checks VerifyChecks) (drill.Class, string) {
	refs := make([]archiveio.Ref, 0, len(t.Steps))
	archByRef := make(map[archiveio.Ref]record.Archive, len(t.Steps))
	for _, step := range t.Steps {
		s, err := d.cat.ReadRun(step.RunID)
		if err != nil {
			return drill.ClassMissing, err.Error()
		}
		a, ok := s.Archive(step.DLE, step.Level)
		if !ok {
			return drill.ClassMissing, fmt.Sprintf("%s %s L%d missing from the run's commit footers", step.RunID, step.DLE, step.Level)
		}
		ref := archiveio.Ref{Run: step.RunID, DLE: step.DLE, Level: step.Level}
		refs = append(refs, ref)
		archByRef[ref] = a
	}
	// Drive the one-pass read of the chain; stop at the first failing archive (a drill fails
	// whole). A failing verdict is carried out via a sentinel error.
	var bad ArchiveVerdict
	errStop := errors.New("drill: archive failed")
	missing, err := d.fs.OpenArchives(refs, medium, func(ref archiveio.Ref, open func() (io.ReadCloser, error)) error {
		v := d.ver.verifyArchive(archByRef[ref], ref, medium, VerifyOptions{Checks: checks, Medium: medium}, open, nil)
		if !v.OK {
			bad = v
			return errStop
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errStop) {
			return bad.Class, bad.Detail
		}
		return drill.ClassPipeline, err.Error()
	}
	if len(missing) > 0 {
		return drill.ClassMissing, fmt.Sprintf("no copy on medium %q", medium)
	}
	return drill.ClassNone, ""
}

// drillSample verifies ONE part of each chain archive on the medium against its
// recorded per-part seal — the bounded-egress tier: a part's worth of bytes per
// archive off the medium instead of the whole chain. rot (the DLE's ledger drill
// count) picks the part, so successive drills rotate through an archive's parts and
// checksum coverage accumulates across runs. A copy that records no seals falls back
// to the full checksum read for the whole target, so the tier never silently proves
// less than it claims.
func (d *driller) drillSample(t drill.Target, medium string, rot int, logf Logf) (drill.Class, string) {
	choices, ok := d.samplePlan(t.Steps, medium, rot)
	if !ok {
		logf.Log("drill %s: copy on %q records no part seals — sampling falls back to a full checksum read", d.dles.display(t.DLE), medium)
		return d.drillVerify(t, medium, CheckChecksum)
	}
	for _, c := range choices {
		ok, err := d.fs.VerifyPart(c.ref, medium, c.idx)
		if err != nil {
			return classifyOpenErr(err), err.Error()
		}
		if !ok {
			return drill.ClassIntegrity, fmt.Sprintf("%s %s L%d part %d of %d: checksum mismatch vs its recorded seal",
				c.ref.Run, c.ref.DLE, c.ref.Level, c.idx+1, c.parts)
		}
	}
	return drill.ClassNone, ""
}

// sampleChoice is one archive's sampled part: which of its parts, and the egress
// (the seal's size) reading it costs.
type sampleChoice struct {
	ref   archiveio.Ref
	idx   int
	parts int
	size  int64
}

// samplePlan picks the part each chain archive's sample would read on the medium
// (rot % parts) and its egress. ok=false when any archive's copy there records no
// aligned seals or is absent — the caller falls back to the full checksum tier,
// which locates and reports the precise fault.
func (d *driller) samplePlan(steps []recovery.Step, medium string, rot int) ([]sampleChoice, bool) {
	choices := make([]sampleChoice, 0, len(steps))
	for _, step := range steps {
		found := false
		for _, p := range placementsOnMedium(d.placementsFor(step.RunID), medium) {
			pa, ok := p.Placed(step.DLE, step.Level)
			if !ok || len(pa.Seals) == 0 || len(pa.Seals) != len(pa.Parts) {
				continue
			}
			idx := rot % len(pa.Parts)
			choices = append(choices, sampleChoice{
				ref:   archiveio.Ref{Run: step.RunID, DLE: step.DLE, Level: step.Level},
				idx:   idx,
				parts: len(pa.Parts),
				size:  pa.Seals[idx].Size,
			})
			found = true
			break
		}
		if !found {
			return nil, false
		}
	}
	return choices, true
}

// targetBytes is the egress a drill of the target reads off the medium at the tier:
// the sampled parts' sizes for the sample tier (falling back to the full chain when
// sampling would), the whole chain's stored bytes otherwise.
func (d *driller) targetBytes(t drill.Target, medium string, tier drill.Tier, rot int) int64 {
	if tier == drill.TierSample {
		if choices, ok := d.samplePlan(t.Steps, medium, rot); ok {
			var n int64
			for _, c := range choices {
				n += c.size
			}
			return n
		}
	}
	return d.chainBytes(t.Steps)
}

// drillChain performs a real point-in-time chain restore of the DLE into a scratch
// dir, then discards it — the strong proof. It calls the same restorer.Extract that
// `nb recover --all` runs (deletion-faithful listed-incremental, one-pass read off
// the chosen medium): the drill rehearses the actual restore path, not a copy of
// it. The outcome is classified from the returned error alone — the restorer's
// documented contract. Driving the proof on the client for a client-only key is
// the documented follow-on — see the design note.
func (d *driller) drillChain(t drill.Target, medium string, logf Logf) (drill.Class, string) {
	dir, err := os.MkdirTemp("", "nbackup-drill-chain-*")
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	defer os.RemoveAll(dir)
	if err := d.rst.Extract(restorer.Request{DLE: t.DLE, RunID: t.RunID, Dest: dir, Medium: medium}, logf); err != nil {
		return classifyRestoreErr(err), err.Error()
	}
	return drill.ClassNone, ""
}

// classifyRestoreErr maps a failed restorer.Extract to a drill class via the
// restorer's error contract: a missing copy/volume (sentinels, via errors.Is) is
// Missing; a role-tagged Sink or Commit fault (tar could not compose the stream,
// or its exit status was bad) is Chain; anything else — an unreadable part or a
// decrypt/decompress child — is Pipeline.
func classifyRestoreErr(err error) drill.Class {
	if errors.Is(err, archivefs.ErrMissingCopy) || errors.Is(err, librarian.ErrVolumeUnavailable) {
		return drill.ClassMissing
	}
	var xe *xfer.Error
	if errors.As(err, &xe) && (xe.Role == xfer.RoleSink || xe.Role == xfer.RoleCommit) {
		return drill.ClassChain
	}
	return drill.ClassPipeline
}

// drillStock validates the documented "recovery never requires NBackup" one-liner:
// it fetches each chain archive's raw payload (NBackup only moves bytes) and decodes
// it with the stock tools (gpg/zstd/gzip/tar) via `sh -c`, restoring into a scratch
// dir it then discards.
func (d *driller) drillStock(t drill.Target, medium string, logf Logf) (drill.Class, string) {
	dir, err := os.MkdirTemp("", "nbackup-drill-stock-*")
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	defer os.RemoveAll(dir)
	for _, step := range t.Steps {
		if cls, detail := d.stockExtractStep(step, dir, medium, logf); cls != drill.ClassNone {
			return cls, detail
		}
	}
	return drill.ClassNone, ""
}

func (d *driller) stockExtractStep(step recovery.Step, dest, medium string, logf Logf) (drill.Class, string) {
	// Fetch the raw (still-encrypted/compressed) payload to a temp file as a transfer whose
	// sink is just the file — NBackup is used only to move bytes off the medium (unavoidable
	// for tape/cloud); the decode is done entirely by the documented stock tools below.
	tmp, err := os.CreateTemp("", "nbackup-drill-raw-*")
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	defer os.Remove(tmp.Name())
	src, err := d.fs.OpenArchive(archiveio.Ref{Run: step.RunID, DLE: step.DLE, Level: step.Level}, medium)
	if err != nil {
		tmp.Close()
		return classifyOpenErr(err), err.Error()
	}
	_, terr := xfer.Transfer(context.Background(), xfer.Reader(src), xfer.NewFilters(), xfer.Writer(tmp))
	tmp.Close()
	if terr != nil {
		return classifyOpenErr(terr), terr.Error()
	}

	script, err := stockPipeline(step.Encrypt, step.Compress, d.rst.DecryptOptsFor(step.DLE).PassphraseFile)
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
	logf.Log("stock-restoring %s %s L%d via documented one-liner: %s", step.RunID, step.DLE, step.Level, script)
	if err := cmd.Run(); err != nil {
		return drill.ClassPipeline, fmt.Sprintf("stock one-liner failed: %v\n%s", err, strings.TrimSpace(stderr.String()))
	}
	return drill.ClassNone, ""
}

// shSingleQuote wraps s in single quotes for safe interpolation into an `sh -c` script,
// escaping any embedded single quote. Used for the passphrase-file path in the stock one-liner.
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// stockPipeline builds the documented restore one-liner for an archive's
// (encrypt, compress): decrypt (gpg) then decompress (zstd/gzip) then untar with
// listed-incremental, reading stdin and extracting into "$1". It is deliberately the
// README's stock command, not NBackup's own filter/crypt/method code.
func stockPipeline(encrypt, compress, passphraseFile string) (string, error) {
	var stages []string
	switch encrypt {
	case "", "none":
	case "gpg":
		// A symmetric dump records no key id, so the documented one-liner pins the
		// passphrase file (a public-key dump auto-discovers its private key from the keyring).
		gpg := "gpg -d --batch --yes --no-tty"
		if passphraseFile != "" {
			gpg = "gpg --passphrase-file " + shSingleQuote(passphraseFile) + " -d --batch --yes --no-tty"
		}
		stages = append(stages, gpg)
	default:
		return "", fmt.Errorf("stock drill: unknown encryption scheme %q", encrypt)
	}
	switch compress {
	case "", "none":
	case "gzip":
		stages = append(stages, "gzip -dc")
	case "zstd":
		stages = append(stages, "zstd -dc")
	default:
		return "", fmt.Errorf("stock drill: unknown compression scheme %q", compress)
	}
	stages = append(stages, `tar --extract --listed-incremental=/dev/null --numeric-owner -C "$1" -f -`)
	return strings.Join(stages, " | "), nil
}

// unattendedReachable reports whether a target's source copy can be read without a
// human loading a tape. Address-identified media (disk/cloud) and robotic libraries
// (which auto-mount) are always reachable; a single-drive station is reachable only
// when every needed volume is already the one loaded in its drive. A medium that
// fails to open or inventory is NOT treated as reachable: OpenAdmin succeeds for
// address-identified media too, so an error here is a real problem (e.g. a
// write-held medium) and rides into the skip reason.
func (d *driller) unattendedReachable(medium string, steps []recovery.Step) (bool, string) {
	am, _, err := d.dep.OpenAdmin(medium)
	if err != nil {
		return false, fmt.Sprintf("cannot open medium %q: %v", medium, err)
	}
	defer am.Close()
	// The changer capability — not a View error — decides "nothing to mount": an
	// address-identified volume (disk/cloud) has no changer, so nothing needs a human.
	if _, isChanger := am.Volume().(media.Changer); !isChanger {
		return true, ""
	}
	view, err := am.View()
	if err != nil {
		return false, fmt.Sprintf("cannot inventory medium %q: %v", medium, err)
	}
	if !view.Manual {
		return true, "" // a robot loads the right run itself
	}
	loaded := ""
	if len(view.Drives) > 0 && view.Drives[0].Loaded {
		loaded = view.Drives[0].Volume.Label
	}
	for _, v := range d.chainLabels(steps, medium) {
		if v != loaded {
			return false, fmt.Sprintf("needs volume %q in the %q drive (a human-only swap); loaded: %s", v, medium, volumeOrEmpty(loaded))
		}
	}
	return true, ""
}

// volumeOrEmpty renders a volume label for a message, or "(empty)" for none.
func volumeOrEmpty(label string) string {
	if label == "" {
		return "(empty)"
	}
	return fmt.Sprintf("%q", label)
}

// chainLabels is the distinct set of volume labels a chain's copies occupy on the
// medium — the tapes an unattended drill would need mounted.
func (d *driller) chainLabels(steps []recovery.Step, medium string) []string {
	seen := map[string]bool{}
	var out []string
	for _, step := range steps {
		for _, p := range placementsOnMedium(d.placementsFor(step.RunID), medium) {
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
func (d *driller) chainBytes(steps []recovery.Step) int64 {
	var n int64
	for _, step := range steps {
		s, err := d.cat.ReadRun(step.RunID)
		if err != nil {
			continue
		}
		if a, ok := s.Archive(step.DLE, step.Level); ok {
			n += a.Compressed
		}
	}
	return n
}

// classifyOpenErr maps an archive-open failure to a class: a missing copy or an
// unavailable volume is ClassMissing, anything else (decrypt setup, read error) is
// ClassPipeline. It matches the producers' sentinel errors (errMissingCopy from the
// catalog read path, librarian.ErrVolumeUnavailable from the mount path) via errors.Is,
// so reclassification does not silently follow a reworded message.
func classifyOpenErr(err error) drill.Class {
	if errors.Is(err, archivefs.ErrMissingCopy) || errors.Is(err, librarian.ErrVolumeUnavailable) {
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
