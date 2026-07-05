package engine

import (
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
)

// dleDirectory is the engine's DLE identity service: it maps between a DLE's
// internal slug (the catalog/ledger key) and its user-facing host:path identity,
// drawing on both the config (the DLEs of this configuration) and the catalog (DLEs
// no longer configured but still recorded in runs). Every command that prints or
// parses a DLE name goes through here, so the two forms can't drift apart.
type dleDirectory struct {
	cfg *config.Config
	cat *catalog.Catalog
}

// names returns the distinct DLE slugs recorded across all catalog runs, sorted —
// the DLEs a recovery session can choose from.
func (d *dleDirectory) names() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range d.cat.Runs() {
		for _, a := range s.Archives {
			if !seen[a.DLE] {
				seen[a.DLE] = true
				out = append(out, a.DLE)
			}
		}
	}
	sort.Strings(out)
	return out
}

// displayMap maps each internal DLE slug to its host:path identity,
// drawing on both the config and the catalog (so a DLE no longer in the config still
// shows its real identity from the seal). The slug stays the internal key; host:path
// is the user-facing form.
func (d *dleDirectory) displayMap() map[string]string {
	m := map[string]string{}
	for _, dle := range d.cfg.DLEs() {
		m[dle.Name()] = dle.ID()
	}
	for _, s := range d.cat.Runs() {
		for _, a := range s.Archives {
			if a.Host == "" && a.Path == "" {
				continue
			}
			if _, ok := m[a.DLE]; !ok {
				m[a.DLE] = a.Host + ":" + a.Path
			}
		}
	}
	return m
}

// source maps an internal DLE slug to its raw source string (a path, or a libpq
// connection reference) as this configuration defines it. "" when the slug is
// not a configured DLE (an old run's DLE may be gone); the catalog records the
// source too, but only a configured DLE's source is authoritative for a live
// action like unit export, which needs the connection identity the config holds.
func (d *dleDirectory) source(slug string) string {
	for _, dle := range d.cfg.DLEs() {
		if dle.Name() == slug {
			return dle.Path
		}
	}
	return ""
}

// display maps an internal DLE slug to its host:path identity for messages,
// falling back to the slug when host/path are unknown.
func (d *dleDirectory) display(slug string) string {
	if id, ok := d.displayMap()[slug]; ok {
		return id
	}
	return slug
}

// displayAll returns the host:path identities of the DLEs a recovery session can
// choose from, sorted — the user-facing peer of names.
func (d *dleDirectory) displayAll() []string {
	disp := d.displayMap()
	var out []string
	for _, slug := range d.names() {
		if id, ok := disp[slug]; ok {
			out = append(out, id)
		} else {
			out = append(out, slug)
		}
	}
	sort.Strings(out)
	return out
}

// resolve maps a user-supplied DLE reference — a host:path identity or the raw
// internal slug — to the internal slug, or ("", false) if no catalog DLE matches.
// Trailing slashes are shell noise (tab completion appends them), so
// "host:/path/" matches the catalog's "host:/path"; a root path ("host:/") keeps
// its slash — it is the path, not a suffix.
func (d *dleDirectory) resolve(arg string) (string, bool) {
	arg = trimDLESlash(arg)
	disp := d.displayMap()
	for _, slug := range d.names() {
		if slug == arg || disp[slug] == arg {
			return slug, true
		}
	}
	return "", false
}

// trimDLESlash strips trailing slashes from a DLE reference, but never the slash
// of a root path ("/" or "host:/").
func trimDLESlash(arg string) string {
	for strings.HasSuffix(arg, "/") && arg != "/" && !strings.HasSuffix(arg, ":/") {
		arg = strings.TrimSuffix(arg, "/")
	}
	return arg
}

// resolveConfigured maps a DLE reference (slug or host:path identity) to its
// configured DLE — the lookup for operations that only make sense on a DLE this
// configuration still dumps (e.g. forcing a full).
func (d *dleDirectory) resolveConfigured(arg string) (config.DLE, bool) {
	for _, dle := range d.cfg.DLEs() {
		if dle.Name() == arg || dle.ID() == arg {
			return dle, true
		}
	}
	return config.DLE{}, false
}
