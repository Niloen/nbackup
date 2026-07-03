package scheduler

import (
	"fmt"

	"github.com/Niloen/nbackup/internal/config"
)

// PreflightDeps are the resolution closures the dump pre-flight runs over. The
// engine wires one value and hands it to both callers — the scheduler's preview
// (Validate, via Deps) and the conductor's real run — so the two checks cannot
// drift.
type PreflightDeps struct {
	CheckCompress     func() error
	PreflightDumptype func(dt, host string, checkArchiver bool, checked map[string]bool) error
	RemoteHost        func(host string) (config.SSHConfig, bool)
	StatSource        func(path string) error
	ProbeReachable    func(host string) error
}

// Preflight checks each DLE the way a run resolves it: the compression scheme,
// then every dumptype's method and encryption scheme, then the source host. The
// scheme checks are always fatal (an unrunnable config); strict selects the two
// policies around them:
//
//   - strict=false (preview: `nb plan` / `nb dump --dry-run`): archiver binaries are
//     not resolved, and a missing local source path or an unreachable remote host is
//     a returned warning, not an error — it may be an unmounted volume or a down
//     client that will be back for the real run, and the preview should still show
//     the rest of the plan (though the affected estimate reads ~0 B).
//   - strict=true (a real dump, just before the run is created): every source host
//     must answer (probed once per host) and every archiver binary must resolve —
//     resolving them also populates the archiver cache, so the parallel dump
//     workers only read it. Nothing is stat'd (the dump itself reads the source),
//     and any failure is fatal. Warnings are always nil.
func Preflight(d PreflightDeps, dles []config.DLE, strict bool) (warnings []string, err error) {
	if err := d.CheckCompress(); err != nil {
		return nil, err
	}
	checkedEnc := map[string]bool{}
	probed := map[string]bool{}
	for _, dle := range dles {
		if strict {
			if !probed[dle.Host] {
				probed[dle.Host] = true
				if err := d.ProbeReachable(dle.Host); err != nil {
					return nil, err
				}
			}
			if err := d.PreflightDumptype(dle.DumpTypeName(), dle.Host, true, checkedEnc); err != nil {
				return nil, err
			}
			continue
		}
		if err := d.PreflightDumptype(dle.DumpTypeName(), dle.Host, false, checkedEnc); err != nil {
			return nil, err
		}
		// Only a local source can be stat'd here; a remote DLE's path lives on the
		// client. A remote host is probed over SSH (once per host) so an unreachable
		// client warns here rather than silently estimating ~0 B — the misleading
		// "healthy" plan `nb check` would otherwise be the only thing to catch.
		if _, remote := d.RemoteHost(dle.Host); !remote {
			if err := d.StatSource(dle.Path); err != nil {
				warnings = append(warnings, fmt.Sprintf("DLE %s: source path %s is missing or unreadable (%v) — the real run will fail unless it becomes available", dle.ID(), dle.Path, err))
			}
		} else if !probed[dle.Host] {
			probed[dle.Host] = true
			if err := d.ProbeReachable(dle.Host); err != nil {
				warnings = append(warnings, fmt.Sprintf("%v — its DLEs cannot be estimated until it is reachable (shown as ~0 B)", err))
			}
		}
	}
	return warnings, nil
}

// Validate checks each DLE the way a real run would resolve it, so a preview
// (`nb plan` / `nb dump --dry-run`) surfaces problems the size estimates would
// otherwise swallow into a misleading ~0 B. It is Preflight in its non-strict
// mode: an unrunnable config is a fatal error, while source paths and hosts that
// are unavailable right now are non-fatal warnings, so a preview no longer gives
// a green light to a run that `nb dump` will reject.
func (s *Scheduler) Validate() (warnings []string, err error) {
	return Preflight(s.d.PreflightDeps, s.d.DLEs(), false)
}
