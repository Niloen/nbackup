package scheduler

import "fmt"

// Validate checks each DLE the way a real run would resolve it, so a preview
// (`nb plan` / `nb dump --dry-run`) surfaces problems the size estimates would
// otherwise swallow into a misleading ~0 B. It runs the same pre-flight a real run
// does — the compression scheme and every dumptype's method and encryption scheme —
// returning a fatal error for an unrunnable config (an unknown compression/method/encryption scheme,
// a missing required key reference, or a scheme/gpg binary not on PATH), so a preview
// no longer gives a green light to a run that `nb dump` will reject. Source paths
// that are missing or unreadable right now are non-fatal warnings (they may be an
// unmounted volume the real run will mount).
func (s *Scheduler) Validate() (warnings []string, err error) {
	if err := s.d.CompressCheck(); err != nil {
		return nil, err
	}
	checkedEnc := map[string]bool{}
	hostProbed := map[string]bool{}
	for _, d := range s.d.DLEs() {
		if err := s.d.PreflightDumptype(d.DumpTypeName(), d.Host, false, checkedEnc); err != nil {
			return nil, err
		}
		// Only a local source can be stat'd here; a remote DLE's path lives on the
		// client. A remote host is probed over SSH (once per host) so an unreachable
		// client warns here rather than silently estimating ~0 B — the misleading
		// "healthy" plan `nb check` would otherwise be the only thing to catch.
		if _, remote := s.d.RemoteHost(d.Host); !remote {
			if err := s.d.StatSource(d.Path); err != nil {
				warnings = append(warnings, fmt.Sprintf("DLE %s: source path %s is missing or unreadable (%v) — the real run will fail unless it becomes available", d.ID(), d.Path, err))
			}
		} else if !hostProbed[d.Host] {
			hostProbed[d.Host] = true
			if err := s.d.ProbeReachable(d.Host); err != nil {
				warnings = append(warnings, fmt.Sprintf("%v — its DLEs cannot be estimated until it is reachable (shown as ~0 B)", err))
			}
		}
	}
	return warnings, nil
}
