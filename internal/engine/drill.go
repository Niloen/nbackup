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
	DLEs       []string      // drill exactly these DLEs (slug or host:path), bypassing selection; empty = risk-biased selection
	Window     time.Duration // each DLE should be drilled within this window
	Sample     int           // max DLEs to drill this run (<=0 = every due DLE); ignored for named DLEs
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
	Medium        string // the run-wide framing medium: the --from pin, else the primary landing
	PerRoute      bool   // no --from pin: each target was drilled off its own landing route (Medium is only the default framing)
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
	// The drill medium is resolved PER DLE, not once. With --from unset, each DLE is
	// drilled off its own landing route (primary first), narrowed to the media that
	// actually hold its chain — so a DLE with two landings passes if EITHER copy is
	// good, and only a DLE with no routed copy anywhere is ClassMissing. An explicit
	// --from pins every target to that one medium (prove THAT copy; absent = a real
	// missing). See drillMedium.
	pin := opts.Medium
	if pin != "" {
		if _, ok := d.cfg.Media[pin]; !ok {
			return nil, fmt.Errorf("unknown drill medium %q %s", pin, mediaNamesHint(d.cfg))
		}
	}
	routes := promiseRoutes(d.cfg, d.cat)
	// The concrete medium the run-wide framing names (header, WORM probe): the pin,
	// else the primary landing. Per-target reads use drillMedium, not this.
	defaultMedium := pin
	if defaultMedium == "" {
		defaultMedium = d.dep.LandingName()
	}

	dles := d.dles.names()
	ledger, err := drill.Load(d.cfg.WorkdirPath())
	if err != nil {
		return nil, err
	}
	var targets []drill.Target
	if len(opts.DLEs) > 0 {
		// Named targets: the operator's re-drill ("was that failure a hiccup?").
		// Drill exactly these DLEs now — no window rotation, no sample cap — so a
		// pass overwrites the DLE's ledger record and clears its warning.
		targets, err = d.namedTargets(opts.DLEs, ledger, opts)
		if err != nil {
			return nil, err
		}
	} else {
		targets = drill.Select(dles, d.cat.Archives(), opts.AsOf, ledger, opts.Window, opts.Sample, opts.Now)
	}

	rep := &DrillReport{
		AsOf: opts.AsOf, Window: opts.Window, Medium: defaultMedium, PerRoute: pin == "", Tier: opts.Tier,
		Apply: opts.Apply, Unattended: opts.Unattended, Ledger: ledger,
	}
	rep.NeverDrilled, rep.Overdue = ledger.Coverage(dles, opts.Window, opts.Now)
	d.toDisplay(rep.NeverDrilled)

	// Price the egress of reading the selected chains off the drill medium — the
	// honest cost of an offsite drill (an encrypted+compressed archive is all-or-
	// nothing, so a structural/chain drill spends the full bytes; the sample tier
	// reads one sealed part per archive, so it is priced on those parts alone).
	// Price each target on the medium it will actually be read from — a cloud-routed
	// DLE prices its provider's egress even when the run's default medium is a free
	// local disk, and vice versa. Providers can differ across DLEs, so the run total
	// aggregates them ("mixed" when it spans more than one paid provider).
	for _, t := range targets {
		m := d.drillMedium(t, routes, pin)
		cm := d.acct.CostModelFor(m)
		if !cm.Priced() {
			continue
		}
		rep.Priced = true
		if rep.Provider == "" {
			rep.Provider = cm.Provider
		} else if rep.Provider != cm.Provider {
			rep.Provider = "mixed"
		}
		if opts.Tier == drill.TierSample {
			var bytes, gets int64
			rec, _ := ledger.Get(t.DLE)
			if choices, ok := d.samplePlan(t.Steps, m, rec.Drills); ok {
				for _, c := range choices {
					bytes += c.size
					gets++
				}
			} else { // sealless copy: sampling falls back to the full checksum read
				bytes += d.chainBytes(t.Steps)
				gets += int64(len(t.Steps))
			}
			rep.ForecastCost += cm.ReadCost(bytes, gets)
		} else {
			refs := make([]archiveio.Ref, 0, len(t.Steps))
			for _, s := range t.Steps {
				refs = append(refs, archiveio.Ref{Run: s.RunID, DLE: s.DLE, Level: s.Level})
			}
			rep.ForecastCost += d.acct.EstimateRead(refs, m).Cost
		}
	}

	if !opts.Apply {
		// Dry-run: show what would run and its forecast cost; touch no media, write
		// no ledger. The WORM probe is detect-only here (no probe object written).
		for _, t := range targets {
			rec, _ := ledger.Get(t.DLE)
			m := d.drillMedium(t, routes, pin)
			b := d.targetBytes(t, m, opts.Tier, rec.Drills)
			rep.Targets = append(rep.Targets, DrillResult{
				DLE: t.DLE, DLEDisplay: d.dles.display(t.DLE), RunID: t.RunID, AsOf: t.AsOf, Medium: m, Tier: opts.Tier, Bytes: b,
			})
			rep.ForecastBytes += b
		}
		rep.Worm = d.wormProbe(defaultMedium, false, opts.Now)
		rep.Posture = d.posture(rep.Worm, 0)
		return rep, nil
	}

	for _, t := range targets {
		prev, _ := ledger.Get(t.DLE)
		m := d.drillMedium(t, routes, pin)
		res := d.drillTarget(t, m, prev.Drills, opts, logf)
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
				DLE: t.DLE, LastDrill: opts.Now, Tier: opts.Tier.String(), Medium: m,
				AsOf: t.AsOf, RunID: t.RunID, OK: res.OK,
				Class: failureToken(res), Detail: res.Detail,
				Bytes:  res.Bytes,
				Drills: prev.Drills + 1, // advances the sample tier's part rotation
			})
			if err := ledger.Save(d.cfg.WorkdirPath()); err != nil {
				return rep, fmt.Errorf("save drill ledger: %w", err)
			}
		}
	}

	rep.Worm = d.wormProbe(defaultMedium, opts.Worm, opts.Now)
	rep.Posture = d.posture(rep.Worm, rep.Failures)
	// Recompute coverage against the freshly updated ledger.
	rep.NeverDrilled, rep.Overdue = ledger.Coverage(dles, opts.Window, opts.Now)
	d.toDisplay(rep.NeverDrilled)
	return rep, nil
}

