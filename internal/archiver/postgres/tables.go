package postgres

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Niloen/nbackup/internal/record"
)

// This file builds the archive's content inventory (record.Unit): one unit per
// table, "table.<db>.<schema>.<table>" (kind-first dotted name; matviews mint
// "matview.…"), captured at dump time — the mapping
// (relfilenode → name) is cluster state that dies with the cluster, so it can
// never be recomputed later. Unit.Size is pg_table_size (heap + toast + fsm +
// vm, as of the dump — NOT this archive's delta bytes), and Unit.Members
// cross-references the raw members in this archive that carry the table's
// bytes: heap segments and forks, plus the TOAST relation and its index, so an
// expert's salvage selection is complete. pg_class is database-local, so the
// inventory costs one catalog query per connectable database, against the
// cluster being dumped.
//
// The inventory is best-effort: a database the backup role cannot enter, or an
// exotic conninfo form, just contributes no units — the archive itself is
// unaffected, `--inventory` merely knows less.

// unitsFor builds the inventory for one dump from the cluster's catalogs and
// the archive's member list. Returns nil when nothing could be mapped.
func (p *postgres) unitsFor(source string, members []record.Member) []record.Unit {
	out, err := p.psql(source, "SELECT oid, datname FROM pg_database WHERE datallowconn")
	if err != nil {
		return nil
	}
	type table struct {
		unit  record.Unit
		order int // first member's stream position, for deterministic-but-informative Members order
	}
	byFile := map[string]*table{} // "base/<dboid>/<relfilenode>" -> its table (heap, toast, and toast-index files all map here)
	var tables []*table
	for _, line := range strings.Split(out, "\n") {
		oid, db, ok := strings.Cut(line, "|")
		if !ok || strings.Contains(db, "/") {
			continue
		}
		conn, ok := connTo(source, db)
		if !ok {
			continue
		}
		if !safeIdent(db) {
			continue // the identity must split unambiguously; exotic names are skipped (best-effort)
		}
		// One row per table/matview: its kind, filenode, its TOAST relation's and
		// TOAST index's filenodes (0/empty when none), name, and total size.
		rels, err := p.psql(conn, `SELECT c.relkind, c.relfilenode, coalesce(tc.relfilenode, 0), coalesce(tic.relfilenode, 0), n.nspname, c.relname, pg_table_size(c.oid)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_class tc ON tc.oid = c.reltoastrelid
LEFT JOIN pg_index ti ON ti.indrelid = tc.oid
LEFT JOIN pg_class tic ON tic.oid = ti.indexrelid
WHERE c.relkind IN ('r','m') AND c.relfilenode <> 0`)
		if err != nil {
			continue
		}
		for _, rel := range strings.Split(rels, "\n") {
			parts := strings.SplitN(rel, "|", 7)
			if len(parts) != 7 || !safeIdent(parts[4]) || !safeIdent(parts[5]) {
				continue
			}
			kind := "table"
			if parts[0] == "m" {
				kind = "matview"
			}
			size, _ := strconv.ParseInt(parts[6], 10, 64)
			t := &table{unit: record.Unit{
				Path: kind + "." + db + "." + parts[4] + "." + parts[5],
				Size: size,
			}}
			tables = append(tables, t)
			for _, filenode := range parts[1:4] {
				if filenode != "" && filenode != "0" {
					byFile["base/"+oid+"/"+filenode] = t
				}
			}
		}
	}
	if len(byFile) == 0 {
		return nil
	}
	for i, m := range members {
		t, ok := byFile[relationKey(m.Path)]
		if !ok {
			continue
		}
		if len(t.unit.Members) == 0 {
			t.order = i
		}
		t.unit.Members = append(t.unit.Members, m.Path)
	}
	units := make([]record.Unit, 0, len(tables))
	for _, t := range tables {
		if len(t.unit.Members) == 0 {
			continue // a mapped table with no file in this archive (shouldn't occur: every dump enumerates all files)
		}
		units = append(units, t.unit)
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Path < units[j].Path })
	return units
}

// safeIdent gates which names may enter a unit identity: the dotted form
// "kind.db.schema.rel" must split unambiguously, and the identity becomes an
// export filename verbatim — so components carrying dots, slashes, spaces, or
// quoting are skipped (best-effort inventory; quoted exotic identifiers are
// vanishingly rare in practice).
func safeIdent(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '$', r == '-':
		default:
			return false
		}
	}
	return true
}

// connTo composes a connection reference to another database of the same
// cluster: a bare database name is swapped outright, a conninfo string gets a
// trailing dbname override (libpq: later keys win). A URL-form source is not
// composable — skip it (best-effort inventory).
func connTo(source, db string) (string, bool) {
	if strings.Contains(source, "://") {
		return "", false
	}
	if strings.Contains(source, "=") {
		return source + " dbname='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(db) + "'", true
	}
	return db, true
}

// relationKey reduces a stream member path to its owning relation's map key:
// "base/<dboid>/<relfilenode>" — stripping the INCREMENTAL delta marker, a
// fork suffix (_fsm/_vm/_init), and a 1GB-segment suffix (.<n>). A path that
// is not relation-file-shaped returns itself (and simply won't be in the map).
func relationKey(memberPath string) string {
	name, _ := assembler{}.Logical(memberPath)
	dir, file := splitLast(name)
	if i := strings.LastIndexByte(file, '.'); i >= 0 {
		if _, err := strconv.Atoi(file[i+1:]); err == nil {
			file = file[:i]
		}
	}
	if i := strings.LastIndexByte(file, '_'); i >= 0 {
		switch file[i+1:] {
		case "fsm", "vm", "init":
			file = file[:i]
		}
	}
	if dir == "" {
		return file
	}
	return dir + "/" + file
}

// splitLast splits a slash path into (dir, last) without the trailing slash.
func splitLast(p string) (string, string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}
