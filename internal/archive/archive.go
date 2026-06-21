// Package archive creates and extracts the tar.zst archives stored inside a
// slot. Archives are ordinary tar streams (zstd-compressed) so they remain
// restorable with standard tools.
package archive

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/Niloen/nbackup/internal/slot"
)

// FileMeta records the state of a file at backup time, used to detect changes
// for incremental backups.
type FileMeta struct {
	ModTime time.Time `json:"mod_time"`
	Size    int64     `json:"size"`
}

// Snapshot maps a file's path (relative to the DLE root) to its metadata.
type Snapshot map[string]FileMeta

// CreateOptions configures a single archive creation.
type CreateOptions struct {
	SourcePath string   // absolute path of the DLE root to archive
	OutFile    string   // destination .tar.zst path
	Base       Snapshot // when non-nil, only files newer than the base are archived (incremental)
}

// Result reports what was archived.
type Result struct {
	SHA256       string
	Compressed   int64
	Uncompressed int64
	FileCount    int
	Entries      []slot.Entry
	Snapshot     Snapshot // full snapshot of the source (always captured)
}

// Create writes a tar.zst archive of SourcePath to OutFile. If Base is nil a
// full backup is produced; otherwise only files that are new or modified
// relative to Base are included. A complete snapshot of the source is always
// returned so it can serve as the base for a later incremental.
func Create(opts CreateOptions) (*Result, error) {
	out, err := os.Create(opts.OutFile)
	if err != nil {
		return nil, err
	}
	defer out.Close()

	hasher := sha256.New()
	counter := &countWriter{}
	mw := io.MultiWriter(out, hasher, counter)

	zw, err := zstd.NewWriter(mw)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)

	res := &Result{Snapshot: Snapshot{}}
	incremental := opts.Base != nil

	root := filepath.Clean(opts.SourcePath)
	walkErr := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// Always record regular files in the snapshot of current state.
		if info.Mode().IsRegular() {
			res.Snapshot[rel] = FileMeta{ModTime: info.ModTime(), Size: info.Size()}
		}

		// Decide whether to include this entry in the archive.
		if incremental {
			if !changed(opts.Base, rel, info) {
				return nil
			}
			// In incrementals, skip plain directory entries; tar recreates
			// parent directories of changed files automatically.
			if info.IsDir() {
				return nil
			}
		}

		return writeEntry(tw, p, rel, info, res)
	})
	if walkErr != nil {
		tw.Close()
		zw.Close()
		return nil, walkErr
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}

	sort.Slice(res.Entries, func(i, j int) bool { return res.Entries[i].Path < res.Entries[j].Path })
	res.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	res.Compressed = counter.n
	return res, nil
}

// changed reports whether the file at rel differs from the base snapshot.
func changed(base Snapshot, rel string, info os.FileInfo) bool {
	if !info.Mode().IsRegular() {
		// Non-regular entries (symlinks) are included only on full backups;
		// for simplicity treat them as unchanged in incrementals.
		return false
	}
	prev, ok := base[rel]
	if !ok {
		return true // new file
	}
	return info.Size() != prev.Size || info.ModTime().After(prev.ModTime)
}

func writeEntry(tw *tar.Writer, fullPath, rel string, info os.FileInfo, res *Result) error {
	var link string
	if info.Mode()&os.ModeSymlink != 0 {
		var err error
		link, err = os.Readlink(fullPath)
		if err != nil {
			return err
		}
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	hdr.Name = rel
	if info.IsDir() {
		hdr.Name = rel + "/"
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}

	if info.Mode().IsRegular() {
		f, err := os.Open(fullPath)
		if err != nil {
			return err
		}
		n, err := io.Copy(tw, f)
		f.Close()
		if err != nil {
			return err
		}
		res.FileCount++
		res.Uncompressed += n
		res.Entries = append(res.Entries, slot.Entry{
			Path:    rel,
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Mode:    uint32(info.Mode().Perm()),
		})
	}
	return nil
}

// Extract unpacks a tar.zst archive into destDir.
func Extract(archiveFile, destDir string) error {
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

	tr := tar.NewReader(zr)
	destRoot := filepath.Clean(destDir)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(destRoot, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&os.ModePerm); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&os.ModePerm)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
			_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		}
	}
	return nil
}

// safeJoin prevents path-traversal during extraction.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.Join(root, name))
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	return clean, nil
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

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
