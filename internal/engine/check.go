package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/accounting"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/depot"
	"github.com/Niloen/nbackup/internal/dumper"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

// CheckReport is the structured result of `nb check`: server readiness
// plus per-host (client) readiness. The CLI renders it and exits non-zero when Failures>0.
type CheckReport struct {
	Server   []CheckLine
	Hosts    []HostCheck
	Failures int
	Warnings int
}

// CheckLine is one assertion: OK, a soft Warn, or (neither) a hard failure.
type CheckLine struct {
	OK   bool
	Warn bool
	Msg  string
}

// HostCheck groups the checks for one source host.
type HostCheck struct {
	Host   string
	Remote bool
	Target string // the SSH target shown for a remote host ("" for local)
	Lines  []CheckLine
}

// checker is the `nb check` health-probe lane. It reads config and probes tools,
// media, and hosts through the two resolution services — the toolchain (executors,
// archivers, transform options) and the depot (medium opens) — and never writes
// backup data. It is stateless; the engine builds one per Check call.
type checker struct {
	cfg  *config.Config
	tc   *toolchain
	dep  *depot.Depot
	cat  *catalog.Catalog
	acct *accounting.Accountant
}

// Check verifies the configuration is runnable: the server side always, and each source
// host. Every probe runs through the host's executor — `Local` for a `localhost` DLE, SSH
// for a remote one — so the same code checks both; the only difference is that a remote
// host is skipped (not probed) when connect is false (the `--offline` view). It never
// writes backup data.
func (e *Engine) Check(connect bool) *CheckReport {
	return (&checker{cfg: e.cfg, tc: e.tc, dep: e.dep, cat: e.cat, acct: e.acct}).Check(connect)
}

// Check runs the full server + per-host probe; see the Engine facade for the contract.
func (c *checker) Check(connect bool) *CheckReport {
	rep := &CheckReport{}
	c.checkServer(rep)
	c.checkStaleness(rep)
	c.checkCapacity(rep)
	for _, host := range c.dleHostsInOrder() {
		rep.Hosts = append(rep.Hosts, c.checkHost(rep, host, connect))
	}
	return rep
}

// checkStaleness fails the check for any configured DLE whose newest backup (at
// any level) predates the dump cycle — cycle is documented as the one scheduling
// knob ("a full never ages past one cycle"), so a DLE older than that has provably
// broken that promise, independent of cron cadence. It is always on (zero config):
// `nb check` is already the command cron gates on ("$? != 0 means don't trust last
// night"), and an overdue DLE is exactly that kind of actionable failure. A DLE with
// no archive at all is only a WARNING, not a failure — a fresh install has nothing
// backed up yet, and that must not turn red before its first dump ever runs.
func (c *checker) checkStaleness(rep *CheckReport) {
	window := c.cfg.CycleDuration()
	dles := make([]string, 0, len(c.cfg.DLEs()))
	idOf := map[string]string{}
	for _, d := range c.cfg.DLEs() {
		dles = append(dles, d.Name())
		idOf[d.Name()] = d.ID()
	}
	stale := c.cat.StaleDLEs(dles, window, time.Now())
	if len(stale) == 0 {
		rep.add(&rep.Server, true, false, fmt.Sprintf("staleness: all DLE(s) backed up within one cycle (%s)", sizeutil.FormatDuration(window)))
		return
	}
	for _, s := range stale {
		disp := idOf[s.DLE]
		if s.LastBackup.IsZero() {
			rep.add(&rep.Server, false, true, fmt.Sprintf("staleness: %s has never been backed up", disp))
			continue
		}
		rep.add(&rep.Server, false, false, fmt.Sprintf("staleness: %s last backed up %s ago, older than one cycle (%s)",
			disp, sizeutil.FormatDuration(time.Since(s.LastBackup)), sizeutil.FormatDuration(window)))
	}
}

// checkCapacity surfaces each bounded medium's make-room feasibility: capacity is
// a promise the dump keeps by reclaiming before writing, so a protected set that
// leaves no room for a typical run is tomorrow's refusal, visible today. Warnings,
// not failures — the next real plan may be smaller than the newest run.
func (c *checker) checkCapacity(rep *CheckReport) {
	for _, rc := range c.acct.RoomChecks(time.Now()) {
		rep.add(&rep.Server, rc.OK, !rc.OK, rc.Msg)
	}
}

// add appends a line and counts a hard failure (not OK and not a warning).
func (rep *CheckReport) add(lines *[]CheckLine, ok, warn bool, msg string) {
	*lines = append(*lines, CheckLine{OK: ok, Warn: warn, Msg: msg})
	switch {
	case !ok && !warn:
		rep.Failures++
	case warn:
		rep.Warnings++
	}
}

