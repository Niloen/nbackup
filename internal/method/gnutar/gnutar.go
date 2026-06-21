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

	"github.com/Niloen/nbackup/internal/dle"
	"github.com/Niloen/nbackup/internal/method"
)

func init() {
	method.Register("gnutar", func(opts method.Options) (method.Method, error) {
		bin := opts.TarPath
		if bin == "" {
			bin = "tar"
		}
		return &gnutar{bin: bin}, nil
	})
}

type gnutar struct {
	bin       string
	checkOnce sync.Once
	checkErr  error
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

// Estimate runs tar to a discarded output with temporary snapshots, so the real
// snapshot library is untouched.
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
	return g.runCreate(est, io.Discard, tmpSnap, "")
}

// Restore consumes a raw tar stream and extracts it with incremental semantics,
// applying deletions recorded in the archive.
func (g *gnutar) Restore(d dle.DLE, in io.Reader, destDir string) error {
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

func (g *gnutar) runCreate(r method.BackupRequest, w io.Writer, snapshot, indexPath string) (int64, error) {
	args := []string{
		"--create", "--file=-",
		"--directory=" + r.DLE.Path,
		"--one-file-system", "--sparse",
		"--listed-incremental=" + snapshot,
		"--totals",
	}
	if indexPath != "" {
		args = append(args, "--verbose", "--index-file="+indexPath)
	}
	args = append(args, ".")

	cmd := exec.Command(g.bin, args...)
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