// drillMedium picks the medium to read a target from. A --from pin drills that one
// medium for every target — the "prove THAT copy" mode, where a DLE absent from it is
// a real ClassMissing. Otherwise it resolves per DLE: the DLE's configured landing
// route (primary first, from promiseRoutes), narrowed to the media that actually hold
// the whole chain, so a DLE with two landings drills whichever copy exists. When no
// routed medium holds the chain it falls back to the route's primary (else the default
// landing), so the resulting "no copy on medium %q" names the medium the copy was owed
// to — an honest replication gap, not a lookup on the wrong medium.
func (d *driller) drillMedium(t drill.Target, routes map[string][]string, pin string) string {
	if pin != "" {
		return pin
	}
	route := routes[t.DLE]
	for _, m := range route {
		if d.chainOn(t.Steps, m) {
			return m
		}
	}
	if len(route) > 0 {
		return route[0]
	}
	return d.dep.LandingName()
}

// chainOn reports whether medium m holds a copy of every step in the restore chain —
// the condition for reading the whole chain off a single medium (the drill's one-pass
// model). A chain split across media fails here for each medium and drillMedium falls
// back to the owed-to primary. The test is per ARCHIVE, not per run: a run dumps many
// DLEs and each is copied/pruned independently, so a medium can hold the run (another
// DLE's archive) while this DLE's archive there has been pruned. Asking only whether
// the run is on the medium would pick a medium that cannot actually read this chain.
func (d *driller) chainOn(steps []recovery.Step, m string) bool {
	for _, s := range steps {
		p, ok := placementOn(d.cat, s.RunID, m)
		if !ok {
			return false
		}
		if _, ok := p.Placed(s.DLE, s.Level); !ok {
			return false
		}
	}
	return true
}

// namedTargets resolves user-named DLEs (slug or host:path) into drill targets,
// bypassing the window/sample selection: a named DLE is drilled unconditionally.
// It reuses Select with a zero window (nothing is "covered") and no cap, then
// insists every name survived — a named target with no recovery point is an
// error, never a silent skip.
func (d *driller) namedTargets(refs []string, ledger *drill.Ledger, opts DrillOptions) ([]drill.Target, error) {
	seen := map[string]bool{}
	slugs := make([]string, 0, len(refs))
	for _, ref := range refs {
		slug, ok := d.dles.resolve(ref)
		if !ok {
			return nil, fmt.Errorf("unknown DLE %q — the catalog knows: %s", ref, strings.Join(d.dles.displayAll(), ", "))
		}
		if !seen[slug] {
			seen[slug] = true
			slugs = append(slugs, slug)
		}
	}
	targets := drill.Select(slugs, d.cat.Archives(), opts.AsOf, ledger, 0, 0, opts.Now)
	if len(targets) != len(slugs) {
		got := map[string]bool{}
		for _, t := range targets {
			got[t.DLE] = true
		}
		for _, slug := range slugs {
			if !got[slug] {
				return nil, fmt.Errorf("DLE %s has no recovery point at or before %s — nothing to drill", d.dles.display(slug), opts.AsOf)
			}
		}
	}
	return targets, nil
}

