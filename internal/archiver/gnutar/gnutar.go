// Package gnutar implements archiver.Archiver using the system GNU tar binary in
// listed-incremental mode (the same mechanism Amanda's amgtar uses). It owns all
// tar-specific concerns — flags, snapshot (.snar) files, and the dumpdir-based
// deletion semantics — and produces/consumes a raw tar stream. It also owns its
// incremental-state library: per-DLE, per-level .snar files under state_dir
// (Amanda's GNUTAR-LISTDIR), so the generic layer never names a snapshot.
// Compression and storage are handled by the caller.
package gnutar

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Niloen/nbackup/internal/archiver"
)

func init() {
	archiver.Register("gnutar", func(opts archiver.Options) (archiver.Archiver, error) {
		bin := opts.Get("tar_path")
		if bin == "" {
			bin = "tar"
		}
		return &gnutar{
			bin:           bin,
			stateDir:      opts.Get("state_dir"),
			oneFileSystem: opts.Bool("one-file-system", true),
			sparse:        opts.Bool("sparse", true),
		}, nil
	})
}

type gnutar struct {
	bin           string
	stateDir      string // root of the per-DLE/per-level .snar library (Amanda's GNUTAR-LISTDIR)
	oneFileSystem bool
	sparse        bool
	checkOnce     sync.Once
	checkErr      error
}

func (g *gnutar) Name() string { return "gnutar" }

// snapPath is the location of a DLE's snapshot for a level within the library.
func (g *gnutar) snapPath(dle string, level int) string {
	return filepath.Join(g.stateDir, dle, fmt.Sprintf("L%d.snar", level))
}

// HasBase reports whether the snapshot left by a completed dump at the level is
// present — the base a higher incremental builds on.
func (g *gnutar) HasBase(dle string, level int) bool {
	_, err := os.Stat(g.snapPath(dle, level))
	return err == nil
}

