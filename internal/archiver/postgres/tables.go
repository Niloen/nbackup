package postgres

import (
	"strconv"
	"strings"

	"github.com/Niloen/nbackup/internal/record"
)

// This file captures the table-name aliases at dump time: one catalog pass
// maps each relation file in the stream ("base/<dboid>/<relfilenode>[_fork]
// [.seg]") to a human-meaningful browse path ("tables/<db>/<schema>.<table>/
// data"), recorded as Member.Alias. The browse tree grafts aliases as
// symlinks, so a mounted backup answers "what tables are in here" by name.
// The mapping is queried per database because pg_class is database-local —
// N+1 psql calls against the cluster being dumped, so the names are exactly
// as of the backup.
//
// Annotation is best-effort: a database that cannot be reached (datallowconn
// on a db the backup role cannot enter, an exotic conninfo form) just
// contributes no aliases — the physical paths stay authoritative, browse is
// merely less friendly there.

// annotateTables fills Member.Alias for the relation files of every
// connectable database in the cluster.
func (p *postgres) annotateTables(source string, members []record.Member) {
	out, err := p.psql(source, "SELECT oid, datname FROM pg_database WHERE datallowconn")
	if err != nil {
		return
	}
	alias := map[string]string{} // "base/<dboid>/<relfilenode>" -> "tables/<db>/<schema>.<table>"
	for _, line := range strings.Split(out, "\n") {
		oid, db, ok := strings.Cut(line, "|")
		if !ok || strings.Contains(db, "/") {
			continue
		}
		conn, ok := connTo(source, db)
		if !ok {
			continue
		}
		rels, err := p.psql(conn, "SELECT c.relfilenode, n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r','m') AND c.relfilenode <> 0")
		if err != nil {
			continue
		}
		for _, rel := range strings.Split(rels, "\n") {
			parts := strings.SplitN(rel, "|", 3)
			if len(parts) != 3 || strings.ContainsAny(parts[1]+parts[2], "/") {
				continue
			}
			alias["base/"+oid+"/"+parts[0]] = "tables/" + db + "/" + parts[1] + "." + parts[2]
		}
	}
	if len(alias) == 0 {
		return
	}
	for i, m := range members {
		if table, fork, ok := relationFile(m.Path, alias); ok {
			members[i].Alias = table + "/" + fork
		}
	}
}

// connTo composes a connection reference to another database of the same
// cluster: a bare database name is swapped outright, a conninfo string gets a
// trailing dbname override (libpq: later keys win). A URL-form source is not
// composable — skip it (best-effort annotation).
func connTo(source, db string) (string, bool) {
	if strings.Contains(source, "://") {
		return "", false
	}
	if strings.Contains(source, "=") {
		return source + " dbname='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(db) + "'", true
	}
	return db, true
}

// relationFile matches a stream member path against the relation-file naming
// ("base/<dboid>/<relfilenode>[_<fork>][.<segment>]", with a possible
// INCREMENTAL. delta marker) and returns the table's alias prefix plus the
// fork file name inside the table's directory: the main fork is "data"
// ("data.1", … for 1GB segments), the others keep their PostgreSQL fork names
// (fsm, vm, init).
func relationFile(memberPath string, alias map[string]string) (table, fork string, ok bool) {
	name, isDelta := assembler{}.Logical(memberPath)
	_ = isDelta // a delta aliases the same logical file
	dir, file := splitLast(name)
	base := file
	seg := ""
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		if _, err := strconv.Atoi(base[i+1:]); err == nil {
			base, seg = base[:i], base[i+1:]
		}
	}
	forkName := "data"
	if i := strings.LastIndexByte(base, '_'); i >= 0 {
		switch base[i+1:] {
		case "fsm", "vm", "init":
			base, forkName = base[:i], base[i+1:]
		}
	}
	table, ok = alias[dir+"/"+base]
	if !ok {
		return "", "", false
	}
	if seg != "" {
		forkName += "." + seg
	}
	return table, forkName, true
}

// splitLast splits a slash path into (dir, last) without the trailing slash.
func splitLast(p string) (string, string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}
