// Package s3 is a placeholder S3 landing medium. It registers the "s3" Store
// type so the medium is selectable and discoverable, but operations are not yet
// implemented. This proves the media seam without committing to an S3 client.
package s3

import (
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/media"
)

func init() {
	media.RegisterStore("s3", func(opts media.Options) (media.Store, error) {
		if opts.Bucket == "" {
			return nil, fmt.Errorf("s3 medium requires a bucket")
		}
		return &store{bucket: opts.Bucket}, nil
	})
}

type store struct{ bucket string }

func (s *store) Name() string { return "s3" }

func ni(op string) error { return fmt.Errorf("s3.%s: %w", op, media.ErrNotImplemented) }

func (s *store) Create(slotID, name string) (io.WriteCloser, error) { return nil, ni("Create") }
func (s *store) Open(slotID, name string) (io.ReadCloser, error)    { return nil, ni("Open") }
func (s *store) Stat(slotID, name string) (media.Object, error)     { return media.Object{}, ni("Stat") }
func (s *store) List(slotID string) ([]media.Object, error)         { return nil, ni("List") }
func (s *store) ListSlots() ([]string, error)                       { return nil, ni("ListSlots") }
func (s *store) Remove(slotID string) error                         { return ni("Remove") }
