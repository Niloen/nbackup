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
)

func init() {
	archiver.Register("gnutar", func(opts archiver.Options, ex programs.Executor, stateRoot string) (archiver.Archiver, error) {
		bin := opts.Get("tar_path")
		if bin == "" {
			bin = "tar"
		}
		if ex == nil {
			ex = programs.Local()
		}
		return &gnutar{
			bin:           bin,
			ex:            ex,
			stateDir:      stateRoot,
			oneFileSystem: opts.Bool("one-file-system", true),
			sparse:        opts.Bool("sparse", true),
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
		Name:   argv[0],
		Args:   argv[1:],
		Stderr: stderr,
		OKExit: []int{1}, // exit 1 = "some files changed as we read them" — a warning
	}
	finish := func() (*archiver.BackupResult, error) {
		members, rerr := g.readIndex(indexPath)
		if rerr != nil {
			return nil, rerr
		}
		return &archiver.BackupResult{
			Uncompressed: parseTotals(stderr.String()),
			FileCount:    countFiles(members),
			Members:      members,
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

// List reads a raw tar stream from in and returns its member paths (`tar -t`), without
// extracting. It is the structural half of a deep verify: the pipeline completing cleanly
// proves the stream is a valid, listable archive, and the returned members compare
// directly against the seal. Exit-1 ("some files changed") is a warning, not a failure.
func (g *gnutar) List(in io.Reader) ([]string, error) {
	if err := g.Check(); err != nil {
		return nil, err
	}
	cmd := g.ex.Command(g.bin, "--list", "--file=-")
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
	members, scanErr := scanMembers(stdout)
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

// createArgs builds the shared tar create-mode argument list. fileTarget is "-" for a
// streamed backup or "/dev/null" for an estimate; indexPath, when set, adds the verbose
// member index (backup only).
func (g *gnutar) createArgs(r archiver.BackupRequest, fileTarget, snapshot, indexPath string) []string {
	args := []string{
		g.bin,
		"--create", "--file=" + fileTarget,
		"--directory=" + r.SourcePath,
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
		args = append(args, "--verbose", "--index-file="+indexPath)
	}
	return append(args, ".")
}

// readIndex reads the member index tar wrote to path on the executor's host.
func (g *gnutar) readIndex(path string) ([]string, error) {
	data, err := g.ex.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return scanMembers(bytes.NewReader(data))
}

// isWarning reports whether a tar exit was a non-fatal warning (exit code 1: "some files
// differ / changed as we read them").
func isWarning(err error) bool {
	ee, ok := err.(interface{ ExitCode() int })
	return ok && ee.ExitCode() == 1
}

// scanMembers reads a newline-separated member listing (a tar --index-file, or `tar -t`
// output) and returns the member tokens, dropping blanks and the bare "./" root entry.
func scanMembers(r io.Reader) ([]string, error) {
	var members []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && line != "./" {
			members = append(members, line)
		}
	}
	return members, sc.Err()
}

func countFiles(members []string) int {
	n := 0
	for _, m := range members {
		if !strings.HasSuffix(m, "/") {
			n++
		}
	}
	return n
}
