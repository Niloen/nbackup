package catalog

// This file holds the catalog's one piece of non-media-derived state: per-DLE operator
// intent, today just the `nb reset` force-full directive. Unlike an Entry (rebuilt by
// scanning the media), a directive cannot be scanned back, so it lives in the cache file
// and is deliberately preserved across a Rebuild; a run consumes it. See the package doc.

// DLEMeta is the catalog's per-DLE operator/planner metadata, keyed by DLE slug. It is a
// struct, not a bare flag, so further per-DLE state can accrete here without reshaping the
// cache. Today it carries only ForceFull: the operator asked (via `nb reset`) that this DLE
// be fulled on its next run; a run consumes it.
type DLEMeta struct {
	ForceFull bool `json:"force_full,omitempty"`
}

// SetForceFull marks a DLE (by slug) to be fulled on its next run and persists. It is the
// store behind `nb reset`: the planner reads ForcedFulls and schedules a mandatory L0.
func (c *Catalog) SetForceFull(slug string) error {
	c.metaFor(slug).ForceFull = true
	return c.persist()
}

// ForcedFulls returns the set of DLE slugs currently flagged for a forced full.
func (c *Catalog) ForcedFulls() map[string]bool {
	out := map[string]bool{}
	for slug, m := range c.dles {
		if m.ForceFull {
			out[slug] = true
		}
	}
	return out
}

// ClearForceFulls drops the force-full flag for the given DLE slugs and persists — called
// once a run seals, having dumped every planned (hence every forced) DLE at L0.
func (c *Catalog) ClearForceFulls(slugs map[string]bool) error {
	changed := false
	for slug := range slugs {
		if m := c.dles[slug]; m != nil && m.ForceFull {
			m.ForceFull = false
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return c.persist()
}

// metaFor returns the DLE's metadata record, creating it on first use.
func (c *Catalog) metaFor(slug string) *DLEMeta {
	if c.dles == nil {
		c.dles = map[string]*DLEMeta{}
	}
	m := c.dles[slug]
	if m == nil {
		m = &DLEMeta{}
		c.dles[slug] = m
	}
	return m
}

// prunedDLEMeta drops zero-value records so the cache file carries only DLEs with live
// metadata (a consumed force-full leaves no residue).
func prunedDLEMeta(dles map[string]*DLEMeta) map[string]*DLEMeta {
	var out map[string]*DLEMeta
	for slug, m := range dles {
		if m == nil || *m == (DLEMeta{}) {
			continue
		}
		if out == nil {
			out = map[string]*DLEMeta{}
		}
		out[slug] = m
	}
	return out
}
