// Package s3 is a placeholder S3 medium. It registers the "s3" Volume type so the
// medium is selectable and discoverable, but operations are not yet implemented.
// This proves the media seam without committing to an S3 client.
package s3

import (
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/media"
)

func init() {
	media.RegisterVolume("s3", func(opts media.Options) (media.Volume, error) {
		bucket := opts.Get("bucket")
		if bucket == "" {
			return nil, fmt.Errorf("s3 medium requires a bucket")
		}
		return &volume{bucket: bucket}, nil
	})
	media.RegisterProfile("s3", media.NewSizeProfile)
}

type volume struct{ bucket string }

func (v *volume) Name() string { return "s3" }

func ni(op string) error { return fmt.Errorf("s3.%s: %w", op, media.ErrNotImplemented) }

func (v *volume) AppendFile(media.Header, func(io.Writer) error) (int, error) {
	return 0, ni("AppendFile")
}
func (v *volume) ReadFile(int) (media.Header, io.ReadCloser, error) {
	return media.Header{}, nil, ni("ReadFile")
}
func (v *volume) Files() ([]media.FileInfo, error) { return nil, ni("Files") }
func (v *volume) RemoveSlot(string) error          { return ni("RemoveSlot") }