// checkAtomShapes is the config-time rung of the atom validation ladder: for each
// dumptype it resolves the archive shape from the declared capabilities and warns —
// never fails — about (a) an inert dumptype part_size (the atom knob does nothing
// without a per-frame stage) and (b) dumptype × medium pairs whose atoms exceed the
// medium's part ceiling and so can never land or sync there. Warnings, because adding
// a low-ceiling medium must not brick the config; the routed pair hardens to a
// dump-time error (atomCeilingErr) and sync refuses per archive.
func (c *checker) checkAtomShapes(rep *CheckReport) {
	dtNames := make([]string, 0, len(c.cfg.DumpTypes))
	for n := range c.cfg.DumpTypes {
		dtNames = append(dtNames, n)
	}
	sort.Strings(dtNames)
	for _, name := range dtNames {
		dt := c.cfg.DumpTypes[name]
		shape, err := dumper.ShapeFor(c.tc.encodePlacement(name))
		if err != nil {
			continue // an unknown scheme fails elsewhere (checkMedia/compression checks)
		}
		if shape != record.ShapeAtomic {
			if dt.PartSize != "" {
				rep.add(&rep.Server, true, true, fmt.Sprintf("dumptype %q sets part_size but has no per-frame (encryption) stage — the atom-size knob is inert there", name))
			}
			continue
		}
		atomSize := c.cfg.AtomSizeBytes(name)
		mNames := make([]string, 0, len(c.cfg.Media))
		for m := range c.cfg.Media {
			mNames = append(mNames, m)
		}
		sort.Strings(mNames)
		for _, m := range mNames {
			ceiling := media.PartSizeFor(c.cfg.Media[m].Type).Max
			if ceiling > 0 && atomSize > ceiling {
				rep.add(&rep.Server, true, true, fmt.Sprintf("dumptype %q atoms (%s part_size) exceed medium %q's %s part ceiling — its archives can never land or sync there (a sealed atom cannot be re-cut)",
					name, sizeutil.FormatBytes(atomSize), m, sizeutil.FormatBytes(ceiling)))
			}
		}
	}
}

func (c *checker) checkServer(rep *CheckReport) {
	c.checkMedia(rep)
	c.checkAtomShapes(rep)

	wd := c.cfg.WorkdirPath()
	if err := writableDir(wd); err != nil {
		rep.add(&rep.Server, false, false, fmt.Sprintf("workdir %s not writable: %v", wd, err))
	} else {
		rep.add(&rep.Server, true, false, fmt.Sprintf("workdir %s writable", wd))
	}
	if !filepath.IsAbs(wd) {
		abs, _ := filepath.Abs(wd)
		rep.add(&rep.Server, false, true, fmt.Sprintf("workdir %q is relative (resolves to %s); a cron job that runs nb from another directory will use a different catalog — set an absolute `workdir`", wd, abs))
	}

	// The compressor is needed server-side for a server-side compress and for restore
	// decompression of any scheme a dumptype records, so check every distinct scheme a
	// missing binary is a real problem even with client-side dumps.
	checkedScheme := map[string]bool{}
	for _, scheme := range append([]string{c.cfg.CompressScheme()}, c.tc.dumptypeCompressSchemes()...) {
		if checkedScheme[scheme] {
			continue
		}
		checkedScheme[scheme] = true
		if err := compress.Check(scheme, c.tc.fopts); err != nil {
			msg := fmt.Sprintf("compression %q: %v", scheme, err)
			// The common failure is simply a missing binary; compress.Check's wrapped
			// LookPath error restates the name three times with no way out. One
			// statement, plus the remedy.
			if errors.Is(err, exec.ErrNotFound) {
				alternatives := "gzip or none"
				if scheme == "gzip" {
					alternatives = "none"
				}
				msg = fmt.Sprintf("compression %q: binary not found on PATH (install %s, or set compress.scheme: %s)", scheme, scheme, alternatives)
			}
			rep.add(&rep.Server, false, false, msg)
		} else {
			rep.add(&rep.Server, true, false, fmt.Sprintf("compression %q available", scheme))
		}
	}

	checked := map[string]bool{}
	for _, d := range c.cfg.DLEs() {
		dt := d.DumpTypeName()
		if checked[dt] {
			continue
		}
		checked[dt] = true
		scheme, opts := c.tc.encryptionFor(dt)
		if scheme == "" || scheme == "none" {
			continue
		}
		if err := crypt.Check(scheme, opts); err != nil {
			rep.add(&rep.Server, false, false, fmt.Sprintf("dumptype %q encryption: %v", dt, err))
		} else {
			rep.add(&rep.Server, true, false, fmt.Sprintf("dumptype %q encryption %q configured", dt, scheme))
			if pf := opts.PassphraseFile; pf != "" {
				if info, statErr := os.Stat(pf); statErr == nil && info.Mode().Perm()&0o077 != 0 {
					rep.add(&rep.Server, false, true, fmt.Sprintf("passphrase_file %q is group/world-readable (%#o) — chmod 600 to protect the symmetric key", pf, info.Mode().Perm()))
				}
			}
		}
	}
}

