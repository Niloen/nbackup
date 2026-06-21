// Package archive creates and extracts the tar.zst archives stored inside a
// slot. It drives the system GNU tar binary so archives use tar's standard
// listed-incremental format (the same mechanism Amanda uses): incrementals
// carry directory census entries, so deletions propagate on restore and the
// archives remain restorable with stock GNU tar. Compression is applied in
// process with zstd, so no external zstd binary is required.
package archive

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// DefaultTar is the GNU tar binary used when none is configured.
const DefaultTar = "tar"

// CreateOptions configures a single archive creation.
type CreateOptions struct {
	Tar          string // GNU tar binary (default "tar")
	SourcePath   string // directory (DLE root) to archive
	OutFile      string // destination .tar.zst path
	Level        int    // 0 = full, >=1 = incremental
	BaseSnapshot string // for level>=1: snapshot (.snar) of the base level; empty for level 0
	OutSnapshot  string // path to write the updated snapshot for this level
}

// Result reports what was archived.
type Result struct {
	SHA256       string
	Compressed   int64    // bytes written to OutFile
	Uncompressed int64    // tar stream size, from --totals
	FileCount    int      // number of member entries
	Members      []string // member paths as listed by tar
}

var totalsRE = regexp.MustCompile(`Total bytes written: (\d+)`)

// Create writes a tar.zst archive of SourcePath to OutFile using GNU tar's
// listed-incremental mode, and writes the updated snapshot to OutSnapshot.
//
// For level 0, BaseSnapshot is empty and tar produces a full backup while
// creating a fresh snapshot. For higher levels, BaseSnapshot is copied to
// OutSnapshot and tar records only changes since that state, updating the
// snapshot in place.
func Create(opts CreateOptions) (*Result, error) {
	if err := prepareSnapshot(opts); err != nil {
		return nil, err
	}

	out, err := os.Create(opts.OutFile)
	if err != nil {
		return nil, err
	}
	defer out.Close()

	hasher := sha256.New()
	counter := &countWriter{}
	zw, err := zstd.NewWriter(io.MultiWriter(out, hasher, counter))
	if err != nil {
		return nil, err
	}

	indexFile, err := os.CreateTemp("", "nbackup-index-*")
	if err != nil {
		return nil, err
	}
	indexPath := indexFile.Name()
	indexFile.Close()
	defer os.Remove(indexPath)

	totals, err := runTarCreate(opts, zw, opts.OutSnapshot, indexPath)
	if cerr := zw.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}

	members, err := readIndex(indexPath)
	if err != nil {
		return nil, err
	}
	return &Result{
		SHA256:       hex.EncodeToString(hasher.Sum(nil)),
		Compressed:   counter.n,
		Uncompressed: totals,
		FileCount:    countFiles(members),
		Members:      members,
	}, nil
}

// Estimate runs tar in the same listed-incremental mode but discards the output,
// returning the uncompressed bytes that would be archived. It uses temporary
// copies of the snapshots so the real snapshot library is untouched.
func Estimate(opts CreateOptions) (int64, error) {
	tmpOut, err := os.CreateTemp("", "nbackup-estsnap-*")
	if err != nil {
		return 0, err
	}
	tmpSnap := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpSnap)

	est := opts
	est.OutSnapshot = tmpSnap
	if err := prepareSnapshot(est); err != nil {
		return 0, err
	}
	return runTarCreate(est, io.Discard, tmpSnap, "")
}

// prepareSnapshot seeds OutSnapshot from BaseSnapshot for incrementals, or
// ensures it is absent for a full (so tar starts a fresh snapshot).
func prepareSnapshot(opts CreateOptions) error {
	if opts.OutSnapshot == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(opts.OutSnapshot), 0o755); err != nil {
		return err
	}
	if opts.Level == 0 || opts.BaseSnapshot == "" {
		// Full backup: remove any stale snapshot so tar treats it as level 0.
		if err := os.Remove(opts.OutSnapshot); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return copyFile(opts.BaseSnapshot, opts.OutSnapshot)
}

// runTarCreate invokes GNU tar, streaming the archive to w and returning the
// uncompressed byte total reported by --totals. If indexPath is non-empty, the
// member listing is written there.
func runTarCreate(opts CreateOptions, w io.Writer, snapshot, indexPath string) (int64, error) {
	tarBin := opts.Tar
	if tarBin == "" {
		tarBin = DefaultTar
	}
	args := []string{
		"--create", "--file=-",
		"--directory=" + opts.SourcePath,
		"--one-file-system", "--sparse",
		"--listed-incremental=" + snapshot,
		"--totals",
	}
	if indexPath != "" {
		args = append(args, "--verbose", "--index-file="+indexPath)
	}
	args = append(args, ".")

	cmd := exec.Command(tarBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", tarBin, err)
	}

	// Drain stderr concurrently to find the totals line and surface diagnostics.
	totalsCh := make(chan int64, 1)
	errCh := make(chan string, 1)
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
		errCh <- diag.String()
	}()

	copyErr := func() error {
		_, e := io.Copy(w, stdout)
		return e
	}()

	total := <-totalsCh
	diag := <-errCh
	waitErr := cmd.Wait()
	if copyErr != nil {
		return 0, fmt.Errorf("read tar output: %w", copyErr)
	}
	if waitErr != nil {
		// GNU tar exit code 1 means "some files differed/changed during read",
		// which is a warning, not a failure.
		if ee, ok := waitErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return total, nil
		}
		return 0, fmt.Errorf("%s failed: %w\n%s", tarBin, waitErr, strings.TrimSpace(diag))
	}
	return total, nil
}

// Extract unpacks one archive into destDir using GNU tar's incremental
// extraction (--listed-incremental=/dev/null), which applies deletions recorded
// in the archive. Archives must be extracted in chain order (full, then
// incrementals) for deletions to apply correctly.
func Extract(tarBin, archiveFile, destDir string) error {
	if tarBin == "" {
		tarBin = DefaultTar
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	f, err := os.Open(archiveFile)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()

	cmd := exec.Command(tarBin,
		"--extract", "--file=-",
		"--directory="+destDir,
		"--listed-incremental=/dev/null",
		"--numeric-owner",
	)
	cmd.Stdin = zr
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil // warnings only
		}
		return fmt.Errorf("%s extract failed: %w\n%s", tarBin, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// CheckTar verifies that the configured tar binary is GNU tar.
func CheckTar(tarBin string) error {
	if tarBin == "" {
		tarBin = DefaultTar
	}
	out, err := exec.Command(tarBin, "--version").Output()
	if err != nil {
		return fmt.Errorf("cannot run %q: %w (GNU tar is required)", tarBin, err)
	}
	if !strings.Contains(string(out), "GNU tar") {
		return fmt.Errorf("%q is not GNU tar; listed-incremental backups require GNU tar", tarBin)
	}
	return nil
}

// HashFile returns the hex sha256 of a file.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

// countFiles counts members that are not directories (directory entries end
// with "/").
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

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