// displayAll rewrites a slice of DLE slugs to their host:path display identities
// in place — coverage lists leave the engine as the ids an operator reads.
func (d *driller) toDisplay(slugs []string) {
	for i, s := range slugs {
		slugs[i] = d.dles.display(s)
	}
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
	return d.frameSample(t, medium, rot, logf)
}

// frameSample adds the sample tier's STRUCTURAL half on a framed archive: decode one
// frame group of the chain's tip through the real pipeline and compare the listed
// members+offsets against the index slice — the seal check above proves the stored
// bytes, this proves they still decode to the recorded structure, still at bounded
// egress (rot rotates the frame across drills like the seal check's part). An archive
// without the ingredients (no frame table, no offsets, no range-capable copy) skips
// silently: the seal sample already ran, and this half only exists where framing does.
func (d *driller) frameSample(t drill.Target, medium string, rot int, logf Logf) (drill.Class, string) {
	step := t.Steps[len(t.Steps)-1] // the tip: the archive a restore would read last
	s, err := d.cat.ReadRun(step.RunID)
	if err != nil {
		return drill.ClassNone, ""
	}
	a, ok := s.Archive(step.DLE, step.Level)
	if !ok {
		return drill.ClassNone, ""
	}
	arch, err := d.tc.restoreArchiver(a.ArchiverType, a.ArchiverName, a.DLE, "")
	if err != nil {
		return drill.ClassNone, ""
	}
	// The restorer resolves the shape itself: framed → decode one frame group;
	// atomic → decrypt-and-list one atom (the KEY-PROVING check); encrypted
	// stream shape → nothing cheap to sample (Ran=false).
	res, err := d.rst.Sample(medium, a, d.rst.DecryptOptsFor(a.DLE), arch, rot)
	if err != nil {
		return classifyReadErr(err), restorer.DecryptHint(a.Encrypt, err).Error()
	}
	if !res.Ran {
		return drill.ClassNone, ""
	}
	if !res.OK {
		return drill.ClassIntegrity, fmt.Sprintf("%s %s L%d %s %d structural sample: %s",
			step.RunID, step.DLE, step.Level, res.Unit, res.Frame, res.Detail)
	}
	fetched := "the stream tail"
	if res.Bytes >= 0 {
		fetched = sizeutil.FormatBytes(res.Bytes)
	}
	logf.Log("drill %s: %s %d structural sample OK (%s fetched)", d.dles.display(t.DLE), res.Unit, res.Frame, fetched)
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
	if errors.Is(err, archiveio.ErrSealMismatch) {
		// The stream's inline seal check caught corruption before (or while) the
		// consumer choked on it — the root cause is a damaged copy, not composition.
		return drill.ClassIntegrity
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

// stockExtractAtomic validates the atomic shape's documented one-liner — a FILE LOOP,
// since each atom is one complete encrypted message and gpg refuses concatenated
// input: the atoms are fetched to files (NBackup only moves bytes; on a bare bucket
// they already ARE these files) and the stock recipe is
// `for p in atoms/*; do gpg -d "$p"; done | zstd -d | tar x`.
func (d *driller) stockExtractAtomic(step recovery.Step, dest, medium string, logf Logf) (drill.Class, string) {
	ref := archiveio.Ref{Run: step.RunID, DLE: step.DLE, Level: step.Level}
	seals, err := d.fs.AtomSeals(ref)
	if err != nil || len(seals) == 0 {
		return drill.ClassPipeline, "atomic archive records no per-part seals on any copy — its atoms cannot be cut; run `nb rebuild`"
	}
	dir, err := os.MkdirTemp("", "nbackup-drill-atoms-*")
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	defer os.RemoveAll(dir)
	src, err := d.fs.OpenArchive(ref, medium)
	if err != nil {
		return classifyOpenErr(err), err.Error()
	}
	for i, s := range seals {
		f, err := os.Create(fmt.Sprintf("%s/atom-%03d.bin", dir, i))
		if err != nil {
			src.Close()
			return drill.ClassPipeline, err.Error()
		}
		_, cerr := io.CopyN(f, src, s.Size)
		f.Close()
		if cerr != nil {
			src.Close()
			return classifyOpenErr(cerr), cerr.Error()
		}
	}
	src.Close()

	tail, err := d.stockTail(step)
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	script, err := stockAtomPipeline(step.Encrypt, step.Compress, d.rst.DecryptOptsFor(step.DLE).PassphraseFile, tail)
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	// `sh -c <script> sh <dest> <atomdir>` makes $1 == dest and $2 == the atoms inside.
	cmd := exec.Command("/bin/sh", "-c", script, "sh", dest, dir)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// The script's "$2"/atom-* glob matches this drill's own staged copies (named
	// atom-NNN.bin above), not the real run directory's `…-L<n>.p*.tar.<ext>[.gpg]`
	// filenames — say so, so a reader copying this line against a real run dir
	// isn't left with a silent no-match (see docs/restore-by-hand.md for that glob).
	logf.Log("stock-restoring %s %s L%d via the documented atom-loop shape (against this drill's staged copies, not the real run-dir filenames): %s", step.RunID, step.DLE, step.Level, script)
	if err := cmd.Run(); err != nil {
		return drill.ClassPipeline, fmt.Sprintf("stock atom loop failed: %v\n%s", err, strings.TrimSpace(stderr.String()))
	}
	return drill.ClassNone, ""
}

// stockAtomPipeline builds the atomic shape's documented restore loop: decrypt each
// atom file in order, concatenate the plaintexts (whole compressed frames — one valid
// stream), decompress, extract into "$1" via the archiver's stock tail. The atoms are
// the files under "$2".
func stockAtomPipeline(encrypt, compress, passphraseFile, extract string) (string, error) {
	tail, err := stockPipeline("none", compress, "", extract)
	if err != nil {
		return "", err
	}
	var gpg string
	switch encrypt {
	case "gpg":
		gpg = "gpg -d --batch --yes --no-tty"
		if passphraseFile != "" {
			gpg = "gpg --passphrase-file " + shSingleQuote(passphraseFile) + " -d --batch --yes --no-tty"
		}
	default:
		return "", fmt.Errorf("stock drill: no documented atom-loop recipe for encryption scheme %q", encrypt)
	}
	return `for p in "$2"/atom-*; do ` + gpg + ` "$p"; done | ` + tail, nil
}

func (d *driller) stockExtractStep(step recovery.Step, dest, medium string, logf Logf) (drill.Class, string) {
	if step.Shape.StandaloneParts() {
		// Each part is a complete file of its type, so the stock recovery is the
		// documented per-file loop over the fetched atoms, not concatenate-then-decode.
		return d.stockExtractAtomic(step, dest, medium, logf)
	}
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

	tail, err := d.stockTail(step)
	if err != nil {
		return drill.ClassPipeline, err.Error()
	}
	script, err := stockPipeline(step.Encrypt, step.Compress, d.rst.DecryptOptsFor(step.DLE).PassphraseFile, tail)
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

// stockTail resolves the archiver's stock extraction fragment for a step — the
// per-format tail of the documented one-liner (gnutar's `tar --extract … -C "$1"`,
// pipe's consumer command). An archiver declaring none fails the stock tier with
// that fact, rather than the tier silently exercising the wrong command.
func (d *driller) stockTail(step recovery.Step) (string, error) {
	arch, err := d.tc.restoreArchiver(step.Archiver, step.ArchiverName, step.DLE, "")
	if err != nil {
		return "", err
	}
	tail := arch.StockExtract()
	if tail == "" {
		return "", fmt.Errorf("stock drill: archiver %q documents no stock extraction command", step.Archiver)
	}
	return tail, nil
}

// stockPipeline builds the documented restore one-liner for an archive's
// (encrypt, compress): decrypt (gpg) then decompress (zstd/gzip) then the
// archiver's stock extraction tail, reading stdin and extracting into "$1". It is
// deliberately the README's stock command, not NBackup's own filter/crypt/method code.
func stockPipeline(encrypt, compress, passphraseFile, extract string) (string, error) {
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
	stages = append(stages, extract)
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
	if errors.Is(err, archiveio.ErrSealMismatch) {
		return drill.ClassIntegrity
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