// checkMedia probes every configured medium, not just the landing one. The landing's
// failure is hard (a dump cannot run); a non-landing medium's is a warning — it is a
// sync/copy target, so the local backup still works but replication to it would fail,
// which a `check` that ignored it would let the operator discover only at sync time. A
// cloud medium's reachability needs credentials/network, so it is reported as
// configured-but-unprobed (it is validated at first use) rather than opened here.
func (c *checker) checkMedia(rep *CheckReport) {
	landing := c.dep.LandingName()
	names := make([]string, 0, len(c.cfg.Media))
	for n := range c.cfg.Media {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		isLanding := name == landing
		label := fmt.Sprintf("medium %q", name)
		if isLanding {
			label = fmt.Sprintf("landing medium %q", name)
		}
		// An unknown medium type is a config error, not a transient readiness issue,
		// so it is a hard failure even for a non-landing medium — otherwise `nb check`
		// green-lights a config whose sync/copy target can never be constructed.
		if !media.KnownVolumeType(c.cfg.Media[name].Type) {
			rep.add(&rep.Server, false, false, fmt.Sprintf("%s has unknown type %q (known: %v)", label, c.cfg.Media[name].Type, media.VolumeTypes()))
			continue
		}
		// The landing is where dumps must go, so `nb check` opens it for real (the one
		// place that deliberately pays the open the rest of the system now defers) and a
		// failure is a hard error. For a cloud landing this is exactly where missing
		// credentials or an unreachable bucket surface; opening it also bootstraps the
		// catalog, so a clean open means the landing is genuinely runnable.
		if isLanding {
			if _, err := c.dep.Landing(); err != nil {
				rep.add(&rep.Server, false, false, fmt.Sprintf("%s not ready: %v", label, err))
			} else {
				rep.add(&rep.Server, true, false, fmt.Sprintf("%s ready", label))
			}
			continue
		}
		// A non-landing cloud tier is a sync/copy target validated at first use: probing
		// it here would force credentials/network for a medium the local backup doesn't
		// need, so report it as configured-but-unprobed.
		if c.cfg.Media[name].Type == "cloud" {
			rep.add(&rep.Server, false, true, fmt.Sprintf("%s (cloud) configured — reachability checked at first use, not here", label))
			continue
		}
		if _, _, _, err := c.dep.MediumVolume(name); err != nil {
			rep.add(&rep.Server, false, true, fmt.Sprintf("%s not ready: %v", label, err))
		} else {
			rep.add(&rep.Server, true, false, fmt.Sprintf("%s ready", label))
		}
	}
}

