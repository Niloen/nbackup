// Package gnutar implements archiver.Archiver using the system GNU tar binary in
// listed-incremental mode. It owns all tar-specific concerns — flags, snapshot
// (.snar) files, and the dumpdir-based
// deletion semantics — and produces/consumes a raw tar stream. It also owns its
// incremental-state library: per-DLE, per-level .snar files under state_dir,
// so the generic layer never names a snapshot.
// Compression and storage are handled by the caller.
//
// Every process and every scratch file (the snapshot library, the member-index temp)
// goes through an injected programs.Executor, so gnutar runs identically whether tar is
// the local binary or a stock tar on a client reached over SSH — gnutar holds no
// knowledge of where it runs. When the executor is remote, state_dir is a client path
// (the .snar library lives on the client) and the produced tar stage fuses with the
// compress/encrypt stages on that client.
package gnutar

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
)

func init() {
	archiver.Register("gnutar", []string{"tar_path", "one-file-system", "sparse"}, func(opts archiver.Options, ex programs.Executor, stateRoot string) (archiver.Archiver, error) {
		bin := opts.Get("tar_path")
		if bin == "" {
			bin = "tar"
		}
		if ex == nil {
			ex = programs.Local()
		}
		oneFS, err := opts.Bool("one-file-system", true)
		if err != nil {
			return nil, err
		}
		sparse, err := opts.Bool("sparse", true)
		if err != nil {
			return nil, err
		}
		return &gnutar{
			bin:           bin,
			ex:            ex,
			stateDir:      stateRoot,
			oneFileSystem: oneFS,
			sparse:        sparse,
		}, nil
	})
}

type gnutar struct {
	bin           string
	ex            programs.Executor // host where tar runs and the .snar library / index temp live
	stateDir      string            // root of the per-DLE/per-level .snar library
	oneFileSystem bool
	sparse        bool
	checkOnce     sync.Once
	checkErr      error
}

func (g *gnutar) Name() string { return "gnutar" }

// The per-DLE/per-level .snar library (snapPath, workSnap, HasBase, seedSnapshot,
// promoteSnapshot) lives in snapshot.go.

// Check verifies the configured binary is GNU tar (cached), running `tar --version` on
// the executor's host.
func (g *gnutar) Check() error {
	g.checkOnce.Do(func() {
		out, err := g.ex.Command(g.bin, "--version").Output()
		if err != nil {
			g.checkErr = fmt.Errorf("cannot run %q: %w (GNU tar is required)", g.bin, err)
			return
		}
		if !strings.Contains(string(out), "GNU tar") {
			g.checkErr = fmt.Errorf("%q is not GNU tar; listed-incremental backups require GNU tar", g.bin)
		}
	})
	return g.checkErr
}

// CheckSource probes the DLE's source directory for readability on the executor's
// host — the tree-archiver meaning of "source ready".
func (g *gnutar) CheckSource(source string) error {
	if err := g.ex.Command("test", "-r", source).Run(); err != nil {
		return fmt.Errorf("not readable")
	}
	return nil
}

// Ext: the raw stream is a tar archive.
func (g *gnutar) Ext() string { return ".tar" }

// DestIsDir: extraction lands in a directory tree the generic layer owns — it may
// create it, must refuse a non-empty one for a chain restore (listed-incremental
// replay deletes), and may clear it to roll back a failed chain.
func (g *gnutar) DestIsDir() bool { return true }

// CanList: `tar -t` enumerates members.
func (g *gnutar) CanList() bool { return true }

// StockExtract is the README's stock extraction tail: whole-archive
// listed-incremental extraction into "$1", exactly what RestoreStage runs.
func (g *gnutar) StockExtract() string {
	return `tar --extract --listed-incremental=/dev/null --numeric-owner -C "$1" -f -`
}

var totalsRE = regexp.MustCompile(`Total bytes written: (\d+)`)

