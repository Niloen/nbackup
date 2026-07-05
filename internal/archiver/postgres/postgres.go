// Package postgres implements archiver.Archiver over PostgreSQL 17's native
// incremental base backups: level 0 is a full `pg_basebackup`, level N≥1 a
// `pg_basebackup --incremental` against the manifest the level-(N−1) dump left
// behind. The whole backup streams — `pg_basebackup --format=tar -D -` writes
// the archive to stdout and the backup_manifest rides inside it as a member,
// teed out in flight — so the only files the archiver ever keeps on the host
// are the manifests themselves: the per-DLE, per-level base files, promoted
// `.new`-then-rename exactly like gnutar's .snar library (manifest.go).
//
// The DLE source string is a libpq connection reference (a database name,
// `service=prod`, or a conninfo string) and authentication is the client's own
// libpq configuration — peer auth as the executor identity, ~/.pgpass,
// ~/.pg_service.conf. NBackup's config carries no connection details and no
// secrets, the same doctrine as SSH (agent) and gpg (keyring). A base backup
// is CLUSTER-level: the source's database only names the connection, so
// configure one DLE per cluster.
//
// The raw stream is a plain GNU tar of the cluster (CanList, real member
// offsets, `.tar`), but the chain is combine-shaped: a changed relation file
// is stored as an `INCREMENTAL.<name>` block delta, so a whole-DLE restore
// gathers every level into staging and merges once with `pg_combinebackup`
// (RestoreIsCombine/CombineStage), and browse-time reads assemble a single
// file's chain versions in-process (assemble.go). The archive's content
// inventory — tables with sizes, for `recover --inventory` — is captured at
// dump time (tables.go).
package postgres

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
)

func init() {
	archiver.Register("postgres", []string{"mode", "bin_dir"}, func(opts archiver.Options, ex programs.Executor, stateRoot string) (archiver.Archiver, error) {
		if ex == nil {
			ex = programs.Local()
		}
		mode := opts.Get("mode")
		if mode == "" {
			mode = "incr"
		}
		if mode != "incr" {
			return nil, fmt.Errorf("postgres archiver: unknown mode %q (supported: incr — PostgreSQL 17+ native incremental base backups)", mode)
		}
		return &postgres{binDir: opts.Get("bin_dir"), ex: ex, stateRoot: stateRoot}, nil
	})
}

type postgres struct {
	binDir    string // directory holding the PostgreSQL client tools; "" = PATH
	ex        programs.Executor
	stateRoot string // root of the per-DLE/per-level manifest library (manifest.go)
}

func (p *postgres) Name() string { return "postgres" }

// bin resolves a PostgreSQL tool name against bin_dir (Debian installs the
// version-17 tools under /usr/lib/postgresql/17/bin, off the default PATH).
func (p *postgres) bin(tool string) string {
	if p.binDir == "" {
		return tool
	}
	return filepath.Join(p.binDir, tool)
}

