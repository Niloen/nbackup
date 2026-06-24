package tape

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Niloen/nbackup/internal/media"
)

// reelPrefix names the room's reels. A manual station has no bays (just the one
// drive); its reels are identified by their own ids — reel-01, reel-02, …
const reelPrefix = "reel-"

// reelName is the directory name of the i-th reel in the room (1-based).
func reelName(i int) string { return fmt.Sprintf("%s%02d", reelPrefix, i) }

// manualChanger emulates a single-drive tape station: one physical drive an
// operator loads by hand from an offline shelf of reels. On disk it reuses the
// library layout (reel-NN subdirectories + the .loaded marker), but the software
// sees only the loaded reel — the others sit on the shelf, invisible until loaded,
// exactly as a lone drive cannot read reels that are not in it. Inserting a reel
// changes the one drive's content; reels are addressed by their own ids, never a
// fixed "drive" position. It backs a shelfChanger (a robotic library is a
// dirChanger).
type manualChanger struct {
	root       string
	capacity   int64
	mu         sync.Mutex
	loadedReel string // physical reel dir in the drive ("" = drive empty)
}

func openManualChanger(root string, capacity int64, reels int) (*manualChanger, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if reels < 1 {
		reels = 1
	}
	// Stock the room with the configured number of (initially blank) reels; the
	// operator loads one into the drive when prompted.
	for i := 1; i <= reels; i++ {
		if err := os.MkdirAll(filepath.Join(root, reelName(i)), 0o755); err != nil {
			return nil, err
		}
	}
	c := &manualChanger{root: root, capacity: capacity}
	if b, err := os.ReadFile(filepath.Join(root, loadedMarker)); err == nil {
		c.loadedReel = strings.TrimSpace(string(b))
	}
	return c, nil
}

// mount loads a physical reel directory into the drive and persists the choice.
// It is the basis of both Insert (an operator swap) and the label/load flows.
func (c *manualChanger) mount(reel string) (device, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dir := filepath.Join(c.root, reel)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("no such reel %q in the room at %s", reel, c.root)
	}
	dev, err := openDir(dir, c.capacity)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(c.root, loadedMarker), []byte(reel), 0o644); err != nil {
		return nil, err
	}
	c.loadedReel = reel
	return dev, nil
}

// loaded reports the reel in the drive by its own id (reel-NN), and the device
// behind it. ok is false only when the drive is empty.
func (c *manualChanger) loaded() (device, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loadedReel == "" {
		return nil, "", false
	}
	dev, err := openDir(filepath.Join(c.root, c.loadedReel), c.capacity)
	if err != nil {
		return nil, "", false
	}
	return dev, c.loadedReel, true
}

// loadedStatus inventories the reel currently in the drive; ok is false when empty.
func (c *manualChanger) loadedStatus() (media.VolumeStatus, bool) {
	c.mu.Lock()
	loadedReel := c.loadedReel
	c.mu.Unlock()
	if loadedReel == "" {
		return media.VolumeStatus{}, false
	}
	st, err := c.reelStatus(loadedReel)
	if err != nil {
		return media.VolumeStatus{}, false
	}
	return st, true
}

// shelf lists every reel in the room except the one in the drive, each by its
// physical reel id (its durable identity; the operator types this or its label).
func (c *manualChanger) shelf() ([]media.VolumeStatus, error) {
	c.mu.Lock()
	loadedReel := c.loadedReel
	c.mu.Unlock()
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, err
	}
	var out []media.VolumeStatus
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), reelPrefix) || e.Name() == loadedReel {
			continue
		}
		st, err := c.reelStatus(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// reelStatus inventories one reel directory (its label, fill, file count).
func (c *manualChanger) reelStatus(reel string) (media.VolumeStatus, error) {
	dev, err := openDir(filepath.Join(c.root, reel), c.capacity)
	if err != nil {
		return media.VolumeStatus{}, err
	}
	return deviceStatus(reel, dev, c.capacity), nil
}

// shelfChanger is the disk-emulated single-drive station: a media.Drive (the loaded
// reel) plus a media.Shelf — its room of reels are directories the software can
// enumerate (Shelf) and load (Insert), so the manual-swap UX runs in one process. It
// is NOT a media.Changer: a single drive has no robot and no bays. The robotic
// library (roboticChanger) is the opposite — a Changer with no Shelf.
type shelfChanger struct {
	*tape
	mc *manualChanger
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
