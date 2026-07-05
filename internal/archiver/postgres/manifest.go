package postgres

import (
	"fmt"
	"path/filepath"
)

// This file is the postgres archiver's incremental-state library: the per-DLE,
// per-level backup manifests under stateRoot, plus the ".new"-then-rename
// promotion that keeps a killed dump from corrupting the base a retry builds
// on — the exact shape of gnutar's .snar library (see gnutar/snapshot.go),
// minus the seed step: pg_basebackup only READS the base manifest
// (--incremental), so nothing is copied before a dump.
//
// Two files back one (DLE, level): the *live manifest* (manifestPath) — the
// committed base the next incremental builds on — and the *work manifest*
// (workManifest) that the in-flight dump tees out of its own stream.
// promoteManifest renames work over live only once the archive has durably
// committed.

// manifestPath is the live manifest for a DLE's level within the library (on
// the executor's host) — the committed base a level+1 dump builds on.
func (p *postgres) manifestPath(dle string, level int) string {
	return filepath.Join(p.stateRoot, dle, fmt.Sprintf("L%d.manifest", level))
}

// workManifest is the side file a dump tees this backup's manifest to; only a
// committed dump promotes it over the live manifestPath.
func (p *postgres) workManifest(dle string, level int) string {
	return p.manifestPath(dle, level) + ".new"
}

// HasBase reports whether the live manifest left by a completed dump at the
// level is a usable base for a higher incremental. Missing or empty (a killed
// dump's residue) is not usable, so the engine forces a full instead.
func (p *postgres) HasBase(dle string, level int) bool {
	n, err := p.ex.Size(p.manifestPath(dle, level))
	return err == nil && n > 0
}

// promoteManifest commits a dump's work manifest into the library by renaming
// it over the live one — invoked by the caller only once the archive is
// durably committed, so a failed dump never advances the base.
func (p *postgres) promoteManifest(dle string, level int) error {
	return p.ex.Rename(p.workManifest(dle, level), p.manifestPath(dle, level))
}
