package postgres

import (
	"strings"
	"testing"
)

func TestSplitUnit(t *testing.T) {
	kind, db, schema, rel, err := splitUnit("table.postgres.public.users")
	if err != nil || kind != "table" || db != "postgres" || schema != "public" || rel != "users" {
		t.Fatalf("splitUnit = %q %q %q %q, %v", kind, db, schema, rel, err)
	}
	if _, _, _, _, err := splitUnit("matview.app.public.daily"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"table.postgres.users", "sequence.a.b.c", "table.a.b.c.d", "users"} {
		if _, _, _, _, err := splitUnit(bad); err == nil {
			t.Errorf("splitUnit(%q) should fail", bad)
		}
	}
}

// TestExportStageScript pins the safety story: every prod-config knob that
// could reach outside the scratch boot is overridden, the postmaster is torn
// down on every path, and each unit lands as <identity>.sql.
func TestExportStageScript(t *testing.T) {
	p := open(t, "/opt/pg/bin", t.TempDir())
	exp := p.Exporter()
	if exp == nil || exp.Ext() != ".sql" {
		t.Fatalf("exporter = %v", exp)
	}
	cmd := exp.Stage("/scratch/data", "/out", []string{"table.postgres.public.users", "table.postgres.public.orders"})
	script := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"chmod 0700 '/scratch/data'",
		`listen_addresses=''`,
		"shared_preload_libraries=''",
		"archive_mode=off",
		"logging_collector=off",
		"external_pid_file=''",
		"trap cleanup EXIT",
		"'/opt/pg/bin/pg_ctl' -D '/scratch/data' -w -l",
		`'/opt/pg/bin/pg_dump' -h "$d" -p 5433 -d 'postgres' -t 'public.users' -f '/out/table.postgres.public.users.sql'`,
		`-t 'public.orders' -f '/out/table.postgres.public.orders.sql'`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("export script lacks %q:\n%s", want, script)
		}
	}
	// A malformed identity must fail the stage, not dump the wrong thing.
	bad := exp.Stage("/scratch/data", "/out", []string{"nonsense"})
	if !strings.Contains(strings.Join(bad.Args, " "), "exit 2") {
		t.Fatal("malformed unit must produce a failing stage")
	}
}

// TestSafeIdent pins the identity gate: only dot-free, shell-and-filename-safe
// components mint units (the dotted identity must split unambiguously).
func TestSafeIdent(t *testing.T) {
	for _, ok := range []string{"users", "Users_2", "a$b", "app-prod"} {
		if !safeIdent(ok) {
			t.Errorf("safeIdent(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "a.b", "a b", `a"b`, "a/b", "a'b"} {
		if safeIdent(bad) {
			t.Errorf("safeIdent(%q) = true", bad)
		}
	}
}
