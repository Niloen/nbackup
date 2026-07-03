// Package disk implements media.Volume backed by a filesystem directory (local or
// networked, e.g. an NFS mount). The run layout — clean payloads plus JSON header
// sidecars under runs/<run>/ — lives in package fslike, shared with the cloud
// medium; this package supplies only the filesystem storage primitives.
package disk

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/media/fslike"
)

func init() {
	// A filesystem directory is an fslike-backed object store, so it inherits the size
	// profile and the concurrent-write capability (eligible as a holding disk) from the
	// shared layout; only the constructor and accepted params are disk-specific.
	s := fslike.Spec()
	s.Type = "disk"
	s.Params = []string{"path", "part_size"}
	s.New = func(opts media.Options) (media.Volume, error) {
		path := opts.Get("path")
		if path == "" {
			return nil, fmt.Errorf("disk medium requires a path")
		}
		// Disk is unbounded, so an archive is always a single part; part_size (the
		// tape-spanning chunk bound) is meaningless here and refused to avoid a
		// silently ignored knob.
		if err := media.RejectPartSize(opts, "disk"); err != nil {
			return nil, err
		}
		// Create the run root up front (like the tape library's openDir) so an
		// uncreatable/unwritable path fails when the medium is opened — e.g. at
		// `nb check` — rather than silently reporting "ready" and only failing once
		// `nb dump` tries to write. fslike.Open's scan otherwise treats a missing
		// root as an empty volume.
		root := filepath.Join(path, "runs")
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, err
		}
		return fslike.Open(fsStore{root: root})
	}
	media.Register(s)
}

// fsStore is a fslike.Store over a local directory. Keys are run-relative paths
// (run/filename); the root holds one subdirectory per run.
type fsStore struct{ root string }

func (s fsStore) Key(run, name string) string { return filepath.Join(run, name) }

func (s fsStore) full(key string) string { return filepath.Join(s.root, key) }

// Writer opens the payload file for streaming. Local writes are not ctx-cancelable mid-write; an
// aborted append (the caller cancels ctx, fslike then skips the header) leaves this partial as a
// sidecar-less orphan a scan ignores, matching the old behavior.
func (s fsStore) Writer(_ context.Context, key string) (io.WriteCloser, error) {
	full := s.full(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, err
	}
	// Create the file read-only (0444) so a committed archive can't be silently
	// overwritten in place — "immutable" is OS-enforced, not just an nb convention.
	// The write here goes through the open fd (mode governs future opens, not this
	// one); the run subdir stays writable so prune can still unlink the file.
	return os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o444)
}

func (s fsStore) WriteAll(_ context.Context, key string, b []byte) error {
	full := s.full(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	// Read-only for the same reason as Writer: a sidecar/index/commit record is
	// immutable once written; prune removes it by unlinking, which the dir permits.
	return os.WriteFile(full, b, 0o444)
}

func (s fsStore) ReadAll(key string) ([]byte, error) { return os.ReadFile(s.full(key)) }

func (s fsStore) Open(key string) (io.ReadCloser, error) { return os.Open(s.full(key)) }

func (s fsStore) RemoveTree(run string) error { return os.RemoveAll(filepath.Join(s.root, run)) }

func (s fsStore) Remove(key string) error {
	if err := os.Remove(s.full(key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s fsStore) List() ([]fslike.Object, error) {
	runs, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []fslike.Object
	for _, sd := range runs {
		if !sd.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.root, sd.Name()))
		if err != nil {
			return nil, err
		}
		for _, fe := range files {
			if fe.IsDir() {
				continue
			}
			out = append(out, fslike.Object{
				Key:  filepath.Join(sd.Name(), fe.Name()),
				Run:  sd.Name(),
				Base: fe.Name(),
			})
		}
	}
	return out, nil
}
