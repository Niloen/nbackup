package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

// CheckReport is the structured result of `nb check`: server readiness
// plus per-host (client) readiness. The CLI renders it and exits non-zero when Failures>0.
type CheckReport struct {
	Server   []CheckLine
	Hosts    []HostCheck
	Failures int
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

// Check verifies the configuration is runnable: the server side always, and each source
// host. Every probe runs through the host's executor — `Local` for a `localhost` DLE, SSH
// for a remote one — so the same code checks both; the only difference is that a remote
// host is skipped (not probed) when connect is false (the `--offline` view). It never
// writes backup data.
func (e *Engine) Check(connect bool) *CheckReport {
	rep := &CheckReport{}
	e.checkServer(rep)
	for _, host := range e.dleHostsInOrder() {
		rep.Hosts = append(rep.Hosts, e.checkHost(rep, host, connect))
	}
	return rep
}

// add appends a line and counts a hard failure (not OK and not a warning).
func (rep *CheckReport) add(lines *[]CheckLine, ok, warn bool, msg string) {
	*lines = append(*lines, CheckLine{OK: ok, Warn: warn, Msg: msg})
	if !ok && !warn {
		rep.Failures++
	}
}

func (e *Engine) checkServer(rep *CheckReport) {
	e.checkMedia(rep)

	wd := e.cfg.WorkdirPath()
	if err := writableDir(wd); err != nil {
		rep.add(&rep.Server, false, false, fmt.Sprintf("workdir %s not writable: %v", wd, err))
	} else {
		rep.add(&rep.Server, true, false, fmt.Sprintf("workdir %s writable", wd))
	}

	// The compressor is needed server-side for a server-side compress and for restore
	// decompression, so a missing binary is a real problem even with client-side dumps.
	if err := compress.Check(e.compressScheme, e.fopts); err != nil {
		rep.add(&rep.Server, false, false, fmt.Sprintf("compression %q: %v", e.compressScheme, err))
	} else {
		rep.add(&rep.Server, true, false, fmt.Sprintf("compression %q available", e.compressScheme))
	}

	checked := map[string]bool{}
	for _, d := range e.cfg.DLEs() {
		dt := d.DumpTypeName()
		if checked[dt] {
			continue
		}
		checked[dt] = true
		scheme, opts := e.encryptionFor(dt)
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
func (e *Engine) checkMedia(rep *CheckReport) {
	landing := e.Landing()
	names := make([]string, 0, len(e.cfg.Media))
	for n := range e.cfg.Media {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		isLanding := name == landing
		label := fmt.Sprintf("medium %q", name)
		if isLanding {
			label = fmt.Sprintf("landing medium %q", name)
		}
		if e.cfg.Media[name].Type == "cloud" {
			rep.add(&rep.Server, false, true, fmt.Sprintf("%s (cloud) configured — reachability checked at first use, not here", label))
			continue
		}
		if _, _, _, err := e.mediumVolume(name); err != nil {
			rep.add(&rep.Server, false, !isLanding, fmt.Sprintf("%s not ready: %v", label, err))
		} else {
			rep.add(&rep.Server, true, false, fmt.Sprintf("%s ready", label))
		}
	}
}

func (e *Engine) checkHost(rep *CheckReport, host string, connect bool) HostCheck {
	ssh, remote := e.cfg.RemoteHost(host)
	hc := HostCheck{Host: host, Remote: remote}
	if remote {
		hc.Target = sshTarget(host, ssh)
		if !connect {
			rep.add(&hc.Lines, false, true, "remote — not probed (drop --offline to connect)")
			return hc
		}
	}

	ex := e.executorFor(host) // Local() for a local host, SSH for a remote one
	if remote {
		if err := ex.Command("true").Run(); err != nil {
			rep.add(&hc.Lines, false, false, fmt.Sprintf("unreachable over SSH: %v", err))
			return hc // nothing else is probeable
		}
		rep.add(&hc.Lines, true, false, "reachable over SSH")
	}

	dles := e.dlesForHost(host)
	seen := map[string]bool{}
	for _, d := range dles {
		dt := d.DumpTypeName()
		if seen[dt] {
			continue
		}
		seen[dt] = true
		arch, err := e.archiverFor(dt, host)
		if err == nil {
			err = arch.Check()
		}
		if err != nil {
			rep.add(&hc.Lines, false, false, fmt.Sprintf("GNU tar (dumptype %q): %v", dt, err))
		} else {
			rep.add(&hc.Lines, true, false, fmt.Sprintf("GNU tar present (dumptype %q)", dt))
		}
		e.checkClientTools(rep, &hc, ex, dt)
	}

	for _, d := range dles {
		if err := ex.Command("test", "-r", d.Path).Run(); err != nil {
			rep.add(&hc.Lines, false, false, fmt.Sprintf("source %s not readable", d.Path))
		} else {
			rep.add(&hc.Lines, true, false, fmt.Sprintf("source %s readable", d.Path))
		}
	}

	// The incremental-state library lives on the host where the archiver runs (the client
	// for a remote DLE, the server for a local one) and is now distinct from the catalog
	// workdir, so verify it for every host.
	stateDir := e.cfg.StateDirFor(host)
	if err := ex.MkdirAll(stateDir); err != nil {
		rep.add(&hc.Lines, false, false, fmt.Sprintf("state_dir %s not creatable: %v", stateDir, err))
	} else {
		rep.add(&hc.Lines, true, false, fmt.Sprintf("state_dir %s writable", stateDir))
	}
	return hc
}

// checkClientTools probes the compressor / gpg on the host when a dumptype runs them there
// (compress/encrypt: client). For a server-side transform there is nothing to check here —
// the server-side tools are covered in checkServer.
func (e *Engine) checkClientTools(rep *CheckReport, hc *HostCheck, ex programs.Executor, dt string) {
	if e.cfg.ResolveDumpType(dt).Compress == "client" {
		if cmd, ok, err := compress.CompressCmd(e.compressScheme, e.fopts); err == nil && ok {
			e.probeTool(rep, hc, ex, cmd.Name, "compressor")
		}
	}
	if e.cfg.EncryptionFor(dt).At == "client" {
		scheme, opts := e.encryptionFor(dt)
		if cmd, ok, err := crypt.EncryptCmd(scheme, opts); err == nil && ok {
			e.probeTool(rep, hc, ex, cmd.Name, "encryptor")
		}
	}
}

func (e *Engine) probeTool(rep *CheckReport, hc *HostCheck, ex programs.Executor, bin, role string) {
	if err := ex.Command(bin, "--version").Run(); err != nil {
		rep.add(&hc.Lines, false, false, fmt.Sprintf("client %s %q: %v", role, bin, err))
	} else {
		rep.add(&hc.Lines, true, false, fmt.Sprintf("client %s %q present", role, bin))
	}
}

// dleHostsInOrder returns the distinct DLE hosts in config order.
func (e *Engine) dleHostsInOrder() []string {
	var order []string
	seen := map[string]bool{}
	for _, d := range e.cfg.DLEs() {
		if !seen[d.Host] {
			seen[d.Host] = true
			order = append(order, d.Host)
		}
	}
	return order
}

// dlesForHost returns the DLEs on a host, in config order.
func (e *Engine) dlesForHost(host string) []config.DLE {
	var out []config.DLE
	for _, d := range e.cfg.DLEs() {
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
