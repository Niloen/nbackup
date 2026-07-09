// This file holds the resolved backup unit (DLE) and Resolve, the plan-time step that
// expands configured sources into the concrete units the planner schedules. A config.DLE
// is the declaration (possibly a wildcard or a partition); a planner.DLE is one unit the
// archiver resolved it into. Only the live-acting commands run Resolve — everything
// retrospective reads the DLE set from the catalog.

package planner

import (
	"fmt"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/config"
)

// DLE is the resolved backup unit: one concrete Scope (from Archiver.Expand) plus the
// host and dumptype it inherited from its config source. It is derived at plan time,
// never parsed from the file. Identity lives here, not on config.DLE, because only a
// concrete unit has a single name — a pattern declaration does not.
type DLE struct {
	archiver.Scope        // Base, Source, Exclude — complete and verbatim from Expand
	Host           string // which executor dumps it
	DumpType       string // resolved dumptype name (already defaulted), keys all per-DLE policy
}

// Name returns the unit's catalog/state slug — the same rule as a plain config.DLE's
// Name(), so plain sources keep their historical identity (see config.Slug).
func (d DLE) Name() string { return config.Slug(d.Host, d.Source) }

// ID returns the user-facing host:source identity, e.g. "app01:/home".
func (d DLE) ID() string { return d.Host + ":" + d.Source }

// DumpTypeName returns the resolved dumptype name (API peer of config.DLE.DumpTypeName).
func (d DLE) DumpTypeName() string { return d.DumpType }

// IsRest reports whether this unit is a partition's remainder — "the rest" of its base.
func (d DLE) IsRest() bool { return d.Base != "" && d.Source == d.Base }

// Expander is the one archiver capability Resolve needs (satisfied by archiver.Archiver).
type Expander interface {
	Expand(p archiver.SourcePattern) ([]archiver.Scope, error)
}

// ExpanderFor resolves the archiver that expands (and later dumps) a dumptype's sources
// on a host.
type ExpanderFor func(dumptype, host string) (Expander, error)

// Resolve expands the configured sources into the concrete DLEs to schedule. A plain
// source resolves to itself with no I/O; a wildcard or partition enumerates over the
// archiver (its executor). Failures are the source's failures — a listing that cannot
// run fails the plan, never degrades to a guess. Two sources resolving to the same slug
// is a hard error: one slug means one incremental-state chain and one catalog identity,
// so a collision (a partition match shadowing an explicit source, nested bases, an
// overlapping selection) must be restructured, not silently merged.
func Resolve(sources []config.DLE, archFor ExpanderFor, exclFor func(dumptype string) []string) ([]DLE, error) {
	var out []DLE
	origin := map[string]string{} // slug -> the source that produced it, for collision errors
	for _, s := range sources {
		dt := s.DumpTypeName()
		arch, err := archFor(dt, s.Host)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", s.ID(), err)
		}
		sp := archiver.SourcePattern{Pattern: s.Path, Exclude: exclFor(dt)}
		if s.Partition != "" {
			sp.Base, sp.Pattern = s.Path, s.Partition
		}
		scopes, err := arch.Expand(sp)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", s.ID(), err)
		}
		for _, sc := range scopes {
			d := DLE{Scope: sc, Host: s.Host, DumpType: dt}
			name := d.Name()
			if prev, dup := origin[name]; dup {
				return nil, fmt.Errorf("resolved DLE %q is produced by both %s and %s — one slug means one state chain, so restructure the sources to give it a single owner (e.g. narrow the partition pattern)", name, prev, s.ID())
			}
			origin[name] = s.ID()
			out = append(out, d)
		}
	}
	return out, nil
}
