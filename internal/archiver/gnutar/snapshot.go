package gnutar

import (
	"fmt"
	"path/filepath"
	"strings"

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

// carvesPath is the live snapshot's sidecar: the carve set (anchored leading-"/" subtree
// excludes) the dump that produced L<level>.snar ran with, newline-separated. It exists
// because tar records an excluded-but-on-disk subtree as "present, not dumped" — never a
// deletion — so a base built with FEWER carves than the next dump wants still contains a
// subtree that now belongs to another DLE, and an incremental on it would retain that
// stale copy. The sidecar is what lets HasBase judge that, entirely inside gnutar.
func (g *gnutar) carvesPath(dle string, level int) string {
	return filepath.Join(g.stateDir, dle, fmt.Sprintf("L%d.carves", level))
}

// carvesOf extracts the anchored subtree carves ("./"-prefixed patterns — a partition
// remainder's system carves and user-written anchored excludes alike) from an exclude
// list, leaving content globs ("*.log") behind: glob edits keep the Amanda stance (no
// forced full); anchored additions re-baseline, because either kind leaves a stale
// subtree copy in the chain.
func carvesOf(exclude []string) []string {
	var out []string
	for _, p := range exclude {
		if strings.HasPrefix(p, "./") {
			out = append(out, p)
		}
	}
	return out
}

// HasBase reports whether the live snapshot left by a completed dump at the level is a
// usable base for a higher incremental covering s. A missing file — or a present but
// empty one, which a killed dump can leave behind — is not usable. A base is also
// unusable when s carves out a subtree the base's dump did NOT (the sidecar comparison
// above): the engine then forces a full, re-baselining the partition remainder after a
// child graduates. Carves REMOVED since the base are fine — an un-excluded subtree
// re-enters the chain wholesale on the next incremental (pinned by
// TestUnexcludedSubtreeReentersChainWholesale) — so the test is subset, not equality,
// and a carve-free request (every plain DLE) skips the sidecar read entirely.
func (g *gnutar) HasBase(dle string, level int, s archiver.Scope) bool {
	n, err := g.ex.Size(g.snapPath(dle, level))
	if err != nil || n == 0 {
		return false
	}
	want := carvesOf(s.Exclude)
	if len(want) == 0 {
		return true
	}
	data, err := g.ex.ReadFile(g.carvesPath(dle, level))
	if err != nil {
		return false // carves wanted but none recorded (or unreadable): re-baseline
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			have[line] = true
		}
	}
	for _, c := range want {
		if !have[c] {
			return false // a subtree newly carved out: the base still contains it
		}
	}
	return true
}

// recordCarves commits the carve set a just-promoted dump ran with, beside its snapshot
// (write ".new", then rename — the library's promote pattern). Always written, even
// empty, so the recorded state is current rather than inherited from an older level.
func (g *gnutar) recordCarves(dle string, level int, carves []string) error {
	work := g.carvesPath(dle, level) + ".new"
	if err := g.ex.WriteFile(work, []byte(strings.Join(carves, "\n"))); err != nil {
		return err
	}
	return g.ex.Rename(work, g.carvesPath(dle, level))
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
