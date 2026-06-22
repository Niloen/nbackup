// Package tape registers the "tape" capacity profile (a volume count × size
// budget) so a tape medium's capacity participates in planning and pruning.
// Tape I/O — writing slots to volumes via a changer — is not yet implemented;
// when it is, it will be a media.Store like any other landing medium, and
// secondary copies will be modeled as copies between Stores.
package tape

import (
	"github.com/Niloen/nbackup/internal/media"
)

func init() {
	media.RegisterProfile("tape", media.NewVolumeProfile)
}
