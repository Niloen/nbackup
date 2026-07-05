package postgres

import (
	"fmt"
	"path"
	"strings"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
)

// This file is the unit exporter: it turns a restored cluster tree into the
// per-table SQL a user can actually act on — boot a THROWAWAY postmaster on
// the restored data directory (a private socket, nothing listening), pg_dump
// each requested unit, tear everything down. The output is stock pg_dump
// plain SQL: inspectable, and its CREATE TABLE errors loudly if the table
// still exists — an accidental double-import cannot silently duplicate rows.
// nb's non-destructive contract holds: the scratch instance reads only the
// restored files, and importing the SQL is the operator's own act.
//
// The boot overrides are the safety story. The restored postgresql.conf is
// PROD's — left alone it could listen on prod's port, archive WAL via prod's
// archive_command, demand extensions this host lacks, or write a pid/log file
// at an absolute prod path. Every such knob is overridden for the throwaway
// boot; the data itself is untouched.

// Exporter: tables materialize as pg_dump SQL.
func (p *postgres) Exporter() archiver.Exporter { return exporter{p} }

type exporter struct{ p *postgres }

// Ext: exported units land as "<unit><ext>" — stock pg_dump plain SQL.
func (exporter) Ext() string { return ".sql" }

// Stage boots the restored tree once and dumps every requested unit. The trap
// tears the postmaster down (and removes the private socket dir) on every
// path, success or failure; the recovery the first boot runs (backup_label +
// the WAL fetched into the backup) is postgres's own, untouched.
//
// source is the DLE's libpq connection reference — used only for its `user=`,
// the role pg_dump connects AS. It matters because the throwaway cluster's
// roles are prod's (a base backup images pg_authid): the OS user running the
// restore is almost never one of them, so pg_dump's default (PGUSER → the OS
// user) fails with "role <os-user> does not exist". The role that took the
// backup is the right one to dump with, and it is guaranteed to exist here.
func (e exporter) Stage(dataDir, destDir, source string, units []string) programs.Cmd {
	overrides := strings.Join([]string{
		`-c listen_addresses=''`,          // sockets only — nothing reachable from outside
		`-c unix_socket_directories='$d'`, // the private socket dir minted below
		"-c port=5433",                    // fixed, known to pg_dump below; prod's conf may set anything
		"-c shared_preload_libraries=''",  // prod's extensions need not exist on this host
		"-c archive_mode=off",             // NEVER run prod's archive_command from a scratch boot
		"-c logging_collector=off",        // no absolute prod log_directory writes; stderr → the -l file
		"-c external_pid_file=''",         // no absolute prod pid path writes
		`-c hba_file='$d/pg_hba.conf'`,    // trust the private socket; prod's hba (peer/md5) can't block a disposable boot
	}, " ")
	// userFlag connects pg_dump as the DLE's configured role. Empty (no `user=`
	// in the source, e.g. a bare dbname or a service= reference) leaves pg_dump's
	// default — the OS user — which is correct when the restore runs as that role.
	userFlag := ""
	if u := connUser(source); u != "" {
		userFlag = "-U " + shQuote(u) + " "
	}
	lines := []string{
		"set -e",
		fmt.Sprintf("chmod 0700 %s", shQuote(dataDir)), // postgres refuses group/world-accessible data dirs
		`d=$(mktemp -d)`,
		// A trust-only hba for the private socket dir: this instance is disposable,
		// unreachable, and read-only to the operator, so prod's authentication (peer
		// mapping to a missing OS role, md5 with no password) must not gate the dump.
		`printf 'local all all trust\n' > "$d/pg_hba.conf"`,
		fmt.Sprintf(`cleanup() { %s -D %s -m immediate stop >/dev/null 2>&1 || true; rm -rf "$d"; }`,
			shQuote(e.p.bin("pg_ctl")), shQuote(dataDir)),
		"trap cleanup EXIT",
		fmt.Sprintf(`%s -D %s -w -l %s start -o "%s"`,
			shQuote(e.p.bin("pg_ctl")), shQuote(dataDir), shQuote(path.Join(dataDir, "nb-export.log")), overrides),
	}
	for _, unit := range units {
		kind, db, schema, rel, err := splitUnit(unit)
		if err != nil {
			// A malformed identity is a caller bug (units come from the recorded
			// inventory); fail the stage loudly rather than dump the wrong thing.
			return programs.Cmd{Name: "sh", Args: []string{"-c", fmt.Sprintf("echo %s >&2; exit 2", shQuote(err.Error()))}}
		}
		_ = kind // table and matview both dump via -t (a matview exports its definition; its data is REFRESHed on import)
		lines = append(lines, fmt.Sprintf(`%s -h "$d" -p 5433 %s-d %s -t %s -f %s`,
			shQuote(e.p.bin("pg_dump")), userFlag, shQuote(db), shQuote(schema+"."+rel), shQuote(path.Join(destDir, unit+".sql"))))
	}
	lines = append(lines, fmt.Sprintf("%s -D %s stop >/dev/null", shQuote(e.p.bin("pg_ctl")), shQuote(dataDir)))
	return programs.Cmd{Name: "bash", Args: []string{"-c", strings.Join(lines, "\n")}}
}

// connUser extracts the role a libpq connection reference authenticates as: the
// `user=` keyword of a conninfo string, or the userinfo of a postgres:// URI.
// "" when neither is present (a bare dbname, or a service= reference whose user
// lives in ~/.pg_service.conf) — the caller then leaves pg_dump's default.
func connUser(source string) string {
	source = strings.TrimSpace(source)
	if u, ok := uriUser(source); ok {
		return u
	}
	// Keyword/value conninfo: scan for a `user` keyword. Values here are the
	// unquoted common case (nb's own DLE strings); quoted/escaped values are a
	// libpq nicety this best-effort parse need not reproduce.
	for _, tok := range strings.Fields(source) {
		if v, ok := strings.CutPrefix(tok, "user="); ok {
			return v
		}
	}
	return ""
}

// uriUser returns the userinfo of a postgres:// or postgresql:// URI (the
// part before '@' in the authority), or ("", false) when source is not such a
// URI or carries no userinfo.
func uriUser(source string) (string, bool) {
	for _, scheme := range []string{"postgresql://", "postgres://"} {
		if rest, ok := strings.CutPrefix(source, scheme); ok {
			authority, _, _ := strings.Cut(rest, "/")
			if userinfo, _, hasAt := strings.Cut(authority, "@"); hasAt {
				user, _, _ := strings.Cut(userinfo, ":") // drop any password
				return user, user != ""
			}
			return "", false
		}
	}
	return "", false
}

// splitUnit parses a unit identity this archiver minted: "kind.db.schema.rel",
// unambiguous because unitsFor admits only dot-free components (safeIdent).
func splitUnit(unit string) (kind, db, schema, rel string, err error) {
	parts := strings.Split(unit, ".")
	if len(parts) != 4 || (parts[0] != "table" && parts[0] != "matview") {
		return "", "", "", "", fmt.Errorf("malformed postgres unit identity %q (want kind.db.schema.rel)", unit)
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}
