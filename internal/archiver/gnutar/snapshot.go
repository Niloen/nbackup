package gnutar

import (
	"fmt"
	"path/filepath"

	"github.com/Niloen/nbackup/internal/archiver"
)

// This file is gnutar's incremental-state library: the per-DLE, per-level GNU tar
// listed-incremental snapshot (.snar) files under stateDir, plus the ".new"-then-rename
// promotion that keeps a killed dump from corrupting the base a retry builds on. The
// generic archiver layer never names a snapshot — it speaks only DLE/Level/BaseLevel and
// HasBase (see ARCHITECTURE.md, "Incremental state belongs to the archiver").
//
// Two files back one (DLE, level): the *live snapshot* (snapPath) — the committed base a
// retry or a higher incremental reads — and the *work snapshot* (workSnap, Amanda's
// ".new") that the in-flight dump writes to. seedSnapshot stages the work snapshot,
// HasBase reports whether a live base is usable, and promoteSnapshot renames work over
// live once the archive has durably committed.

// snapPath is the live snapshot for a DLE's level within the library (on the executor's
// host) — the committed base a retry or a higher incremental reads.
func (g *gnutar) snapPath(dle string, level int) string {
	return filepath.Join(g.stateDir, dle, fmt.Sprintf("L%d.snar", level))
}

// workSnap is the work snapshot: the side file (Amanda's ".new") a dump writes its new
// snapshot to. tar updates it in place; only a committed dump promotes it over the live
// snapPath, so a killed dump leaves the committed base untouched.
func (g *gnutar) workSnap(dle string, level int) string {
	return g.snapPath(dle, level) + ".new"
}

// HasBase reports whether the live snapshot left by a completed dump at the level is a
// usable base for a higher incremental. A missing file — or a present but empty one, which
// a killed dump can leave behind — is not usable, so it reports false and the engine
// forces a full instead of building a full-sized incremental on a dead snapshot.
func (g *gnutar) HasBase(dle string, level int) bool {
	n, err := g.ex.Size(g.snapPath(dle, level))
	return err == nil && n > 0
}

// seedSnapshot prepares outSnap as the starting incremental state for the dump on the
// executor's host: a copy of the base level's live snapshot for an incremental (so the
// committed base is never mutated — tar updates outSnap in place), or an absent file for a
// full so tar starts fresh. The caller passes either the work snapshot (a real dump) or a
// throwaway temp file (an estimate), so the live library is touched only on promotion.
func (g *gnutar) seedSnapshot(r archiver.BackupRequest, outSnap string) error {
	if err := g.ex.MkdirAll(filepath.Dir(outSnap)); err != nil {
		return err
	}
	if r.Level == 0 || r.BaseLevel < 0 {
		return g.ex.Remove(outSnap) // Remove treats an absent file as success
	}
	return g.ex.CopyFile(g.snapPath(r.DLE, r.BaseLevel), outSnap)
}

// promoteSnapshot commits a dump's work snapshot into the library by renaming it over the
// live snapshot — the rename half of the ".new"-then-promote pattern. The caller invokes
// it only once the archive is durably committed, so a failed dump never advances the base.
func (g *gnutar) promoteSnapshot(dle string, level int) error {
	return g.ex.Rename(g.workSnap(dle, level), g.snapPath(dle, level))
}