// psql runs one SQL statement over the source's libpq connection reference and
// returns its unaligned, tuples-only output. -w fails rather than prompts: an
// unauthenticated identity must surface as an error, never a hang.
func (p *postgres) psql(source, sql string) (string, error) {
	cmd := p.ex.Command(p.bin("psql"), "-X", "-Atw", "-d", source, "-c", sql)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// psql's own message ("connection refused", "role … does not exist",
		// "password authentication failed") is the real diagnosis; a bare
		// "exit status 2" from the exec error hides it. Surface it verbatim.
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Check verifies the PostgreSQL 17+ client tools this archiver drives are
// runnable on the executor's host: pg_basebackup takes the dumps and
// pg_combinebackup merges a chain at restore. Both appeared with the required
// versions in PostgreSQL 17 (incremental base backups), so the version gate is
// one check.
func (p *postgres) Check() error {
	for _, tool := range []string{"pg_basebackup", "pg_combinebackup"} {
		out, err := p.ex.Command(p.bin(tool), "--version").Output()
		if err != nil {
			return fmt.Errorf("cannot run %q: %w (PostgreSQL 17+ client tools are required; set bin_dir if they are off PATH, e.g. /usr/lib/postgresql/17/bin)", p.bin(tool), err)
		}
		if major := pgMajor(string(out)); major > 0 && major < 17 {
			return fmt.Errorf("%q is PostgreSQL %d; incremental base backups require PostgreSQL 17+", p.bin(tool), major)
		}
	}
	return nil
}

// InterpretDumpError rewrites a failed dump's raw error into the action that fixes it.
// pg_basebackup fails an incremental in two operator-recoverable ways that its own
// message states but does not prescribe a remedy for, and the stage error leads with a
// generic "bash: exit status 1" that buries the database's words. This surfaces the
// PostgreSQL diagnosis and names the fix: turning summarize_wal back on (currently off),
// or `nb reset` for a fresh full (the WAL summaries no longer reach back to the base,
// which happens when summarization was disabled for any window since the last full — an
// incremental from that base is impossible until a new full re-anchors the chain).
func (p *postgres) InterpretDumpError(req archiver.BackupRequest, dleDisplay string, err error) error {
	if err == nil {
		return err
	}
	msg := err.Error()
	if req.Level > 0 {
		switch {
		case strings.Contains(msg, "unless WAL summarization is enabled"):
			return fmt.Errorf("this incremental needs WAL summarization, which is off on the server — turn it on with two separate statements:\n  ALTER SYSTEM SET summarize_wal = on;\n  SELECT pg_reload_conf();\n%s", extractPGError(msg))
		case strings.Contains(msg, "WAL summaries are required") && strings.Contains(msg, "incomplete"):
			return fmt.Errorf("this incremental cannot be built — PostgreSQL no longer has the WAL summaries reaching back to the last full (summarize_wal was disabled for some window since it). Force a fresh full on the next run:\n  nb reset %s\n%s", dleDisplay, extractPGError(msg))
		}
	}
	// No specific remedy, but still strip the shell-wrapper noise when pg_basebackup
	// left a real diagnosis, so the message reads as the database's own words.
	if pg := extractPGError(msg); pg != "" && pg != msg {
		return errors.New(pg)
	}
	return err
}

// extractPGError pulls the PostgreSQL/pg_basebackup diagnostic lines out of a noisy
// multi-line stage error (which leads with a generic "bash: exit status N" from the shell
// that ran the dump script), or "" when there are none to lift out.
func extractPGError(msg string) string {
	var keep []string
	for _, line := range strings.Split(msg, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "pg_basebackup:") || strings.Contains(l, "ERROR:") ||
			strings.HasPrefix(l, "DETAIL:") || strings.HasPrefix(l, "HINT:") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// pgMajor parses the major version from a `--version` line
// ("pg_basebackup (PostgreSQL) 17.2"), or 0 when unrecognized.
func pgMajor(version string) int {
	fields := strings.Fields(version)
	if len(fields) == 0 {
		return 0
	}
	major, _, _ := strings.Cut(fields[len(fields)-1], ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// CheckSource proves the whole client-side auth chain by actually connecting
// (`SELECT 1`) as the executor's identity — peer auth, ~/.pgpass, or
// ~/.pg_service.conf, whichever the client is configured with — then verifies
// the connecting role may actually take a base backup (pg_basebackup starts a
// WAL sender, which needs the REPLICATION attribute or superuser — a plain
// LOGIN role connects fine but fails the dump), and finally that WAL
// summarization is on, without which the server cannot take the incremental
// (level ≥ 1) dumps this archiver schedules. All three are proven live so
// `nb check` fails now rather than the nightly dump failing later.
func (p *postgres) CheckSource(source string) error {
	if _, err := p.psql(source, "SELECT 1"); err != nil {
		return fmt.Errorf("cannot connect: %w\n(check the server is reachable; auth is the client's own libpq config — peer auth as this identity, ~/.pgpass, or ~/.pg_service.conf; grant a role once with: CREATE ROLE <user> LOGIN REPLICATION)", err)
	}
	// pg_basebackup starts a WAL sender, so the role must have REPLICATION (or be
	// superuser); pg_roles is world-readable and exposes both for current_user.
	canReplicate, err := p.psql(source, "SELECT rolsuper OR rolreplication FROM pg_roles WHERE rolname = current_user")
	if err != nil {
		return fmt.Errorf("cannot read the connecting role's attributes: %w", err)
	}
	if canReplicate != "t" {
		return fmt.Errorf("the connecting role lacks REPLICATION — pg_basebackup cannot start a WAL sender, so the dump would fail: grant it with `ALTER ROLE <user> REPLICATION` (or connect as a superuser)")
	}
	on, err := p.psql(source, "SHOW summarize_wal")
	if err != nil {
		return fmt.Errorf("cannot read summarize_wal: %w (PostgreSQL 17+ is required for incremental base backups)", err)
	}
	if on != "on" {
		return fmt.Errorf("summarize_wal is off — incremental backups need it. Turn it on with two separate statements:\n  ALTER SYSTEM SET summarize_wal = on;\n  SELECT pg_reload_conf();")
	}
	return nil
}

// Estimate: a full is the cluster's size (a base backup images every
// database); an incremental has no cheap estimator, and 0 already means "no
// estimate" to the planner.
func (p *postgres) Estimate(r archiver.BackupRequest) (int64, error) {
	if r.Level > 0 {
		return 0, nil
	}
	out, err := p.psql(r.Source, "SELECT sum(pg_database_size(oid)) FROM pg_database")
	if err != nil {
		return 0, fmt.Errorf("postgres estimate: %w", err)
	}
	n, perr := strconv.ParseInt(out, 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("postgres estimate: unexpected size %q", out)
	}
	return n, nil
}

// BackupSource builds the streaming dump stage: pg_basebackup writes the tar
// to stdout while two FIFO readers tee the backup_manifest (this dump's
// incremental state, landing as the ".new" work manifest) and the member index
// out of the stream in flight — nothing is staged, and the explicit
// mkfifo+wait (rather than process substitution) guarantees both side files
// are complete before the script exits, so Finish never races them.
func (p *postgres) BackupSource(r archiver.BackupRequest) (*archiver.BackupSource, error) {
	if len(r.Exclude) > 0 {
		return nil, fmt.Errorf("postgres archiver does not support exclude patterns — drop `exclude` from the dumptype (a base backup images the whole cluster)")
	}
	base := ""
	if r.Level > 0 {
		base = p.manifestPath(r.DLE, r.BaseLevel)
		if err := p.ex.Stat(base); err != nil {
			return nil, fmt.Errorf("postgres L%d needs the committed L%d manifest %s, which is missing — a full must run first", r.Level, r.BaseLevel, base)
		}
	}
	if err := p.ex.MkdirAll(filepath.Dir(p.manifestPath(r.DLE, r.Level))); err != nil {
		return nil, err
	}
	indexPath, err := p.ex.TempFile("nbackup-pgindex-*")
	if err != nil {
		return nil, err
	}
	work := p.workManifest(r.DLE, r.Level)

	basebackup := shJoin(p.basebackupArgs(r.Source, base))
	script := strings.Join([]string{
		"set -e -o pipefail",
		`d=$(mktemp -d)`,
		`trap 'rm -rf "$d"' EXIT`,
		`mkfifo "$d/m" "$d/i"`,
		// Neither tar exits before its FIFO hits EOF (no --occurrence), so tee
		// never takes a SIGPIPE from a reader that stopped early. Their stderr is
		// silenced: when pg_basebackup fails it feeds tee nothing, and two tars each
		// crying "This does not look like a tar archive" would bury the ONE message
		// that matters (pg_basebackup's) — which pipefail preserves as the stage's
		// exit and Finish surfaces from this stage's stderr.
		fmt.Sprintf(`tar -xOf "$d/m" backup_manifest > %s 2>/dev/null & p1=$!`, shQuote(work)),
		fmt.Sprintf(`tar --list --block-number -f "$d/i" > %s 2>/dev/null & p2=$!`, shQuote(indexPath)),
		fmt.Sprintf(`%s | tee "$d/m" "$d/i"`, basebackup),
		`wait $p1 $p2`,
	}, "\n")

	stderr := &bytes.Buffer{}
	stage := programs.Cmd{Name: "bash", Args: []string{"-c", script}, Stderr: stderr}
	finish := func() (*archiver.BackupResult, error) {
		if n, err := p.ex.Size(work); err != nil || n == 0 {
			return nil, fmt.Errorf("pg_basebackup stream carried no backup_manifest (the next incremental would have no base): %s", strings.TrimSpace(stderr.String()))
		}
		data, err := p.ex.ReadFile(indexPath)
		if err != nil {
			return nil, err
		}
		members, err := scanMemberOffsets(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return &archiver.BackupResult{
			// The raw stream size is metered by the caller off the stage's own
			// output (pg_basebackup has no totals side channel).
			FileCount: archiver.CountFiles(members),
			Members:   members,
			Units:     p.unitsFor(r.Source, members),
		}, nil
	}
	promote := func() error { return p.promoteManifest(r.DLE, r.Level) }
	cleanup := func() { _ = p.ex.Remove(indexPath) }
	return &archiver.BackupSource{Stage: stage, Exec: p.ex, Finish: finish, Promote: promote, Cleanup: cleanup}, nil
}

// basebackupArgs is the pg_basebackup invocation: tar to stdout, WAL fetched
// into the same tar (stdout allows no second archive), a fast checkpoint so a
// dump starts when scheduled rather than waiting out a spread checkpoint, and
// -w so a missing credential fails instead of prompting. base, when set, is
// the committed prior-level manifest that makes this dump incremental.
func (p *postgres) basebackupArgs(source, base string) []string {
	args := []string{p.bin("pg_basebackup"), "-d", source, "--format=tar", "-D", "-", "--wal-method=fetch", "--checkpoint=fast", "-w"}
	if base != "" {
		args = append(args, "--incremental="+base)
	}
	return args
}

// Ext: the raw stream is a tar archive (pg_basebackup's own tar format).
func (p *postgres) Ext() string { return ".tar" }

// DestIsDir: a restore lands a data directory the generic layer owns the
// lifecycle of (created empty, rolled back on a failed chain).
func (p *postgres) DestIsDir() bool { return true }

// SourceIsPath: no — the DLE's source is a libpq connection reference, not a
// filesystem path; a preview must not stat it (readiness is CheckSource's live
// connect, proven by `nb check`).
func (p *postgres) SourceIsPath() bool { return false }

// CanList: `tar -t` enumerates the cluster files.
func (p *postgres) CanList() bool { return true }

// List reads the raw tar stream and returns its members with offsets — the
// structural half of a deep verify, same convention as the recorded index.
func (p *postgres) List(in io.Reader) ([]record.Member, error) {
	cmd := p.ex.Command("tar", "--list", "--block-number", "--file=-")
	cmd.Stdin = in
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tar: %w", err)
	}
	members, scanErr := scanMemberOffsets(stdout)
	waitErr := cmd.Wait()
	if scanErr != nil {
		return nil, scanErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("tar list failed: %w\n%s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return members, nil
}

// RestoreStage extracts the raw tar into dest. In a whole-DLE chain restore
// dest is this level's staging directory under the destination (the chain is
// combine-shaped; see RestoreIsCombine) and members is empty; with members it
// is a plain selective extraction (e.g. grabbing postgresql.conf) — relation
// files from an incremental chain go through the Assembler instead.
// --no-recursion for a selection, as gnutar does: a selection enumerates every
// member explicitly (ancestor dirs included), and without it tar's recursive
// dir match consumes the subtree and then rejects the now-redundant child args
// ("base/5/: Not found in archive", exit 2).
func (p *postgres) RestoreStage(dest string, members []string) programs.Cmd {
	args := []string{"--extract", "--file=-", "--directory=" + dest}
	if len(members) > 0 {
		args = append(args, "--no-recursion")
	}
	return programs.Cmd{Name: "tar", Args: append(args, members...)}
}

// RestoreIsCombine: yes — an incremental stores changed relation files as
// block deltas, so the chain's levels must exist on disk simultaneously for
// pg_combinebackup to merge; each level stages under the destination and
// CombineStage finalizes.
func (p *postgres) RestoreIsCombine() bool { return true }

// CombineStage merges the staged level directories (chain order, base first)
// into dest with pg_combinebackup — the database's own reconstruction tool —
// then lifts the result up and removes the staging, leaving dest as the data
// directory. --copy-file-range lets the kernel reflink instead of copy where
// the filesystem supports it (the staging lives under dest, so same fs).
func (p *postgres) CombineStage(dest string, stagingDirs []string) programs.Cmd {
	out := path.Join(dest, ".nb-combine", "out")
	combine := shJoin(append(append([]string{p.bin("pg_combinebackup"), "--copy-file-range"}, stagingDirs...), "-o", out))
	script := strings.Join([]string{
		"set -e",
		combine,
		fmt.Sprintf(`find %s -mindepth 1 -maxdepth 1 -exec mv -t %s {} +`, shQuote(out), shQuote(dest)),
		fmt.Sprintf(`rm -rf %s`, shQuote(path.Join(dest, ".nb-combine"))),
	}, "\n")
	return programs.Cmd{Name: "bash", Args: []string{"-c", script}}
}

// StockExtract opens one level's tar with stock tools; the README documents
// that a chain recovery then merges the extracted level directories with
// `pg_combinebackup L0 L1 … -o <datadir>` — still only the operator's own
// PostgreSQL tools.
func (p *postgres) StockExtract() string {
	return `tar --extract -C "$1" -f -`
}

// SpliceTrailer: the stream is plain tar — whole member extents splice, and
// the end-of-archive marker is tar's two 512-byte zero blocks.
func (p *postgres) SpliceTrailer() []byte { return make([]byte, 1024) }

// scanMemberOffsets parses a `tar --list --block-number` listing into members
// with raw-stream byte offsets ("block N: path", ×512). The same parse gnutar
// uses — duplicated rather than shared because it is tar-format knowledge the
// two archivers happen to both need, not a generic-layer concept.
func scanMemberOffsets(r io.Reader) ([]record.Member, error) {
	var members []record.Member
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		m := record.Member{Path: line, Off: -1}
		if rest, ok := strings.CutPrefix(line, "block "); ok {
			if num, p, found := strings.Cut(rest, ": "); found {
				if n, err := strconv.ParseInt(num, 10, 64); err == nil {
					m = record.Member{Path: p, Off: n * 512}
				}
			}
		}
		if m.Path == "./" || strings.HasPrefix(m.Path, "** ") {
			continue
		}
		members = append(members, m)
	}
	return members, sc.Err()
}

// shQuote single-quotes s for a shell command line, the '\” idiom for
// embedded quotes — so a conninfo or path with spaces rides as one word.
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shJoin quotes each token and joins with spaces.
func shJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = shQuote(a)
	}
	return strings.Join(out, " ")
}
