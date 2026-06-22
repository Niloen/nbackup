// Package tape is a placeholder tape Vault medium. It registers the "tape"
// Vault type so secondary copies to tape are selectable and discoverable, but
// operations (and volume spanning via a changer) are not yet implemented.
package tape

import (
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/media"
)

func init() {
	media.RegisterVault("tape", func(opts media.Options) (media.Vault, error) {
		return &vault{device: opts.Get("device")}, nil
	})
}

type vault struct{ device string }

func (v *vault) Name() string { return "tape" }

func ni(op string) error { return fmt.Errorf("tape.%s: %w", op, media.ErrNotImplemented) }

func (v *vault) Put(slotID string, r io.Reader) error     { return ni("Put") }
func (v *vault) Get(slotID string) (io.ReadCloser, error) { return nil, ni("Get") }
func (v *vault) ListSlots() ([]string, error)             { return nil, ni("ListSlots") }
