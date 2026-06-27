// Package disk implements media.Volume backed by a filesystem directory (local or
// networked, e.g. an NFS mount). The slot layout — clean payloads plus JSON header
// sidecars under slots/<slot>/ — lives in package fslike, shared with the cloud
// medium; this package supplies only the filesystem storage primitives.
package disk

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/media/fslike"
)

func init() {
	media.RegisterVolume("disk", func(opts media.Options) (media.Volume, error) {
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
		// Create the slot root up front (like the tape library's openDir) so an
		// uncreatable/unwritable path fails when the medium is opened — e.g. at
		// `nb check` — rather than silently reporting "ready" and only failing once
		// `nb dump` tries to write. fslike.Open's scan otherwise treats a missing
		// root as an empty volume.
		root := filepath.Join(path, "slots")
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, err
		}
		return fslike.Open(fsStore{root: root})
	})
	media.RegisterProfile("disk", media.NewSizeProfile)
	media.RegisterParams("disk", "path", "part_size")
}

// fsStore is a fslike.Store over a local directory. Keys are slot-relative paths
// (slot/filename); the root holds one subdirectory per slot.
type fsStore struct{ root string }

func (s fsStore) Key(slot, name string) string { return filepath.Join(slot, name) }

func (s fsStore) full(key string) string { return filepath.Join(s.root, key) }

func (s fsStore) Write(key string, write func(w io.Writer) error) error {
	full := s.full(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close() // leave the partial as a sidecar-less orphan; scan ignores it
		return err
	}
	return f.Close()
}

func (s fsStore) WriteAll(key string, b []byte) error {
	full := s.full(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, b, 0o644)
}

func (s fsStore) ReadAll(key string) ([]byte, error) { return os.ReadFile(s.full(key)) }

func (s fsStore) Open(key string) (io.ReadCloser, error) { return os.Open(s.full(key)) }

func (s fsStore) RemoveTree(slot string) error { return os.RemoveAll(filepath.Join(s.root, slot)) }

func (s fsStore) Remove(key string) error {
	if err := os.Remove(s.full(key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s fsStore) List() ([]fslike.Object, error) {
	slots, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []fslike.Object
	for _, sd := range slots {
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
				Slot: sd.Name(),
				Base: fe.Name(),
			})
		}
	}
	return out, nil
}
