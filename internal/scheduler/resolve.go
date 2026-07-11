// This file is the impure half of source resolution: expanding the configured sources
// into the concrete units the (pure) planner schedules — the same driver role the
// scheduler plays for estimates. Only the live-acting commands run it.

package scheduler

import (
	"fmt"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
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

// DLESource is one of the scheduler's two online seams (the other is EstimateSource):
// it enumerates the plannable units and, when it can, refines their bases. The live
// source (liveDLEs) probes the archiver — enumerating over SSH/find and force-fulling a
// DLE whose incremental base is unusable (an archiver probe of the client's state). The
// catalog source (catalogDLEs) reads the recorded resolved set and touches no host, so
// it cannot refine bases (it has neither host access nor the Scope excludes HasBase
// needs) — RefineBases is a no-op. Base refinement rides here, not on a separate flag,
// because it is a capability of live resolution, not an independent choice.
type DLESource interface {
	Resolve() ([]planner.DLE, []SourceFailure, error)
	RefineBases(*planner.Plan)
}

// liveDLEs is the archiver-probing DLE source (the default): it enumerates over the
// archiver (SSH/find) and refines bases by probing the client's incremental state. It
// owns both algorithms and holds only the three Deps closures they need, so it stands
// alone — no hop back through the Scheduler.
type liveDLEs struct {
	dles        func() []config.DLE
	archiverFor func(dtName, host string) (archiver.Archiver, error)
	excludeFor  func(dtName string) []string
}

// Resolve expands the configured sources into the concrete DLEs to schedule (see the
// pure Resolve free function); a wildcard/partition enumerates over the archiver.
func (l liveDLEs) Resolve() ([]planner.DLE, []SourceFailure, error) {
	return Resolve(l.dles(),
		func(dt, host string) (Expander, error) { return l.archiverFor(dt, host) },
		l.excludeFor)
}

// RefineBases downgrades any planned incremental whose base incremental state is missing
// or unusable to a full, in place, with a warning. The planner picks the level from the
// catalog's run history, which can outlive the archiver's per-host state — a state_dir
// that moved, or a base a crashed dump never finished. Rather than fail the run (or,
// worse, dump a full-sized "incremental" onto a dead base), force a fresh full, the way
// Amanda falls back to level 0 when it can't find a usable base. A real run and a preview
// (`nb plan` / `--dry-run`) both go through here, so they agree.
func (l liveDLEs) RefineBases(plan *planner.Plan) {
	for i := range plan.Items {
		it := &plan.Items[i]
		if it.Level < 1 {
			continue
		}
		ar, err := l.archiverFor(it.DLE.DumpTypeName(), it.DLE.Host)
		if err != nil || ar.HasBase(it.Name, it.BaseLevel, it.DLE.Scope) {
			continue
		}
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"DLE %s: forcing a full (L0) — its L%d incremental base is not usable for this dump. Most often this is DELIBERATE: a subtree was newly carved out (a partition child graduated, or an anchored ./ exclude was added), which re-baselines once so the old chain's stale copy ages out. Otherwise the state is genuinely missing (an interrupted prior dump, or a moved state_dir)",
			it.DLE.ID(), it.BaseLevel))
		it.Level, it.BaseLevel, it.BaseRun = 0, -1, ""
		it.EstBytes = it.FullBytes
		it.Reason = "forced full: incremental base missing or unusable"
	}
}

// catalogDLEs is the offline DLE source: it reconstructs the set from the catalog's
// recorded resolved set and never touches a host, so it cannot refine bases. It holds
// only the two Deps closures it reads.
type catalogDLEs struct {
	resolvedSet func() []catalog.ResolvedDLE
	dles        func() []config.DLE
}

// Resolve rebuilds the plannable DLE set from the catalog's recorded resolved set
// (LatestResolved) — the concrete units the last run resolved its sources into, pattern
// children included — with NO archiver I/O (no SSH, no find), so the offline plan and the
// web ghost calendar never touch a host. The recorded set does not persist excludes/carves
// (see catalog/resolved.go), but the offline decision path never consumes them: history
// estimates key off the slug, the level comes from run history, and RefineBases is skipped
// offline. So the reconstructed Scope carries only Source and (for a remainder) Base.
//
// When no set was ever recorded (a fresh or `nb rebuild`-flattened catalog) it falls
// back to the configured SCALAR sources, which resolve to themselves with no I/O.
// Pattern sources cannot be enumerated without probing, so they are reported as source
// failures (the failure ladder's unit class) — the gap is visible, never guessed.
func (c catalogDLEs) Resolve() ([]planner.DLE, []SourceFailure, error) {
	if set := c.resolvedSet(); len(set) > 0 {
		out := make([]planner.DLE, 0, len(set))
		for _, r := range set {
			base := ""
			if r.Rest { // a remainder's Base equals its Source (see planner.DLE.IsRest)
				base = r.Source
			}
			out = append(out, planner.DLE{
				Scope:    archiver.Scope{Base: base, Source: r.Source},
				Host:     r.Host,
				DumpType: r.DumpType,
				Origin:   r.Origin,
			})
		}
		return out, nil, nil
	}
	var out []planner.DLE
	var failures []SourceFailure
	for _, src := range c.dles() {
		if src.Partition != "" {
			failures = append(failures, SourceFailure{Source: src, Err: fmt.Errorf(
				"pattern source cannot be enumerated offline (no resolved set recorded yet); run a live `nb plan` or a dump first")})
			continue
		}
		out = append(out, planner.DLE{
			Scope:    archiver.Scope{Source: src.Path},
			Host:     src.Host,
			DumpType: src.DumpTypeName(),
			Origin:   src.ID(),
		})
	}
	return out, failures, nil
}

func (catalogDLEs) RefineBases(*planner.Plan) {} // no host, no Scope: nothing to probe

func (s *Scheduler) liveDLEs() DLESource {
	return liveDLEs{dles: s.d.DLEs, archiverFor: s.d.ArchiverFor, excludeFor: s.d.ExcludeFor}
}
func (s *Scheduler) catalogDLEs() DLESource {
	return catalogDLEs{resolvedSet: s.d.ResolvedSet, dles: s.d.DLEs}
}
