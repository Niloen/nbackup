// Package localdisk implements media.Store backed by a local directory. Each
// slot is a directory; each object is a file beneath it. This is the default
// landing medium.
package localdisk

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Niloen/nbackup/internal/media"
)

func init() {
	media.RegisterStore("local-disk", func(opts media.Options) (media.Store, error) {
		path := opts.Get("path")
		if path == "" {
			return nil, fmt.Errorf("local-disk medium requires a path")
		}
		return &store{root: path}, nil
	})
	media.RegisterProfile("local-disk", media.NewSizeProfile)
}

type store struct{ root string }

func (s *store) Name() string { return "local-disk" }

func (s *store) path(slotID, name string) string {
	return filepath.Join(s.root, slotID, filepath.FromSlash(name))
}

func (s *store) Create(slotID, name string) (io.WriteCloser, error) {
	p := s.path(slotID, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	return os.Create(p)
}

func (s *store) Open(slotID, name string) (io.ReadCloser, error) {
	return os.Open(s.path(slotID, name))
}

func (s *store) Stat(slotID, name string) (media.Object, error) {
	fi, err := os.Stat(s.path(slotID, name))
	if err != nil {
		return media.Object{}, err
	}
	return media.Object{Name: name, Size: fi.Size()}, nil
}

func (s *store) List(slotID string) ([]media.Object, error) {
	base := filepath.Join(s.root, slotID)
	var objs []media.Object
	err := filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		objs = append(objs, media.Object{Name: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return objs, nil
}

func (s *store) ListSlots() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "slot-") {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

func (s *store) Remove(slotID string) error {
	return os.RemoveAll(filepath.Join(s.root, slotID))
}
