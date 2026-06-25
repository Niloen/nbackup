package tape

import (
	"fmt"

	"github.com/Niloen/nbackup/internal/media"
)

// reelPrefix names the room's reels. A manual station has no bays (just the one
// drive); its reels are identified by their own ids — reel-01, reel-02, …
const reelPrefix = "reel-"

// reelName is the directory name of the i-th reel in the room (1-based).
func reelName(i int) string { return fmt.Sprintf("%s%02d", reelPrefix, i) }

// manualStation emulates a single-drive tape station: one physical drive an
// operator loads by hand from an offline shelf of reels. On disk it reuses the
// shared dirLibrary layout (reel-NN subdirectories + the .loaded marker), but the
// software sees only the loaded reel — the others sit on the shelf, invisible until
// loaded, exactly as a lone drive cannot read reels that are not in it. Inserting a
// reel changes the one drive's content; reels are addressed by their own ids, never
// a fixed "drive" position. It backs a shelfChanger (a robotic library is a
// dirChanger). It is NOT a media.Changer — hence "Station", not "Changer".
type manualStation struct{ *dirLibrary }

func openManualStation(root string, capacity int64, reels int) (*manualStation, error) {
	lib, err := openDirLibrary(root, capacity, reelPrefix, reels)
	if err != nil {
		return nil, err
	}
	return &manualStation{lib}, nil
}

// loaded reports the reel in the drive by its own id (reel-NN), and the device
// behind it. ok is false only when the drive is empty.
func (c *manualStation) loaded() (device, string, bool) { return c.loadedDevice() }

// loadedStatus inventories the reel currently in the drive; ok is false when empty.
func (c *manualStation) loadedStatus() (media.VolumeStatus, bool) {
	c.mu.Lock()
	loadedReel := c.dirLibrary.loaded
	c.mu.Unlock()
	if loadedReel == "" {
		return media.VolumeStatus{}, false
	}
	st, err := c.entryStatus(loadedReel)
	if err != nil {
		return media.VolumeStatus{}, false
	}
	return st, true
}

// shelf lists every reel in the room except the one in the drive, each by its
// physical reel id (its durable identity; the operator types this or its label).
func (c *manualStation) shelf() ([]media.VolumeStatus, error) { return c.entries(true) }

// shelfChanger is the disk-emulated single-drive station: a media.Drive (the loaded
// reel) plus a media.Shelf — its room of reels are directories the software can
// enumerate (Shelf) and load (Insert), so the manual-swap UX runs in one process. It
// is NOT a media.Changer: a single drive has no robot and no bays. The robotic
// library (roboticChanger) is the opposite — a Changer with no Shelf.
type shelfChanger struct {
	*tape
	mc *manualStation
}

// Loaded reports the reel in the drive (media.Drive); ok is false when empty.
func (m *shelfChanger) Loaded() (media.VolumeStatus, bool) { return m.mc.loadedStatus() }

// Shelf lists the reels available to load (media.Shelf).
func (m *shelfChanger) Shelf() ([]media.VolumeStatus, error) { return m.mc.shelf() }

// Insert swaps a shelf reel into the single drive (media.Shelf): the drive's content
// changes, and subsequent Volume/Labeled ops act on the new reel.
func (m *shelfChanger) Insert(reel string) error {
	dev, err := m.mc.mount(reel)
	if err != nil {
		return err
	}
	m.dev, m.bay = dev, reel
	return nil
}
