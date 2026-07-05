package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pg17Bin locates a PostgreSQL 17+ install carrying the server tools (the test
// runs a throwaway cluster), preferring the Debian versioned layout, or skips.
func pg17Bin(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{"/usr/lib/postgresql/17/bin", "/usr/lib/postgresql/18/bin"} {
		if _, err := os.Stat(filepath.Join(dir, "initdb")); err == nil {
			return dir
		}
	}
	t.Skip("PostgreSQL 17+ server not installed (apt install postgresql-17); skipping postgres CLI e2e")
	return ""
}

// startPG initdbs and starts a throwaway cluster on a unix socket with WAL
// summarization on, returning the conninfo. Stops with the test.
func startPG(t *testing.T, bin string) string {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	sock := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(filepath.Join(bin, name), args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("initdb", "-D", data, "--auth=trust", "--no-sync")
	f, err := os.OpenFile(filepath.Join(data, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\nsummarize_wal = on\nlisten_addresses = ''\nunix_socket_directories = '%s'\n", sock)
	f.Close()
	run("pg_ctl", "-D", data, "-l", filepath.Join(data, "log"), "-w", "start")
	t.Cleanup(func() { _ = exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-m", "immediate", "stop").Run() })
	return fmt.Sprintf("host=%s dbname=postgres", sock)
}

// writePostgresConfig writes a real config whose one dumptype uses the
// postgres archiver against the live throwaway cluster.
func writePostgresConfig(t *testing.T, bin, conninfo string) (cfgPath, base string) {
	t.Helper()
	base = t.TempDir()
	cfg := fmt.Sprintf(`
landing: disk
workdir: %s
state_dir: %s
compress:
  scheme: none
media:
  disk: { type: disk, path: %s }
archivers:
  pg:
    type: postgres
    bin_dir: %s
dumptypes:
  databases:
    archiver: pg
sources:
  databases:
    localhost: ["%s"]
`, filepath.Join(base, "catalog"), filepath.Join(base, "state"),
		filepath.Join(base, "runs"), bin, conninfo)
	cfgPath = filepath.Join(base, "nbackup.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, base
}

// TestPostgresArchiverEndToEnd drives the postgres archiver through the real
// CLI against a live PostgreSQL 17 cluster: check (connectivity +
// summarize_wal), a full dump, an incremental dump after data changed
// (HasBase promotes the manifest, the planner schedules L1), verify --deep
// (structural member compare over the raw tar listing), selected-file
// recovery of an assembled table file VIA ITS TABLE ALIAS, and a whole-DLE
// recover --all whose gather-then-combine output boots as a working cluster
// holding every row.
func TestPostgresArchiverEndToEnd(t *testing.T) {
	bin := pg17Bin(t)
	conninfo := startPG(t, bin)
	cfgPath, base := writePostgresConfig(t, bin, conninfo)
	dleID := "localhost:" + conninfo

	psql := func(sql string) string {
		t.Helper()
		out, err := exec.Command(filepath.Join(bin, "psql"), "-X", "-Atw", "-d", conninfo, "-c", sql).CombinedOutput()
		if err != nil {
			t.Fatalf("psql %q: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	psql("CREATE TABLE users (id int, name text)")
	psql("INSERT INTO users SELECT g, 'u' || g FROM generate_series(1, 1000) g")

	out, err := runCmd(t, "-c", cfgPath, "check")
	if err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}

	if out, err = runCmd(t, "-c", cfgPath, "dump"); err != nil {
		t.Fatalf("full dump: %v\n%s", err, out)
	}
	if !strings.Contains(out, "L0") {
		t.Fatalf("first dump should be a full:\n%s", out)
	}

	psql("INSERT INTO users SELECT g, 'v' || g FROM generate_series(1001, 2000) g")
	psql("CHECKPOINT")
	if out, err = runCmd(t, "-c", cfgPath, "dump"); err != nil {
		t.Fatalf("incremental dump: %v\n%s", err, out)
	}
	if !strings.Contains(out, "L1") {
		t.Fatalf("second dump should be an incremental (the manifest promoted):\n%s", out)
	}

	if out, err = runCmd(t, "-c", cfgPath, "verify", "--deep"); err != nil {
		t.Fatalf("verify --deep: %v\n%s", err, out)
	}

	// The inventory: the tables the archiver reported at dump time, sized.
	out, err = runCmd(t, "-c", cfgPath, "recover", "--dle", dleID, "--inventory")
	if err != nil {
		t.Fatalf("--inventory: %v\n%s", err, out)
	}
	if !strings.Contains(out, "tables/postgres/public.users") || !strings.Contains(out, "units · run") {
		t.Fatalf("inventory lacks the users table:\n%s", out)
	}

	// The interactive shell: inventory-driven selection. `add public.users`
	// matches the unit by suffix and selects its files — the delta-tipped heap
	// file rides the assembler, and postgresql.conf in the same extraction
	// keeps the ancestor-dir step non-empty (the --no-recursion regression).
	selDest := filepath.Join(base, "sel")
	script := strings.Join([]string{
		"inventory users",
		"add public.users",
		"add postgresql.conf",
		"dest " + selDest,
		"extract",
		"quit",
	}, "\n") + "\n"
	oldIn := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(script))
	out, err = runCmd(t, "-c", cfgPath, "recover", "--dle", dleID)
	stdinReader = oldIn
	if err != nil {
		t.Fatalf("shell session: %v\n%s", err, out)
	}
	if !strings.Contains(out, "named units — 'inventory' lists them") {
		t.Fatalf("shell lacks the inventory hint:\n%s", out)
	}
	if !strings.Contains(out, "matched unit tables/postgres/public.users") {
		t.Fatalf("unit add did not match:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(selDest, "postgresql.conf")); err != nil {
		t.Fatalf("postgresql.conf not recovered: %v", err)
	}
	var relFile string
	_ = filepath.WalkDir(filepath.Join(selDest, "base"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && relFile == "" {
			relFile = p
		}
		return nil
	})
	if relFile == "" {
		t.Fatalf("no relation file recovered under %s/base", selDest)
	}
	if fi, _ := os.Stat(relFile); fi.Size() == 0 || fi.Size()%8192 != 0 {
		t.Fatalf("assembled relation file has a non-block size %d", fi.Size())
	}

	// Whole-DLE restore: gather-then-combine into --dest, then the restored
	// cluster must boot and hold all 2000 rows.
	dest := filepath.Join(base, "restored")
	if out, err = runCmd(t, "-c", cfgPath, "recover", "--all", "--dle", dleID, "--dest", dest); err != nil {
		t.Fatalf("recover --all: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dest, ".nb-combine")); !os.IsNotExist(err) {
		t.Fatal("combine staging survived in the restored data dir")
	}
	sock2 := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dest, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\nlisten_addresses = ''\nunix_socket_directories = '%s'\n", sock2)
	f.Close()
	if err := os.Chmod(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(filepath.Join(bin, "pg_ctl"), "-D", dest, "-l", filepath.Join(dest, "restored.log"), "-w", "start").CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(dest, "restored.log"))
		t.Fatalf("restored cluster failed to start: %v\n%s\n%s", err, out, log)
	}
	defer exec.Command(filepath.Join(bin, "pg_ctl"), "-D", dest, "-m", "immediate", "stop").Run()
	got, err := exec.Command(filepath.Join(bin, "psql"), "-X", "-Atw",
		"-d", fmt.Sprintf("host=%s dbname=postgres", sock2), "-c", "SELECT count(*) FROM users").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "2000" {
		t.Fatalf("restored row count = %s, want 2000", got)
	}
}
