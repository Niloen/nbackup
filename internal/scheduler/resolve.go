// This file is the impure half of source resolution: expanding the configured sources
// into the concrete units the (pure) planner schedules — the same driver role the
// scheduler plays for estimates. Only the live-acting commands run it.

package scheduler

import (
	"fmt"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/planner"
)

// Expander is the one archiver capability Resolve needs (satisfied by archiver.Archiver).
type Expander interface {
	Expand(p archiver.SourcePattern) ([]archiver.Scope, error)
}

// ExpanderFor resolves the archiver that expands (and later dumps) a dumptype's sources
// on a host.
type ExpanderFor func(dumptype, host string) (Expander, error)

// PatternOf is the one mapping from a config source to the archiver's expansion input —
// shared by Resolve and the live probers (`nb check`) so the two cannot drift: the
// mapping form hands its base explicitly; a scalar's whole source is the pattern.
func PatternOf(s config.DLE, excl []string) archiver.SourcePattern {
	sp := archiver.SourcePattern{Pattern: s.Path, Exclude: excl}
	if s.Partition != "" {
		sp.Base, sp.Pattern = s.Path, s.Partition
	}
	return sp
}

// SourceFailure is one source whose enumeration could not run — the failure ladder's
// unit class, per source and ATOMIC: the source contributes no units at all this run
// (matches and remainder drop together, so the rest can never balloon from a failed
// listing), the run proceeds for every other source, and the failure is reported like
// a dump failure. Never a guess: what to dump comes only from a successful listing.
type SourceFailure struct {
	Source config.DLE
	Err    error
}

// Resolve expands the configured sources into the concrete DLEs to schedule. A plain
// source resolves to itself with no I/O; a wildcard or partition enumerates over the
// archiver (its executor). Failures split by the ladder: an enumeration that cannot
// run fails THAT SOURCE (returned in failures — unit class), while a config-class
// problem — an unresolvable archiver definition, or two sources producing the same
// slug (one slug means one incremental-state chain and one catalog identity) — fails
// the whole resolution.
func Resolve(sources []config.DLE, archFor ExpanderFor, exclFor func(dumptype string) []string) ([]planner.DLE, []SourceFailure, error) {
	var out []planner.DLE
	var failures []SourceFailure
	origin := map[string]string{} // slug -> the source that produced it, for collision errors
	for _, s := range sources {
		dt := s.DumpTypeName()
		arch, err := archFor(dt, s.Host)
		if err != nil {
			return nil, nil, fmt.Errorf("source %s: %w", s.ID(), err)
		}
		scopes, err := arch.Expand(PatternOf(s, exclFor(dt)))
		if err != nil {
			failures = append(failures, SourceFailure{Source: s, Err: err})
			continue
		}
		for _, sc := range scopes {
			d := planner.DLE{Scope: sc, Host: s.Host, DumpType: dt, Origin: s.ID()}
			name := d.Name()
			if prev, dup := origin[name]; dup {
				return nil, nil, fmt.Errorf("resolved DLE %q is produced by both %s and %s — one slug means one state chain, so restructure the sources to give it a single owner (e.g. narrow the partition pattern)", name, prev, s.ID())
			}
			origin[name] = s.ID()
			out = append(out, d)
		}
	}
	return out, failures, nil
}
