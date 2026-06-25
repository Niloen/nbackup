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

// loadedMarker records which entry is currently in the drive, persisted so the
// mounted tape survives across CLI invocations (each opens a fresh handle).
const loadedMarker = ".loaded"

// dirLibrary is the directory-backed core shared by the two emulated shapes: a
// root holding N prefixed subdirectories (one cartridge each, a dirDevice) plus a
// .loaded marker naming the entry in the drive. The robotic library (dirChanger)
// and the single-drive station (manualStation) differ only in their prefix and the
// inventory surface they expose over this core.
type dirLibrary struct {
	root     string
	capacity int64
	prefix   string // subdir prefix: "bay-" (library) or "reel-" (station)
	mu       sync.Mutex
	loaded   string // entry dir in the drive ("" = drive empty)
}

// openDirLibrary stocks root with count blank entries (named prefixNN) and reads
// the persisted loaded marker. count is floored at 1.
func openDirLibrary(root string, capacity int64, prefix string, count int) (*dirLibrary, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if count < 1 {
		count = 1
	}
	for i := 1; i <= count; i++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("%s%02d", prefix, i)), 0o755); err != nil {
			return nil, err
		}
	}
	c := &dirLibrary{root: root, capacity: capacity, prefix: prefix}
	if b, err := os.ReadFile(filepath.Join(root, loadedMarker)); err == nil {
		c.loaded = strings.TrimSpace(string(b))
	}
	return c, nil
}

// mount loads an entry directory into the drive and persists the choice. It is the
// basis of both Mount/Insert (a robot or operator swap) and the label/load flows.
func (c *dirLibrary) mount(entry string) (device, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dir := filepath.Join(c.root, entry)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("no such %s%q in library %s", c.prefix, entry, c.root)
	}
	dev, err := openDir(dir, c.capacity)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(c.root, loadedMarker), []byte(entry), 0o644); err != nil {
		return nil, err
	}
	c.loaded = entry
	return dev, nil
}

// loadedDevice reports the entry in the drive by its own id and the device behind
// it; ok is false only when the drive is empty.
func (c *dirLibrary) loadedDevice() (device, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded == "" {
		return nil, "", false
	}
	dev, err := openDir(filepath.Join(c.root, c.loaded), c.capacity)
	if err != nil {
		return nil, "", false
	}
	return dev, c.loaded, true
}

// entries inventories the prefixed entry directories. When skipLoaded is set the
// entry currently in the drive is omitted (the station's offline shelf); otherwise
// every entry is reported (the library's bays). Results are sorted by id.
func (c *dirLibrary) entries(skipLoaded bool) ([]media.VolumeStatus, error) {
	c.mu.Lock()
	loaded := c.loaded
	c.mu.Unlock()
	dirEntries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, err
	}
	var out []media.VolumeStatus
	for _, e := range dirEntries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), c.prefix) {
			continue
		}
		if skipLoaded && e.Name() == loaded {
			continue
		}
		st, err := c.entryStatus(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// entryStatus inventories one entry directory (its label, fill, file count).
func (c *dirLibrary) entryStatus(entry string) (media.VolumeStatus, error) {
	dev, err := openDir(filepath.Join(c.root, entry), c.capacity)
	if err != nil {
		return media.VolumeStatus{}, err
	}
	return deviceStatus(entry, dev, c.capacity), nil
}