// parseTotals extracts tar's `--totals` byte count from its stderr, or 0 if absent.
func parseTotals(stderr string) int64 {
	for _, line := range strings.Split(stderr, "\n") {
		if m := totalsRE.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			return n
		}
	}
	return 0
}

// BackupSource builds the tar-create stage that produces the raw archive stream and a
// Finish hook that, after the pipeline drains, reads the member index and totals from the
// host scratch. tar writes the new snapshot to a work snapshot (a ".new" side file) seeded
// from the committed base, which is read but never mutated; Promote renames the work
// snapshot over the live base only once the caller has durably committed the archive — so a
// killed dump (out of space, signal) leaves the base intact and a retry at the same level
// still works.
func (g *gnutar) BackupSource(r archiver.BackupRequest) (*archiver.BackupSource, error) {
	if err := g.Check(); err != nil {
		return nil, err
	}
	work := g.workSnap(r.DLE, r.Level)
	if err := g.seedSnapshot(r, work); err != nil {
		return nil, err
	}
	indexPath, err := g.ex.TempFile("nbackup-index-*")
	if err != nil {
		return nil, err
	}
	stderr := &bytes.Buffer{}
	argv := g.createArgs(r, "-", work, indexPath)
	stage := programs.Cmd{
		Name: argv[0],
		Args: argv[1:],
		// exit 1 = "some files changed as we read them" (a warning); exit 2 = a fatal tar
		// error OR merely unreadable members — the two share an exit code, so accept it here
		// and let finish classify the stderr (Amanda's "strange"/partial dump).
		Stderr: stderr,
		OKExit: []int{1, 2},
	}
	finish := func() (*archiver.BackupResult, error) {
		// tar continues past an unreadable file, writes a valid end-of-archive for what it
		// could read, and exits 2; a genuinely fatal error (write failure, OOM) also exits 2
		// but leaves a non-read-error line in stderr. Classify so a partial dump still commits
		// (with the unreadable list) while a fatal one aborts.
		unreadable, fatal := classifyTarStderr(stderr.String())
		if fatal != nil {
			return nil, fatal
		}
		members, rerr := g.readIndex(indexPath)
		if rerr != nil {
			return nil, rerr
		}
		return &archiver.BackupResult{
			Uncompressed: parseTotals(stderr.String()),
			FileCount:    archiver.CountFiles(members),
			Members:      members,
			Unreadable:   unreadable,
		}, nil
	}
	promote := func() error { return g.promoteSnapshot(r.DLE, r.Level) }
	cleanup := func() { _ = g.ex.Remove(indexPath) }
	return &archiver.BackupSource{Stage: stage, Exec: g.ex, Finish: finish, Promote: promote, Cleanup: cleanup}, nil
}