// Check verifies the configured binary is GNU tar (cached).
func (g *gnutar) Check() error {
	g.checkOnce.Do(func() {
		out, err := exec.Command(g.bin, "--version").Output()
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

// Backup writes a raw tar stream to out and updates the DLE's snapshot for this
// level in the library.
func (g *gnutar) Backup(r archiver.BackupRequest, out io.Writer) (*archiver.BackupResult, error) {
	if err := g.Check(); err != nil {
		return nil, err
	}
	outSnap := g.snapPath(r.DLE, r.Level)
	if err := g.seedSnapshot(r, outSnap); err != nil {
		return nil, err
	}

	indexFile, err := os.CreateTemp("", "nbackup-index-*")
	if err != nil {
		return nil, err
	}
	indexPath := indexFile.Name()
	indexFile.Close()
	defer os.Remove(indexPath)

	totals, err := g.runCreate(r, out, outSnap, indexPath)
	if err != nil {
		return nil, err
	}
	members, err := readIndex(indexPath)
	if err != nil {
		return nil, err
	}
	return &archiver.BackupResult{
		Uncompressed: totals,
		FileCount:    countFiles(members),
		Members:      members,
	}, nil
}

// Estimate computes the dump size the way Amanda's client estimate does: it runs
// tar with the archive targeted at /dev/null. GNU tar detects the null device and
// walks metadata without reading file bodies, so this is fast yet exact — it
// honors excludes, one-file-system, and the listed-incremental base natively. A
// throwaway snapshot is used so the real .snar library is untouched. The result
// is the uncompressed archive size (an upper bound on the bytes finally stored).
func (g *gnutar) Estimate(r archiver.BackupRequest) (int64, error) {
	if err := g.Check(); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp("", "nbackup-estsnap-*")
	if err != nil {
		return 0, err
	}
	tmpSnap := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpSnap)

	if err := g.seedSnapshot(r, tmpSnap); err != nil {
		return 0, err
	}

	args := g.createArgs(r, "/dev/null", tmpSnap, "")
	cmd := exec.Command(g.bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil && !isWarning(err) {
		return 0, fmt.Errorf("%s estimate failed: %w\n%s", g.bin, err, strings.TrimSpace(stderr.String()))
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if m := totalsRE.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			return n, nil
		}
	}
	return 0, nil
}

// Restore consumes a raw tar stream and extracts into destDir. With no members it
// extracts the whole archive in listed-incremental mode, applying the deletions
// recorded in the archive (a chain restore). With members it extracts only those
// named entries in plain mode — selected-file recovery, which never deletes.
func (g *gnutar) Restore(in io.Reader, destDir string, members []string) error {
	if err := g.Check(); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	args := []string{
		"--extract", "--file=-",
		"--directory=" + destDir,
		"--numeric-owner",
	}
	if len(members) == 0 {
		// Whole-archive chain restore: honor the incremental dumpdir so deletions
		// recorded since the base are applied.
		args = append(args, "--listed-incremental=/dev/null")
	} else {
		// Selected files: match the exact members and do not apply deletions.
		// --no-recursion makes each named member match only itself, so listing a
		// directory alongside its files (we enumerate every descendant ourselves)
		// doesn't double-match and spuriously report the files "not found".
		args = append(args, "--no-recursion")
		args = append(args, members...)
	}
	cmd := exec.Command(g.bin, args...)
	cmd.Stdin = in
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if isWarning(err) {
			return nil
		}
		return fmt.Errorf("%s extract failed: %w\n%s", g.bin, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// List reads a raw tar stream from in and returns its member paths (`tar -t`),
// without extracting. It is the structural half of a deep verify: the pipeline
// completing cleanly proves the stream is a valid, listable archive, and the
// returned members compare directly against the seal (tar lists the same stored
// names the create-time --index-file recorded). Exit-1 ("some files changed") is a
// warning, not a failure. The caller owns draining/closing in; List only reads what
// tar consumes.
func (g *gnutar) List(in io.Reader) ([]string, error) {
	if err := g.Check(); err != nil {
		return nil, err
	}
	cmd := exec.Command(g.bin, "--list", "--file=-")
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

// seedSnapshot prepares outSnap as the starting incremental state for the dump:
// a copy of the base level's snapshot for an incremental (so the real base file
// is never mutated — tar updates outSnap in place), or an absent file for a full
// so tar starts fresh.
func (g *gnutar) seedSnapshot(r archiver.BackupRequest, outSnap string) error {
	if err := os.MkdirAll(filepath.Dir(outSnap), 0o755); err != nil {
		return err
	}
	if r.Level == 0 || r.BaseLevel < 0 {
		if err := os.Remove(outSnap); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return copyFile(g.snapPath(r.DLE, r.BaseLevel), outSnap)
}

// createArgs builds the shared tar create-mode argument list. fileTarget is "-"
// for a streamed backup or "/dev/null" for an estimate; indexPath, when set, adds
// the verbose member index (backup only).
func (g *gnutar) createArgs(r archiver.BackupRequest, fileTarget, snapshot, indexPath string) []string {
	args := []string{
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

func (g *gnutar) runCreate(r archiver.BackupRequest, w io.Writer, snapshot, indexPath string) (int64, error) {
	cmd := exec.Command(g.bin, g.createArgs(r, "-", snapshot, indexPath)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", g.bin, err)
	}

	totalsCh := make(chan int64, 1)
	diagCh := make(chan string, 1)
	go func() {
		var total int64
		var diag strings.Builder
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if m := totalsRE.FindStringSubmatch(line); m != nil {
				total, _ = strconv.ParseInt(m[1], 10, 64)
				continue
			}
			diag.WriteString(line)
			diag.WriteByte('\n')
		}
		totalsCh <- total
		diagCh <- diag.String()
	}()

	_, copyErr := io.Copy(w, stdout)
	if copyErr != nil {
		// The consumer (a full volume that cannot span, a failed write) stopped
		// reading, so tar is now blocked writing to its stdout pipe. Kill it so its
		// stderr closes and the scan goroutine and Wait below return — otherwise this
		// deadlocks waiting on a child that can never make progress.
		_ = cmd.Process.Kill()
	}
	total := <-totalsCh
	diag := <-diagCh
	waitErr := cmd.Wait()
	if copyErr != nil {
		return 0, fmt.Errorf("read tar output: %w", copyErr)
	}
	if waitErr != nil {
		if isWarning(waitErr) {
			return total, nil
		}
		return 0, fmt.Errorf("%s failed: %w\n%s", g.bin, waitErr, strings.TrimSpace(diag))
	}
	return total, nil
}

// isWarning reports whether a tar exit was a non-fatal warning (exit code 1:
// "some files differ / changed as we read them").
func isWarning(err error) bool {
	ee, ok := err.(*exec.ExitError)
	return ok && ee.ExitCode() == 1
}

func readIndex(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return scanMembers(f)
}

// scanMembers reads a newline-separated member listing (a tar --index-file, or
// `tar -t` output) and returns the member tokens, dropping blanks and the bare "./"
// root entry. Shared by Backup's index read and List so both normalize identically,
// keeping the seal's members and a deep verify's listing directly comparable.
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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
