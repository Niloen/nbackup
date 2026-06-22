// Package gnutar implements method.Method using the system GNU tar binary in
// listed-incremental mode (the same mechanism Amanda's amgtar uses). It owns all
// tar-specific concerns — flags, snapshot (.snar) files, and the dumpdir-based
// deletion semantics — and produces/consumes a raw tar stream. Compression and
// storage are handled by the caller.
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

	"github.com/Niloen/nbackup/internal/method"
)

func init() {
	method.Register("gnutar", func(opts method.Options) (method.Method, error) {
		bin := opts.Get("tar_path")
		if bin == "" {
			bin = "tar"
		}
		var exclude []string
		for _, p := range strings.Split(opts.Get("exclude"), ",") {
			if p = strings.TrimSpace(p); p != "" {
				exclude = append(exclude, p)
			}
		}
		return &gnutar{
			bin:           bin,
			oneFileSystem: opts.Bool("one-file-system", true),
			sparse:        opts.Bool("sparse", true),
			exclude:       exclude,
		}, nil
	})
}

type gnutar struct {
	bin           string
	oneFileSystem bool
	sparse        bool
	exclude       []string
	checkOnce     sync.Once
	checkErr      error
}

func (g *gnutar) Name() string { return "gnutar" }

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

// Backup writes a raw tar stream to out and updates the snapshot at OutSnap.
func (g *gnutar) Backup(r method.BackupRequest, out io.Writer) (*method.BackupResult, error) {
	if err := g.Check(); err != nil {
		return nil, err
	}
	if err := g.prepareSnapshot(r); err != nil {
		return nil, err
	}

	indexFile, err := os.CreateTemp("", "nbackup-index-*")
	if err != nil {
		return nil, err
	}
	indexPath := indexFile.Name()
	indexFile.Close()
	defer os.Remove(indexPath)

	totals, err := g.runCreate(r, out, r.OutSnap, indexPath)
	if err != nil {
		return nil, err
	}
	members, err := readIndex(indexPath)
	if err != nil {
		return nil, err
	}
	return &method.BackupResult{
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
func (g *gnutar) Estimate(r method.BackupRequest) (int64, error) {
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

	est := r
	est.OutSnap = tmpSnap
	if err := g.prepareSnapshot(est); err != nil {
		return 0, err
	}

	args := g.createArgs(est, "/dev/null", tmpSnap, "")
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

// Restore consumes a raw tar stream and extracts it with incremental semantics,
// applying deletions recorded in the archive.
func (g *gnutar) Restore(in io.Reader, destDir string) error {
	if err := g.Check(); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	cmd := exec.Command(g.bin,
		"--extract", "--file=-",
		"--directory="+destDir,
		"--listed-incremental=/dev/null",
		"--numeric-owner",
	)
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

// prepareSnapshot seeds OutSnap from BaseSnap for incrementals, or removes it
// for a full so tar starts a fresh snapshot.
func (g *gnutar) prepareSnapshot(r method.BackupRequest) error {
	if r.OutSnap == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.OutSnap), 0o755); err != nil {
		return err
	}
	if r.Level == 0 || r.BaseSnap == "" {
		if err := os.Remove(r.OutSnap); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return copyFile(r.BaseSnap, r.OutSnap)
}

// createArgs builds the shared tar create-mode argument list. fileTarget is "-"
// for a streamed backup or "/dev/null" for an estimate; indexPath, when set, adds
// the verbose member index (backup only).
func (g *gnutar) createArgs(r method.BackupRequest, fileTarget, snapshot, indexPath string) []string {
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
	for _, p := range g.exclude {
		args = append(args, "--exclude="+p)
	}
	if indexPath != "" {
		args = append(args, "--verbose", "--index-file="+indexPath)
	}
	return append(args, ".")
}

func (g *gnutar) runCreate(r method.BackupRequest, w io.Writer, snapshot, indexPath string) (int64, error) {
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
	var members []string
	sc := bufio.NewScanner(f)
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
