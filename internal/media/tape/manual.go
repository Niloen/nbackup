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

// driveBay is the single, fixed bay id a manual station exposes. Unlike a robotic
// library — where the mounted *position* moves between many bays — a single drive
// has one position whose *content* the operator swaps.
const driveBay = "drive"

// reelPrefix names the room's reels. A manual station has no bays (just the one
// drive); its reels are identified by their own ids — reel-01, reel-02, …
const reelPrefix = "reel-"

// reelName is the directory name of the i-th reel in the room (1-based).
func reelName(i int) string { return fmt.Sprintf("%s%02d", reelPrefix, i) }

// manualChanger emulates a single-drive tape station: one physical drive an
// operator loads by hand from an offline shelf of reels. On disk it reuses the
// library layout (bay-NN subdirectories + the .loaded marker), but presents it as
// the single drive: bays() reports only the loaded reel as "drive", and the other
// reels are the shelf — invisible to the changer's inventory, exactly as a lone
// drive cannot read reels that are not in it. Inserting a reel changes the one
// drive's content; we never switch bay. (A robotic library is a dirChanger.)
type manualChanger struct {
	root      string
	capacity  int64
	mu        sync.Mutex
	loadedBay string // physical reel dir in the drive ("" = drive empty)
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
		c.loadedBay = strings.TrimSpace(string(b))
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
	c.loadedBay = reel
	return dev, nil
}

// loaded reports the single drive: its bay id is always "drive"; the reel behind
// it is whatever was last inserted. ok is false only when the drive is empty.
func (c *manualChanger) loaded() (device, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loadedBay == "" {
		return nil, "", false
	}
	dev, err := openDir(filepath.Join(c.root, c.loadedBay), c.capacity)
	if err != nil {
		return nil, "", false
	}
	return dev, driveBay, true
}

// bays reports the single drive position (the manual-station view of a Changer):
// always one entry, "drive", carrying the loaded reel's status — or a blank drive
// when empty.
func (c *manualChanger) bays() ([]media.BayStatus, error) {
	c.mu.Lock()
	loadedBay := c.loadedBay
	c.mu.Unlock()
	if loadedBay == "" {
		return []media.BayStatus{{Bay: driveBay, Blank: true}}, nil
	}
	st, err := c.reelStatus(loadedBay)
	if err != nil {
		return nil, err
	}
	st.Bay = driveBay
	return []media.BayStatus{st}, nil
}

// shelf lists every reel in the room except the one in the drive, each by its
// physical reel id (its durable identity; the operator types this or its label).
func (c *manualChanger) shelf() ([]media.BayStatus, error) {
	c.mu.Lock()
	loadedBay := c.loadedBay
	c.mu.Unlock()
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, err
	}
	var out []media.BayStatus
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), reelPrefix) || e.Name() == loadedBay {
			continue
		}
		st, err := c.reelStatus(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bay < out[j].Bay })
	return out, nil
}

// reelStatus inventories one reel directory (its label, fill, file count).
func (c *manualChanger) reelStatus(reel string) (media.BayStatus, error) {
	dev, err := openDir(filepath.Join(c.root, reel), c.capacity)
	if err != nil {
		return media.BayStatus{}, err
	}
	n, _ := dev.count()
	st := media.BayStatus{Bay: reel, Capacity: c.capacity, Used: dev.used, Files: n, Blank: n == 0}
	if lbl, ok, _ := readLabel(dev); ok {
		st.Label = lbl.Name
	}
	return st, nil
}

// manualTape is a tape whose changer is a single-drive manual station. It adds the
// operator-swap operations (Shelf/Insert) of media.ManualChanger to the base tape,
// so the engine can prompt for and perform a reel swap; a robotic-library tape
// (plain *tape) deliberately does not satisfy ManualChanger.
type manualTape struct {
	*tape
	mc *manualChanger
}

// Shelf lists the reels available to load (media.ManualChanger).
func (m *manualTape) Shelf() ([]media.BayStatus, error) { return m.mc.shelf() }

// Insert swaps a shelf reel into the single drive (media.ManualChanger): the one
// bay's content changes, and subsequent Volume/Labeled ops act on the new reel.
func (m *manualTape) Insert(reel string) error {
	dev, err := m.mc.mount(reel)
	if err != nil {
		return err
	}
	m.dev, m.bay = dev, driveBay
	return nil
}