func (c *checker) checkHost(rep *CheckReport, host string, connect bool) HostCheck {
	ssh, remote := c.cfg.RemoteHost(host)
	hc := HostCheck{Host: host, Remote: remote}
	if remote {
		hc.Target = sshTarget(host, ssh)
		if !connect {
			rep.add(&hc.Lines, false, true, "remote — not probed (drop --offline to connect)")
			return hc
		}
	}

	ex := c.tc.executorFor(host) // Local() for a local host, SSH for a remote one
	if remote {
		if err := ex.Command("true").Run(); err != nil {
			rep.add(&hc.Lines, false, false, fmt.Sprintf("unreachable over SSH: %v", err))
			return hc // nothing else is probeable
		}
		rep.add(&hc.Lines, true, false, "reachable over SSH")
	}

	dles := c.dlesForHost(host)
	seen := map[string]bool{}
	for _, d := range dles {
		dt := d.DumpTypeName()
		if seen[dt] {
			continue
		}
		seen[dt] = true
		arch, err := c.tc.archiverFor(dt, host)
		if err == nil {
			err = arch.Check()
		}
		// The checker is archiver-neutral: the archiver's own Check error names the
		// missing tool (e.g. GNU tar), so the generic line just says "archiver".
		if err != nil {
			rep.add(&hc.Lines, false, false, fmt.Sprintf("archiver (dumptype %q): %v", dt, err))
		} else {
			rep.add(&hc.Lines, true, false, fmt.Sprintf("archiver ready (dumptype %q)", dt))
		}
		c.checkClientTools(rep, &hc, ex, dt)
	}

	// The source probe is the archiver's (CheckSource): "ready" means readable
	// directory for a tree archiver, nothing at all for pipe (its producer command
	// owns the source), connectivity for a future db archiver. An archiver that
	// failed to open was already reported above — don't pile a second line on.
	for _, d := range dles {
		arch, err := c.tc.archiverFor(d.DumpTypeName(), host)
		if err != nil {
			continue
		}
		if err := arch.CheckSource(d.Path); err != nil {
			rep.add(&hc.Lines, false, false, fmt.Sprintf("source %s: %v", d.Path, err))
		} else {
			rep.add(&hc.Lines, true, false, fmt.Sprintf("source %s ready", d.Path))
		}
	}

	// The incremental-state library lives on the host where the archiver runs (the client
	// for a remote DLE, the server for a local one) and is now distinct from the catalog
	// workdir, so verify it for every host.
	stateDir := c.cfg.StateDirFor(host)
	if err := ex.MkdirAll(stateDir); err != nil {
		rep.add(&hc.Lines, false, false, fmt.Sprintf("state_dir %s not creatable: %v", stateDir, err))
	} else {
		rep.add(&hc.Lines, true, false, fmt.Sprintf("state_dir %s writable", stateDir))
	}
	// A relative state_dir resolves against the working directory of whoever runs nb
	// on this host, so a cron job started from elsewhere loses the incremental base and
	// silently re-fulls every DLE. The catalog rebuilds from media; this state does not.
	if !filepath.IsAbs(stateDir) {
		rep.add(&hc.Lines, false, true, fmt.Sprintf("state_dir %q is relative; it resolves against nb's working directory on %s, so a cron job run from another directory will lose the incremental base and re-full — set an absolute `state_dir`", stateDir, host))
	}
	return hc
}

// checkClientTools probes the compressor / gpg on the host when a dumptype runs them there
// (compress/encrypt: client). For a server-side transform there is nothing to check here —
// the server-side tools are covered in checkServer.
func (c *checker) checkClientTools(rep *CheckReport, hc *HostCheck, ex programs.Executor, dt string) {
	if c.cfg.CompressionFor(dt).At == "client" {
		scheme, opts := c.tc.compressionFor(dt)
		if cmd, ok, err := compress.CompressCmd(scheme, opts); err == nil && ok {
			c.probeTool(rep, hc, ex, cmd.Name, "compressor")
		}
	}
	if c.cfg.EncryptionFor(dt).At == "client" {
		scheme, opts := c.tc.encryptionFor(dt)
		if cmd, ok, err := crypt.EncryptCmd(scheme, opts); err == nil && ok {
			c.probeTool(rep, hc, ex, cmd.Name, "encryptor")
		}
	}
}

func (c *checker) probeTool(rep *CheckReport, hc *HostCheck, ex programs.Executor, bin, role string) {
	if err := ex.Command(bin, "--version").Run(); err != nil {
		rep.add(&hc.Lines, false, false, fmt.Sprintf("client %s %q: %v", role, bin, err))
	} else {
		rep.add(&hc.Lines, true, false, fmt.Sprintf("client %s %q present", role, bin))
	}
}

// dleHostsInOrder returns the distinct DLE hosts in config order.
func (c *checker) dleHostsInOrder() []string {
	var order []string
	seen := map[string]bool{}
	for _, d := range c.cfg.DLEs() {
		if !seen[d.Host] {
			seen[d.Host] = true
			order = append(order, d.Host)
		}
	}
	return order
}

// dlesForHost returns the DLEs on a host, in config order.
func (c *checker) dlesForHost(host string) []config.DLE {
	var out []config.DLE
	for _, d := range c.cfg.DLEs() {
		if d.Host == host {
			out = append(out, d)
		}
	}
	return out
}

// sshTarget renders the human-readable SSH target for a host.
func sshTarget(host string, ssh config.SSHConfig) string {
	t := "ssh "
	if ssh.User != "" {
		t += ssh.User + "@"
	}
	t += host
	if ssh.Port != "" {
		t += " -p " + ssh.Port
	}
	return t
}

// writableDir reports whether dir is writable, creating it if absent (the workdir is
// created on first use, so a creatable path is fine).
func writableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".nbackup-check")
	f, err := os.Create(probe)
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(probe)
}
