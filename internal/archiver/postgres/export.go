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
func (e exporter) Stage(dataDir, destDir string, units []string) programs.Cmd {
	overrides := strings.Join([]string{
		`-c listen_addresses=''`,          // sockets only — nothing reachable from outside
		`-c unix_socket_directories='$d'`, // the private socket dir minted below
		"-c port=5433",                    // fixed, known to pg_dump below; prod's conf may set anything
		"-c shared_preload_libraries=''",  // prod's extensions need not exist on this host
		"-c archive_mode=off",             // NEVER run prod's archive_command from a scratch boot
		"-c logging_collector=off",        // no absolute prod log_directory writes; stderr → the -l file
		"-c external_pid_file=''",         // no absolute prod pid path writes
	}, " ")
	lines := []string{
		"set -e",
		fmt.Sprintf("chmod 0700 %s", shQuote(dataDir)), // postgres refuses group/world-accessible data dirs
		`d=$(mktemp -d)`,
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
		lines = append(lines, fmt.Sprintf(`%s -h "$d" -p 5433 -d %s -t %s -f %s`,
			shQuote(e.p.bin("pg_dump")), shQuote(db), shQuote(schema+"."+rel), shQuote(path.Join(destDir, unit+".sql"))))
	}
	lines = append(lines, fmt.Sprintf("%s -D %s stop >/dev/null", shQuote(e.p.bin("pg_ctl")), shQuote(dataDir)))
	return programs.Cmd{Name: "bash", Args: []string{"-c", strings.Join(lines, "\n")}}
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
