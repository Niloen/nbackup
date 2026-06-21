// Package slot defines NBackup's primary artifact: an immutable, self-contained
// directory describing a single backup run.
package slot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// FileSlot is the slot metadata file written last to seal a slot.
	FileSlot = "SLOT.json"
	// FileManifest enumerates the archives and their contents.
	FileManifest = "MANIFEST.json"
	// FileChecksums lists sha256 checksums of every archive file.
	FileChecksums = "CHECKSUMS.sha256"
	// DirArchives holds the tar.zst archive files.
	DirArchives = "archives"

	// StatusOpen marks a slot whose creation is in progress.
	StatusOpen = "open"
	// StatusSealed marks an immutable, complete slot.
	StatusSealed = "sealed"
)

// Slot is the content of SLOT.json.
type Slot struct {
	ID         string    `json:"id"`          // e.g. "slot-2026-06-21"
	Date       string    `json:"date"`        // run date, YYYY-MM-DD
	CreatedAt  time.Time `json:"created_at"`  // when creation started
	SealedAt   time.Time `json:"sealed_at"`   // when sealed (zero if open)
	Status     string    `json:"status"`      // open | sealed
	Generator  string    `json:"generator"`   // tool that produced the slot
	Archives   []Archive `json:"archives"`    // one entry per DLE backed up
	TotalBytes int64     `json:"total_bytes"` // sum of compressed archive sizes
}

// Archive describes a single tar.zst within a slot.
type Archive struct {
	DLE          string `json:"dle"`           // DLE name, e.g. "app01-home"
	Host         string `json:"host"`          // source host
	Path         string `json:"path"`          // source path
	Level        int    `json:"level"`         // 0 = full, >=1 = incremental
	File         string `json:"file"`          // path relative to slot root
	Compressed   int64  `json:"compressed"`    // size on disk
	Uncompressed int64  `json:"uncompressed"`  // sum of file sizes archived
	FileCount    int    `json:"file_count"`    // number of regular files
	SHA256       string `json:"sha256"`        // checksum of the archive file
	BaseSlot     string `json:"base_slot"`     // for level>=1, the L0 slot ID it builds on
}

// Manifest is the content of MANIFEST.json: per-archive file listings.
type Manifest struct {
	SlotID   string         `json:"slot_id"`
	Archives []ArchiveFiles `json:"archives"`
}

// ArchiveFiles lists the files contained in one archive.
type ArchiveFiles struct {
	DLE   string  `json:"dle"`
	Level int     `json:"level"`
	Files []Entry `json:"files"`
}

// Entry is a single file recorded in the manifest.
type Entry struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Mode    uint32    `json:"mode"`
}

// ID builds a slot ID from a date.
func ID(date time.Time) string {
	return "slot-" + date.Format("2006-01-02")
}

// DateString formats a date the way slots use it.
func DateString(date time.Time) string {
	return date.Format("2006-01-02")
}

// ParseDateField parses a slot's Date field (YYYY-MM-DD).
func ParseDateField(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

// Write serializes the slot metadata into the slot directory, sealing it.
func (s *Slot) Write(dir string) error {
	return writeJSON(filepath.Join(dir, FileSlot), s)
}

// WriteManifest serializes the manifest into the slot directory.
func (m *Manifest) Write(dir string) error {
	return writeJSON(filepath.Join(dir, FileManifest), m)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// Read loads SLOT.json from a slot directory.
func Read(dir string) (*Slot, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileSlot))
	if err != nil {
		return nil, err
	}
	var s Slot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileSlot, err)
	}
	return &s, nil
}

// ReadManifest loads MANIFEST.json from a slot directory.
func ReadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileManifest))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileManifest, err)
	}
	return &m, nil
}

// WriteChecksums writes the CHECKSUMS.sha256 file in the standard
// "<hex>  <path>" format understood by sha256sum.
func WriteChecksums(dir string, sums map[string]string) error {
	paths := make([]string, 0, len(sums))
	for p := range sums {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "%s  %s\n", sums[p], p)
	}
	return os.WriteFile(filepath.Join(dir, FileChecksums), []byte(b.String()), 0o644)
}

// ReadChecksums parses CHECKSUMS.sha256 into a path->hex map.
func ReadChecksums(dir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileChecksums))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed checksum line: %q", line)
		}
		out[parts[1]] = parts[0]
	}
	return out, nil
}

// IsSealed reports whether a slot directory contains a sealed slot.
func IsSealed(dir string) bool {
	s, err := Read(dir)
	return err == nil && s.Status == StatusSealed
}

// List returns the sealed (and open) slots found under a catalog root,
// sorted ascending by date.
func List(catalog string) ([]*Slot, error) {
	entries, err := os.ReadDir(catalog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var slots []*Slot
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "slot-") {
			continue
		}
		s, err := Read(filepath.Join(catalog, e.Name()))
		if err != nil {
			continue // skip unreadable/partial slots
		}
		slots = append(slots, s)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Date < slots[j].Date })
	return slots, nil
}
