package catalog

// This file is the catalog's resolved-set record: which concrete DLEs the most recent
// run resolved its sources into, written once per run at plan-commit (before any byte
// moves, so a crashed run's intent still stands). It is what lets the retrospective
// surfaces answer questions config structurally cannot: a pattern source's children
// have no config slug, so their staleness tracking and landing-route judgment key off
// this record instead. Only the LATEST set is kept — every consumer wants current
// intent (a unit that stops being resolved is retired, exactly like a DLE removed from
// config) — and it is the third catalog-owned, non-rebuildable operational record
// (after forced-fulls and the usage ledger): `nb rebuild` cannot reconstruct intent
// from artifacts, so after a rebuild the surfaces degrade to config-derived answers
// until the next run records fresh intent.

// ResolvedDLE is one unit of a run's resolved backup set: its catalog slug, its
// display identity (a resolved-but-never-dumped unit has no archive to supply one),
// and the dumptype it was resolved under — the key to its landing route, which config
// cannot provide for a slug it does not contain.
type ResolvedDLE struct {
	DLE      string `json:"dle"`
	Host     string `json:"host,omitempty"`
	Source   string `json:"source,omitempty"`
	DumpType string `json:"dumptype,omitempty"`
	// Origin is the config source (its ID) the unit was resolved from. When a source
	// fails to enumerate on a later run, its previous units are carried forward into
	// that run's set BY ORIGIN — intent persists through the outage (staleness keeps
	// flagging, coverage keeps owing) even though nothing is dumped on a guess.
	Origin string `json:"origin,omitempty"`
}

// ResolvedSet is the latest run's resolved set, stamped with the run that recorded it.
type ResolvedSet struct {
	Run  string        `json:"run"`
	DLEs []ResolvedDLE `json:"dles"`
}

// RecordResolved stores the run's resolved DLE set as the latest (replacing the
// previous run's) and persists. Called once per run, after planning succeeds and
// before any dump starts — intent is recorded even if the run then crashes.
func (c *Catalog) RecordResolved(runID string, set []ResolvedDLE) error {
	c.resolved = &ResolvedSet{Run: runID, DLEs: set}
	return c.persist()
}

// LatestResolved returns the most recent run's recorded resolved set — nil when none
// exists (a catalog predating the record, or one rebuilt from media; consumers then
// fall back to config-derived answers).
func (c *Catalog) LatestResolved() []ResolvedDLE {
	if c.resolved == nil {
		return nil
	}
	return c.resolved.DLEs
}