// Estimate computes the dump size by running tar with the archive targeted at
// /dev/null. GNU tar detects the null device and walks
// metadata without reading file bodies, so this is fast yet exact — it honors excludes,
// one-file-system, and the listed-incremental base natively. A throwaway snapshot is
// used so the real .snar library is untouched. The result is the uncompressed archive
// size (an upper bound on the bytes finally stored).
func (g *gnutar) Estimate(r archiver.BackupRequest) (int64, error) {
	if err := g.Check(); err != nil {
		return 0, err
	}
	tmpSnap, err := g.ex.TempFile("nbackup-estsnap-*")
	if err != nil {
		return 0, err
	}
	defer g.ex.Remove(tmpSnap)

	if err := g.seedSnapshot(r, tmpSnap); err != nil {
		return 0, err
	}

	args := g.createArgs(r, "/dev/null", tmpSnap, "")
	cmd := g.ex.Command(args[0], args[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	total := parseTotals(stderr.String())
	if runErr != nil && !isWarning(runErr) {
		// tar hit an unreadable member (exit 2) but still walked the rest and
		// reported a running total. Surface that total as a *floor* alongside the
		// error so the caller can warn rather than silently estimating ~0 B — a
		// silent 0 would let capacity planning undercount.
		return total, fmt.Errorf("%s estimate incomplete — source not fully readable: %w\n%s", g.bin, runErr, strings.TrimSpace(stderr.String()))
	}
	return total, nil
}

// List reads a raw tar stream from in and returns its members with their stream offsets
// (`tar -tR`), without extracting. It is the structural half of a deep verify: the
// pipeline completing cleanly proves the stream is a valid, listable archive, and the
// returned members compare directly against the seal — offsets included, so a member
// that moved in the stream is caught even when every name survives. Exit-1 ("some files
// changed") is a warning, not a failure.
func (g *gnutar) List(in io.Reader) ([]record.Member, error) {
	if err := g.Check(); err != nil {
		return nil, err
	}
	cmd := g.ex.Command(g.bin, "--list", "--block-number", "--file=-")
	cmd.Stdin = in
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", g.bin, err)
	}
	members, scanErr := scanMemberOffsets(stdout)
	waitErr := cmd.Wait()
	if scanErr != nil {
		return nil, scanErr
	}
	if waitErr != nil && !isWarning(waitErr) {
		return nil, fmt.Errorf("%s list failed: %w\n%s", g.bin, waitErr, strings.TrimSpace(stderr.String()))
	}
	return members, nil
}

// RestoreStage returns the tar program stage that extracts a raw archive stream into
// destDir — the read-side peer of BackupSource's source stage. Composing it as a stage
// (rather than running it) lets a decode→extract pipeline (`gpg -d | zstd -d | tar -x`)
// run entirely on one host, so a client-only key decrypts on the client. With members it
// extracts only those in plain mode (no deletions); without, a whole-archive chain restore
// that honors the listed-incremental dumpdir. tar exit 1 ("files changed") is a warning.
func (g *gnutar) RestoreStage(destDir string, members []string) programs.Cmd {
	args := []string{
		"--extract", "--file=-",
		"--directory=" + destDir,
		"--numeric-owner",
	}
	if len(members) == 0 {
		args = append(args, "--listed-incremental=/dev/null")
	} else {
		args = append(args, "--no-recursion")
		args = append(args, members...)
	}
	return programs.Cmd{Name: g.bin, Args: args, OKExit: []int{1}}
}

// RestoreIsCombine: no — a chain restore is tar's own additive listed-incremental
// replay, each level applied directly into the destination.
func (g *gnutar) RestoreIsCombine() bool { return false }

func (g *gnutar) CombineStage(string, []string) programs.Cmd { return programs.Cmd{} }

// Assembler: nil — tar incrementals store whole files, so the newest version of a
// path IS the file (the browse tree's default).
func (g *gnutar) Assembler() archiver.Assembler { return nil }

// SpliceTrailer: tar streams splice — every member leads with a self-describing
// 512-byte header and carries no cross-member state, so a concatenation of whole
// member extents is itself a readable tar stream; the trailer is tar's end-of-archive
// marker, two 512-byte zero blocks. The one place tar's splice/EOF shape is known.
func (g *gnutar) SpliceTrailer() []byte { return make([]byte, 1024) }

// createArgs builds the shared tar create-mode argument list. fileTarget is "-" for a
// streamed backup or "/dev/null" for an estimate; indexPath, when set, adds the verbose
// member index (backup only).
func (g *gnutar) createArgs(r archiver.BackupRequest, fileTarget, snapshot, indexPath string) []string {
	args := []string{
		g.bin,
		"--create", "--file=" + fileTarget,
		"--directory=" + r.Source,
		"--listed-incremental=" + snapshot,
		"--totals",
	}
	if g.oneFileSystem {
		args = append(args, "--one-file-system")
	}
	if g.sparse {
		args = append(args, "--sparse")
	}
	for _, p := range r.Exclude {
		args = append(args, "--exclude="+p)
	}
	if indexPath != "" {
		args = append(args, "--verbose", "--block-number", "--index-file="+indexPath)
	}
	return append(args, ".")
}

// readIndex reads the member index tar wrote to path on the executor's host.
func (g *gnutar) readIndex(path string) ([]record.Member, error) {
	data, err := g.ex.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return scanMemberOffsets(bytes.NewReader(data))
}

// isWarning reports whether a tar exit was a non-fatal warning (exit code 1: "some files
// differ / changed as we read them").
func isWarning(err error) bool {
	ee, ok := err.(interface{ ExitCode() int })
	return ok && ee.ExitCode() == 1
}

// tarReadErrMarkers are the stderr substrings that mark a member tar could not read but
// skipped past — a permission/metadata error that leaves the archive a valid (partial)
// stream of what *was* readable. These do not corrupt the archive, so they downgrade a
// fatal-looking exit 2 to a partial dump.
var tarReadErrMarkers = []string{": Cannot open", ": Cannot stat", ": Cannot read", ": Cannot change ownership", ": Cannot change mode", ": Cannot utime", ": Permission denied"}

// tarBenignInfo are informational/warning stderr lines tar emits in normal operation
// (incremental directory notes, one-file-system skips, the totals/summary lines). They are
// not errors and must not be mistaken for a fatal one.
// The volatile-file warnings ("file changed as we read it", "File removed before we read
// it", "File shrunk by … bytes") are all exit-1 notices that a live file mutated during the
// walk — the archive stays valid, so they are benign, not fatal. This is what stops a dump
// from aborting merely because a file was removed or rewritten while tar scanned it.
var tarBenignInfo = []string{"Directory is new", "Directory has been renamed", "Directory is on a different filesystem", "contains a cache directory tag", "Removing leading", "socket ignored", "file changed as we read it", "File removed before we read it", "File shrunk by", "file is the archive; not dumped", "Total bytes written", "Total bytes read", "Exiting with failure status due to previous errors"}

// classifyTarStderr separates a tar run's stderr into the paths it could not read (a
// partial dump) and a fatal error (anything unrecognized on a non-zero exit). It is
// deliberately conservative: an unrecognized line is treated as fatal, so the dump fails
// exactly as before rather than risk committing a genuinely broken archive — only a stderr
// made up solely of known read errors and benign info yields a partial commit.
func classifyTarStderr(stderr string) (unreadable []string, fatal error) {
	for _, raw := range strings.Split(stderr, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || containsAny(line, tarBenignInfo) {
			continue
		}
		if path, ok := tarReadError(line); ok {
			if path != "" {
				unreadable = append(unreadable, path)
			}
			continue
		}
		return nil, fmt.Errorf("%s", line)
	}
	return unreadable, nil
}

// tarReadError reports whether line is a read/permission error and, if so, the offending
// path (parsed from `tar: <path>: Cannot open: …`); the path is "" when tar named none.
func tarReadError(line string) (string, bool) {
	for _, m := range tarReadErrMarkers {
		i := strings.Index(line, m)
		if i < 0 {
			continue
		}
		head := line[:i] // "tar: <path>"
		if j := strings.Index(head, ": "); j >= 0 {
			return strings.TrimSpace(head[j+2:]), true
		}
		return "", true
	}
	return "", false
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// scanMemberOffsets reads a --block-number member listing (a create-mode --index-file,
// or `tar -tR` output) and returns the members with their raw-stream byte offsets. tar
// prefixes each member with the 512-byte archive block its header starts at ("block N:
// path"); the ×512 conversion happens here — nothing outside gnutar knows tar's block
// size. Blanks, the bare "./" root entry, and list mode's "** Block of NULs **" /
// "** End of File **" end-of-archive markers are dropped. A line without the block
// prefix is kept with offset -1 rather than dropped, so unexpected tar output degrades
// to "offset unreported", never to a missing member.
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
			if num, path, found := strings.Cut(rest, ": "); found {
				if n, err := strconv.ParseInt(num, 10, 64); err == nil {
					m = record.Member{Path: path, Off: n * 512}
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
