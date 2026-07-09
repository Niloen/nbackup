// This file holds the resolved backup unit: the pure value type the planner schedules.
// A config.DLE is the declaration (possibly a wildcard or a partition); a planner.DLE is
// one unit the archiver resolved it into. Resolution itself — the impure, I/O-driving
// step — lives in the scheduler (the engine-side driver), keeping this package pure.

package planner

import (
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
	// Origin is the config source this unit was resolved from (its ID) — the key that
	// ties a unit back to its declaration when the declaration itself fails later (a
	// source whose enumeration errors carries its previously-resolved units forward
	// for staleness/coverage by Origin, without guessing what to dump).
	Origin string
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

// FailedUnit is a unit tonight's plan could not schedule — Amanda's "planner: FAILED":
// its source could not be enumerated, its estimate failed outright, or its host was
// unreachable at pre-flight. The failure ladder's unit class: the run proceeds without
// it, reports it FAILED (status/report/mail), and exits non-zero; the recording
// machinery keeps owing it (staleness, coverage) so the gap can never go quiet.
type FailedUnit struct {
	// DLE is the resolved unit when known (an estimate/pre-flight failure); the zero
	// value (Source == "") for a source that could not be enumerated at all — its
	// units are unknowable, so only Origin can carry the promise forward.
	DLE    DLE
	ID     string // display identity: the unit's host:source, or the failed source's config ID
	Origin string // the config source it came from — the carry-forward key
	Reason string
}
