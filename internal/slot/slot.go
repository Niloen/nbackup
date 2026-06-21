// Package slot defines NBackup's primary artifact: an immutable, self-contained
// directory describing a single backup run.
package slot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	ID         string    `json:"id"`          // e.g. "slot-2026-06-21" or "slot-2026-06-21.2"
	Date       string    `json:"date"`        // run date, YYYY-MM-DD
	Sequence   int       `json:"sequence"`    // 1 for the first run of the day, 2+ for later runs
	CreatedAt  time.Time `json:"created_at"`  // when creation started
	SealedAt   time.Time `json:"sealed_at"`   // when sealed (zero if open)
	Status     string    `json:"status"`      // open | sealed
	Generator  string    `json:"generator"`   // tool that produced the slot
	Archives   []Archive `json:"archives"`    // one entry per DLE backed up
	TotalBytes int64     `json:"total_bytes"` // sum of compressed archive sizes
}

// Archive describes a single tar.zst within a slot.
type Archive struct {
	DLE          string `json:"dle"`          // DLE name, e.g. "app01-home"
	Host         string `json:"host"`         // source host
	Path         string `json:"path"`         // source path
	Level        int    `json:"level"`        // 0 = full, >=1 = incremental (GNU tar listed-incremental)
	File         string `json:"file"`         // path relative to slot root
	Compressed   int64  `json:"compressed"`   // size on disk
	Uncompressed int64  `json:"uncompressed"` // tar stream size (from --totals)
	FileCount    int    `json:"file_count"`   // number of member entries archived
	SHA256       string `json:"sha256"`       // checksum of the archive file
	BaseSlot     string `json:"base_slot"`    // for level>=1, the slot whose state this builds on
}

// Manifest is the content of MANIFEST.json: per-archive member listings as
// produced by GNU tar.
type Manifest struct {
	SlotID   string         `json:"slot_id"`
	Archives []ArchiveFiles `json:"archives"`
}

// ArchiveFiles lists the members contained in one archive.
type ArchiveFiles struct {
	DLE   string   `json:"dle"`
	Level int      `json:"level"`
	Files []string `json:"files"`
}

// ID builds a slot ID from a date and sequence number. Sequence 1 yields the
// bare "slot-DATE"; higher sequences append ".N".
func ID(date time.Time, seq int) string {
	return IDFromParts(DateString(date), seq)
}

// IDFromParts builds a slot ID from a date string and sequence number.
func IDFromParts(date string, seq int) string {
	if seq <= 1 {
		return "slot-" + date
	}
	return fmt.Sprintf("slot-%s.%d", date, seq)
}

// DateString formats a date the way slots use it.
func DateString(date time.Time) string {
	return date.Format("2006-01-02")
}

// ParseDateField parses a slot's Date field (YYYY-MM-DD).
func ParseDateField(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

// ParseID extracts the date and sequence from a slot ID. A bare "slot-DATE" has
// sequence 1.
func ParseID(id string) (date string, seq int, err error) {
	rest, ok := strings.CutPrefix(id, "slot-")
	if !ok {
		return "", 0, fmt.Errorf("not a slot id: %q", id)
	}
	date, seqStr, hasSeq := strings.Cut(rest, ".")
	if !hasSeq {
		return date, 1, nil
	}
	seq, err = strconv.Atoi(seqStr)
	if err != nil {
		return "", 0, fmt.Errorf("bad sequence in slot id %q: %w", id, err)
	}
	return date, seq, nil
}

// Less reports whether slot a comes before slot b in run order, keyed by date
// then sequence (so "slot-DATE.10" correctly follows "slot-DATE.2").
func Less(a, b *Slot) bool {
	if a.Date != b.Date {
		return a.Date < b.Date
	}
	return a.Sequence < b.Sequence
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
	if s.Sequence == 0 {
		s.Sequence = 1
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

// List returns the sealed (and open) slots found under a catalog root, sorted
// ascending by run order (date, then sequence).
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
	sort.Slice(slots, func(i, j int) bool { return Less(slots[i], slots[j]) })
	return slots, nil
}
