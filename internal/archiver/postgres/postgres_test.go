package postgres

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
)

// open builds a postgres archiver whose PostgreSQL tools live in binDir (the
// unit tests point it at fake shell scripts; the integration test at the real
// PG17 install).
func open(t *testing.T, binDir, stateRoot string) *postgres {
	t.Helper()
	a, err := archiver.Open("postgres", archiver.Options{"bin_dir": binDir}, programs.Local(), stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	return a.(*postgres)
}

func TestModeValidation(t *testing.T) {
	if _, err := archiver.Open("postgres", archiver.Options{"mode": "wal"}, programs.Local(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("mode wal should be rejected, got %v", err)
	}
}

// fakeTool writes an executable shell script into dir.
func fakeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakePsql answers the queries the archiver issues: connectivity, WAL
// summarization, size, database list, and one database's relation map.
func fakePsql(t *testing.T, dir string, summarizeWAL string) {
	fakeTool(t, dir, "psql", fmt.Sprintf(`case "$*" in
  *pg_database_size*) echo 12345 ;;
  *pg_database*) echo "5|testdb" ;;
  *pg_class*) echo "1234|2234|3234|public|users|90112" ;;
  *summarize_wal*) echo %s ;;
  *"SELECT 1"*) echo 1 ;;
  *) echo "unexpected psql args: $*" >&2; exit 2 ;;
esac`, summarizeWAL))
}

// clusterTar builds a small pg_basebackup-shaped tar on disk (relation files,
// a config file, and the backup_manifest) and returns its path plus the
// manifest content.
func clusterTar(t *testing.T) (tarPath, manifest string) {
	t.Helper()
	dir := t.TempDir()
	manifest = `{"PostgreSQL-Backup-Manifest-Version": 1}`
	for p, content := range map[string]string{
		"base/5/1234":       strings.Repeat("x", 16),
		"base/5/1234_fsm":   "fsm",
		"base/5/2234":       "toast heap",
		"base/5/3234":       "toast index",
		"base/5/9999":       "unmapped relation",
		"postgresql.conf":   "port=5432\n",
		"backup_manifest":   manifest,
		"global/pg_control": "ctl",
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tarPath = filepath.Join(t.TempDir(), "cluster.tar")
	cmd := exec.Command("tar", "-cf", tarPath, "-C", dir, "base", "global", "postgresql.conf", "backup_manifest")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture tar: %v\n%s", err, out)
	}
	return tarPath, manifest
}

// TestBackupStreamAndState drives the full-produce path the dumper does — run
// the stage, drain the stream, Finish, Promote — against a fake pg_basebackup
// that emits a fixture tar, and checks: the stream is byte-identical to the
// tool's output, the manifest was teed out in flight and promotes into the
// library, the member index carries offsets, and the inventory maps the
// table with its size, forks, toast, and toast index.
func TestBackupStreamAndState(t *testing.T) {
	bin := t.TempDir()
	tarPath, manifest := clusterTar(t)
	fakeTool(t, bin, "pg_basebackup", "exec cat "+tarPath)
	fakePsql(t, bin, "on")
	state := t.TempDir()
	p := open(t, bin, state)

	bs, err := p.BackupSource(archiver.BackupRequest{DLE: "db01-app", Source: "testdb", Level: 0, BaseLevel: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Cleanup()
	out, wait, err := bs.Exec.RunPipe(context.Background(), nil, bs.Stage)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	out.Close()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(tarPath)
	if !bytes.Equal(stream, want) {
		t.Fatalf("stream = %d bytes, want the fixture tar's %d", len(stream), len(want))
	}

	res, err := bs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]int64{}
	for _, m := range res.Members {
		byPath[m.Path] = m.Off
	}
	if off, ok := byPath["base/5/1234"]; !ok || off < 0 {
		t.Fatalf("member base/5/1234 missing or offsetless: %+v", byPath)
	}
	if len(res.Units) != 1 {
		t.Fatalf("units = %+v", res.Units)
	}
	u := res.Units[0]
	if u.Path != "tables/testdb/public.users" || u.Size != 90112 {
		t.Fatalf("unit = %+v", u)
	}
	wantMembers := map[string]bool{"base/5/1234": true, "base/5/1234_fsm": true, "base/5/2234": true, "base/5/3234": true}
	if len(u.Members) != len(wantMembers) {
		t.Fatalf("unit members = %v", u.Members)
	}
	for _, m := range u.Members {
		if !wantMembers[m] {
			t.Fatalf("unexpected unit member %q (unmapped/non-relation files must stay out)", m)
		}
	}

	// The dump's manifest landed as the work file and only Promote commits it.
	if p.HasBase("db01-app", 0) {
		t.Fatal("HasBase before promote")
	}
	if err := bs.Promote(); err != nil {
		t.Fatal(err)
	}
	if !p.HasBase("db01-app", 0) {
		t.Fatal("HasBase after promote")
	}
	got, err := os.ReadFile(p.manifestPath("db01-app", 0))
	if err != nil || strings.TrimSpace(string(got)) != manifest {
		t.Fatalf("promoted manifest = %q, %v", got, err)
	}
}

// TestIncrementalNeedsBase: an L1 without the committed L0 manifest is
// rejected up front; with it, the stage passes --incremental=<manifest>.
func TestIncrementalNeedsBase(t *testing.T) {
	bin := t.TempDir()
	p := open(t, bin, t.TempDir())
	if _, err := p.BackupSource(archiver.BackupRequest{DLE: "d", Source: "db", Level: 1, BaseLevel: 0}); err == nil || !strings.Contains(err.Error(), "a full must run first") {
		t.Fatalf("want missing-base error, got %v", err)
	}
	live := p.manifestPath("d", 0)
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	bs, err := p.BackupSource(archiver.BackupRequest{DLE: "d", Source: "db", Level: 1, BaseLevel: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Cleanup()
	script := strings.Join(bs.Stage.Args, " ")
	if !strings.Contains(script, shQuote("--incremental="+live)) {
		t.Fatalf("stage script lacks --incremental=%s:\n%s", live, script)
	}
}

func TestCheckSource(t *testing.T) {
	bin := t.TempDir()
	fakePsql(t, bin, "off")
	p := open(t, bin, t.TempDir())
	err := p.CheckSource("testdb")
	if err == nil || !strings.Contains(err.Error(), "summarize_wal = on") {
		t.Fatalf("want summarize_wal hint, got %v", err)
	}
	fakePsql(t, bin, "on")
	if err := p.CheckSource("testdb"); err != nil {
		t.Fatal(err)
	}
}

func TestEstimate(t *testing.T) {
	bin := t.TempDir()
	fakePsql(t, bin, "on")
	p := open(t, bin, t.TempDir())
	n, err := p.Estimate(archiver.BackupRequest{Source: "testdb", Level: 0, BaseLevel: -1})
	if err != nil || n != 12345 {
		t.Fatalf("estimate = %d, %v", n, err)
	}
	n, err = p.Estimate(archiver.BackupRequest{Source: "testdb", Level: 1, BaseLevel: 0})
	if err != nil || n != 0 {
		t.Fatalf("incremental estimate = %d, %v (want 0: no estimator)", n, err)
	}
}

func TestPgMajor(t *testing.T) {
	for in, want := range map[string]int{
		"pg_basebackup (PostgreSQL) 17.2":       17,
		"pg_combinebackup (PostgreSQL) 18beta1": 0, // unparseable minor tail → unknown, not a failure
		"pg_basebackup (PostgreSQL) 15.6":       15,
		"":                                      0,
	} {
		if got := pgMajor(in); got != want {
			t.Errorf("pgMajor(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestLogical(t *testing.T) {
	a := assembler{}
	if l, d := a.Logical("base/16384/INCREMENTAL.2619"); l != "base/16384/2619" || !d {
		t.Fatalf("Logical delta = %q, %v", l, d)
	}
	if l, d := a.Logical("base/16384/2619"); l != "base/16384/2619" || d {
		t.Fatalf("Logical whole = %q, %v", l, d)
	}
	if l, d := a.Logical("postgresql.conf"); l != "postgresql.conf" || d {
		t.Fatalf("Logical conf = %q, %v", l, d)
	}
}

// delta builds a synthetic INCREMENTAL file: truncation length plus
// (block number → content) pairs, each content padded to a full block.
func delta(t *testing.T, truncTo uint32, blocks map[uint32]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	nums := make([]uint32, 0, len(blocks))
	for n := range blocks {
		nums = append(nums, n)
	}
	// deterministic order
	for i := 0; i < len(nums); i++ {
		for j := i + 1; j < len(nums); j++ {
			if nums[j] < nums[i] {
				nums[i], nums[j] = nums[j], nums[i]
			}
		}
	}
	for _, v := range []uint32{incrementalMagic, uint32(len(nums)), truncTo} {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := binary.Write(&buf, binary.LittleEndian, nums); err != nil {
		t.Fatal(err)
	}
	// Real PG17 deltas pad the header to a BLCKSZ boundary before block data
	// (only when blocks follow).
	if len(nums) > 0 {
		if pad := (blckSz - buf.Len()%blckSz) % blckSz; pad > 0 {
			buf.Write(make([]byte, pad))
		}
	}
	for _, n := range nums {
		buf.Write(bytes.Repeat([]byte{blocks[n]}, blckSz))
	}
	return buf.Bytes()
}

// block reads one block of the assembled output and asserts it is uniformly b.
func assertBlock(t *testing.T, content []byte, i int, b byte) {
	t.Helper()
	blk := content[i*blckSz : (i+1)*blckSz]
	for _, got := range blk {
		if got != b {
			t.Fatalf("block %d = %#x, want %#x", i, got, b)
		}
	}
}

func TestAssemble(t *testing.T) {
	a := assembler{}
	base := append(bytes.Repeat([]byte{1}, blckSz), bytes.Repeat([]byte{2}, blckSz)...) // 2 blocks

	// Overlay: keep block 0, replace block 1, grow to 3 with a new block 2.
	d := delta(t, 2, map[uint32]byte{1: 7, 2: 8})
	rc, err := a.Assemble([]archiver.Version{
		{R: bytes.NewReader(base)},
		{R: bytes.NewReader(d), Delta: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(rc)
	if len(out) != 3*blckSz {
		t.Fatalf("len = %d blocks", len(out)/blckSz)
	}
	assertBlock(t, out, 0, 1)
	assertBlock(t, out, 1, 7)
	assertBlock(t, out, 2, 8)

	// Truncation: the file shrank to 1 block; stale base content past it dies.
	d = delta(t, 1, nil)
	rc, err = a.Assemble([]archiver.Version{
		{R: bytes.NewReader(base)},
		{R: bytes.NewReader(d), Delta: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = io.ReadAll(rc)
	if len(out) != blckSz {
		t.Fatalf("truncated len = %d blocks, want 1", len(out)/blckSz)
	}
	assertBlock(t, out, 0, 1)

	// Shrink-then-regrow: truncation 1 with a delta block 2 — block 1 must be
	// zero (the old base content is dead), block 2 the delta's.
	d = delta(t, 1, map[uint32]byte{2: 9})
	rc, err = a.Assemble([]archiver.Version{
		{R: bytes.NewReader(base)},
		{R: bytes.NewReader(d), Delta: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = io.ReadAll(rc)
	if len(out) != 3*blckSz {
		t.Fatalf("regrown len = %d blocks, want 3", len(out)/blckSz)
	}
	assertBlock(t, out, 0, 1)
	assertBlock(t, out, 1, 0)
	assertBlock(t, out, 2, 9)

	// A newer WHOLE version replaces outright (a file rewritten between dumps).
	rc, err = a.Assemble([]archiver.Version{
		{R: bytes.NewReader(base)},
		{R: bytes.NewReader(bytes.Repeat([]byte{5}, blckSz))},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = io.ReadAll(rc)
	if len(out) != blckSz {
		t.Fatalf("whole-replace len = %d", len(out))
	}
	assertBlock(t, out, 0, 5)

	// Three levels: base, delta, delta.
	d1 := delta(t, 2, map[uint32]byte{0: 3})
	d2 := delta(t, 2, map[uint32]byte{1: 4})
	rc, err = a.Assemble([]archiver.Version{
		{R: bytes.NewReader(base)},
		{R: bytes.NewReader(d1), Delta: true},
		{R: bytes.NewReader(d2), Delta: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = io.ReadAll(rc)
	assertBlock(t, out, 0, 3)
	assertBlock(t, out, 1, 4)

	// A leading delta means the chain is broken for this file.
	if _, err := a.Assemble([]archiver.Version{{R: bytes.NewReader(d1), Delta: true}}); err == nil {
		t.Fatal("leading delta must error")
	}
	// Garbage delta: bad magic.
	if _, err := a.Assemble([]archiver.Version{
		{R: bytes.NewReader(base)},
		{R: bytes.NewReader([]byte("not a delta, definitely long enough")), Delta: true},
	}); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("bad magic error = %v", err)
	}
}

func TestRelationKey(t *testing.T) {
	// Every stored form of one relation's files keys back to the same owner.
	for in, want := range map[string]string{
		"base/5/1234":                 "base/5/1234",
		"base/5/1234.1":               "base/5/1234",
		"base/5/1234_fsm":             "base/5/1234",
		"base/5/1234_vm":              "base/5/1234",
		"base/5/1234_fsm.2":           "base/5/1234",
		"base/5/INCREMENTAL.1234":     "base/5/1234",
		"base/5/INCREMENTAL.1234_fsm": "base/5/1234",
		"base/5/1234_other":           "base/5/1234_other", // unknown fork suffix stays distinct
		"postgresql.conf":             "postgresql.conf",
	} {
		if got := relationKey(in); got != want {
			t.Errorf("relationKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCombineStage(t *testing.T) {
	p := open(t, "/opt/pg/bin", t.TempDir())
	if !p.RestoreIsCombine() {
		t.Fatal("postgres must be combine-shaped")
	}
	cmd := p.CombineStage("/restore/dest", []string{"/restore/dest/.nb-combine/L0", "/restore/dest/.nb-combine/L1"})
	script := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"/opt/pg/bin/pg_combinebackup", "--copy-file-range",
		"'/restore/dest/.nb-combine/L0' '/restore/dest/.nb-combine/L1'",
		"'-o' '/restore/dest/.nb-combine/out'",
		"rm -rf '/restore/dest/.nb-combine'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("combine script lacks %q:\n%s", want, script)
		}
	}
}

func TestExcludeRejected(t *testing.T) {
	p := open(t, t.TempDir(), t.TempDir())
	if _, err := p.BackupSource(archiver.BackupRequest{DLE: "d", Source: "db", Exclude: []string{"x"}}); err == nil {
		t.Fatal("exclude must be rejected")
	}
}

// --- integration against a real PostgreSQL 17 ------------------------------------

// pg17 locates a PostgreSQL 17+ install that carries the SERVER tools too
// (initdb/pg_ctl — the test spins a throwaway cluster), preferring the Debian
// versioned layout over PATH (where Debian symlinks only client tools), or skips.
func pg17(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{"/usr/lib/postgresql/17/bin", "/usr/lib/postgresql/18/bin", ""} {
		candidate := "pg_basebackup"
		if dir != "" {
			candidate = filepath.Join(dir, "pg_basebackup")
		}
		p, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(p), "initdb")); err != nil {
			continue
		}
		out, err := exec.Command(p, "--version").Output()
		if err == nil && pgMajor(string(out)) >= 17 {
			return filepath.Dir(p)
		}
	}
	t.Skip("PostgreSQL 17+ server not installed (apt install postgresql-17); skipping live-cluster integration")
	return ""
}

// startCluster initdbs a throwaway cluster with WAL summarization on, starts
// it on a unix socket (no TCP port collisions), and returns the conninfo and
// the bin dir. The cluster stops and is deleted with the test.
func startCluster(t *testing.T, bin string) (conninfo string) {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	sock := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(filepath.Join(bin, name), args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("initdb", "-D", data, "--auth=trust", "--no-sync")
	cfg := fmt.Sprintf("\nsummarize_wal = on\nlisten_addresses = ''\nunix_socket_directories = '%s'\n", sock)
	f, err := os.OpenFile(filepath.Join(data, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(cfg); err != nil {
		t.Fatal(err)
	}
	f.Close()
	run("pg_ctl", "-D", data, "-l", filepath.Join(data, "log"), "-w", "start")
	t.Cleanup(func() { _ = exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-m", "immediate", "stop").Run() })
	return fmt.Sprintf("host=%s dbname=postgres", sock)
}

// TestLiveIncrementalCycle is the real thing: full → change data → incremental
// → gather-then-combine restore → the combined cluster boots and holds the
// rows — plus the assembler cross-validated byte-for-byte against
// pg_combinebackup's own output for every relation file, and real table
// aliases.
func TestLiveIncrementalCycle(t *testing.T) {
	bin := pg17(t)
	conninfo := startCluster(t, bin)
	state := t.TempDir()
	p := open(t, bin, state)
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckSource(conninfo); err != nil {
		t.Fatal(err)
	}
	psql := func(sql string) string {
		out, err := exec.Command(filepath.Join(bin, "psql"), "-X", "-Atw", "-d", conninfo, "-c", sql).CombinedOutput()
		if err != nil {
			t.Fatalf("psql %q: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	psql("CREATE TABLE users (id int, name text)")
	psql("INSERT INTO users SELECT g, 'u' || g FROM generate_series(1, 1000) g")

	dump := func(level, baseLevel int) ([]byte, *archiver.BackupResult) {
		t.Helper()
		bs, err := p.BackupSource(archiver.BackupRequest{DLE: "db01", Source: conninfo, Level: level, BaseLevel: baseLevel})
		if err != nil {
			t.Fatal(err)
		}
		defer bs.Cleanup()
		out, wait, err := bs.Exec.RunPipe(context.Background(), nil, bs.Stage)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := io.ReadAll(out)
		if err != nil {
			t.Fatal(err)
		}
		out.Close()
		if err := wait(); err != nil {
			t.Fatal(err)
		}
		res, err := bs.Finish()
		if err != nil {
			t.Fatal(err)
		}
		if err := bs.Promote(); err != nil {
			t.Fatal(err)
		}
		return stream, res
	}

	full, fullRes := dump(0, -1)
	if !p.HasBase("db01", 0) {
		t.Fatal("no base after promoted full")
	}
	var users record.Unit
	for _, u := range fullRes.Units {
		if u.Path == "tables/postgres/public.users" {
			users = u
		}
	}
	if users.Path == "" || users.Size == 0 || len(users.Members) == 0 {
		t.Fatalf("users table unit missing/empty: %+v (all: %d units)", users, len(fullRes.Units))
	}
	// The unit's TOAST relation rides along (name text is toastable).
	toast := 0
	for _, m := range users.Members {
		if !strings.HasPrefix(m, "base/") {
			t.Fatalf("unit member %q outside base/", m)
		}
		toast++
	}
	if toast < 2 {
		t.Fatalf("users unit should span heap+toast files, got %v", users.Members)
	}

	psql("INSERT INTO users SELECT g, 'v' || g FROM generate_series(1001, 2000) g")
	psql("CHECKPOINT")
	incr, incrRes := dump(1, 0)
	hasDelta := false
	for _, m := range incrRes.Members {
		if strings.Contains(m.Path, "INCREMENTAL.") {
			hasDelta = true
		}
	}
	if !hasDelta {
		t.Fatal("incremental stream carries no INCREMENTAL members")
	}

	// Gather-then-combine restore, exactly as the restorer stages it.
	dest := t.TempDir()
	staging := []string{filepath.Join(dest, ".nb-combine", "L0"), filepath.Join(dest, ".nb-combine", "L1")}
	for i, stream := range [][]byte{full, incr} {
		if err := os.MkdirAll(staging[i], 0o755); err != nil {
			t.Fatal(err)
		}
		stage := p.RestoreStage(staging[i], nil)
		out, wait, err := programs.Local().RunPipe(context.Background(), bytes.NewReader(stream), stage)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, out)
		out.Close()
		if err := wait(); err != nil {
			t.Fatal(err)
		}
	}

	// Cross-validate the Go assembler against pg_combinebackup before the
	// staging is consumed: every relation file the incremental stored as a
	// delta must assemble byte-for-byte to what the real tool produces.
	combined := filepath.Join(t.TempDir(), "combined")
	if out, err := exec.Command(filepath.Join(bin, "pg_combinebackup"), staging[0], staging[1], "-o", combined).CombinedOutput(); err != nil {
		t.Fatalf("pg_combinebackup: %v\n%s", err, out)
	}
	a := assembler{}
	checked := 0
	err := filepath.Walk(staging[1], func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.Contains(filepath.Base(path), "INCREMENTAL.") {
			return err
		}
		relDelta, _ := filepath.Rel(staging[1], path)
		logical, _ := a.Logical(filepath.ToSlash(relDelta))
		baseBytes, err := os.ReadFile(filepath.Join(staging[0], filepath.FromSlash(logical)))
		if err != nil {
			return nil // file new since the full; stored whole, nothing to assemble
		}
		deltaBytes, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rc, err := a.Assemble([]archiver.Version{
			{R: bytes.NewReader(baseBytes)},
			{R: bytes.NewReader(deltaBytes), Delta: true},
		})
		if err != nil {
			return fmt.Errorf("assemble %s: %w", logical, err)
		}
		got, _ := io.ReadAll(rc)
		want, err := os.ReadFile(filepath.Join(combined, filepath.FromSlash(logical)))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("assembled %s differs from pg_combinebackup (%d vs %d bytes)", logical, len(got), len(want))
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("cross-validated no delta files — the test lost its teeth")
	}
	t.Logf("assembler cross-validated on %d delta files", checked)

	// The archiver's own CombineStage merges the staging into dest…
	stage := p.CombineStage(dest, staging)
	out, wait, err := programs.Local().RunPipe(context.Background(), nil, stage)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, out)
	out.Close()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".nb-combine")); !os.IsNotExist(err) {
		t.Fatal("staging survived the combine")
	}

	// …and the restored cluster boots with all 2000 rows.
	sock2 := t.TempDir()
	cfg := fmt.Sprintf("\nlisten_addresses = ''\nunix_socket_directories = '%s'\n", sock2)
	f, err := os.OpenFile(filepath.Join(dest, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(cfg)
	f.Close()
	if err := os.Chmod(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(filepath.Join(bin, "pg_ctl"), "-D", dest, "-l", filepath.Join(dest, "restored.log"), "-w", "start").CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(dest, "restored.log"))
		t.Fatalf("restored cluster failed to start: %v\n%s\n%s", err, out, log)
	}
	defer exec.Command(filepath.Join(bin, "pg_ctl"), "-D", dest, "-m", "immediate", "stop").Run()
	out2, err := exec.Command(filepath.Join(bin, "psql"), "-X", "-Atw", "-d", fmt.Sprintf("host=%s dbname=postgres", sock2), "-c", "SELECT count(*) FROM users").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out2)); got != "2000" {
		t.Fatalf("restored row count = %s, want 2000", got)
	}
}
