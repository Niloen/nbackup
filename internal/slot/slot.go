// Package slot defines NBackup's primary artifact format: the metadata of an
// immutable, self-contained backup run. It is pure data plus (de)serialization;
// it makes no assumptions about where the bytes live (that is a media concern).
package slot

import (
	"encoding/json"
	"fmt"
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
	Method       string `json:"method"`       // dump method that produced it
	Codec        string `json:"codec"`        // compression codec (zstd|gzip|none); reversed on restore
	Level        int    `json:"level"`        // 0 = full, >=1 = incremental
	File         string `json:"file"`         // path relative to slot root
	Compressed   int64  `json:"compressed"`   // size on disk
	Uncompressed int64  `json:"uncompressed"` // archive stream size before compression
	FileCount    int    `json:"file_count"`   // number of member entries archived
	SHA256       string `json:"sha256"`       // checksum of the archive file
	BaseSlot     string `json:"base_slot"`    // for level>=1, the slot whose state this builds on
}

// Manifest is the content of MANIFEST.json: per-archive member listings.
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

// NewSlot starts a new open slot for a run. Archives are added with AddArchive
// and the slot is finalized with Seal; callers should not set the lifecycle
// fields (Status, timestamps, TotalBytes) directly.
func NewSlot(id, date string, seq int, generator string, now time.Time) *Slot {
	return &Slot{
		ID:        id,
		Date:      date,
		Sequence:  seq,
		CreatedAt: now,
		Status:    StatusOpen,
		Generator: generator,
	}
}

// AddArchive appends an archive and keeps TotalBytes in sync, so the running
// total can never drift from the recorded archives.
func (s *Slot) AddArchive(a Archive) {
	s.Archives = append(s.Archives, a)
	s.TotalBytes += a.Compressed
}

// Seal marks the slot immutable. It refuses to seal a slot with no archives, so
// an empty run can never be recorded as a recovery point.
func (s *Slot) Seal(now time.Time) error {
	if len(s.Archives) == 0 {
		return fmt.Errorf("cannot seal slot %s: no archives", s.ID)
	}
	s.Status = StatusSealed
	s.SealedAt = now
	return nil
}

// IsSealed reports whether the slot has been sealed.
func (s *Slot) IsSealed() bool { return s.Status == StatusSealed }

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

// Marshal serializes the slot metadata as indented JSON.
func (s *Slot) Marshal() ([]byte, error) { return marshalJSON(s) }

// ParseSlot deserializes SLOT.json content.
func ParseSlot(data []byte) (*Slot, error) {
	var s Slot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileSlot, err)
	}
	if s.Sequence == 0 {
		s.Sequence = 1
	}
	return &s, nil
}

// Marshal serializes the manifest as indented JSON.
func (m *Manifest) Marshal() ([]byte, error) { return marshalJSON(m) }

func marshalJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// FormatChecksums renders a path->hex map in the "<hex>  <path>" format
// understood by sha256sum, sorted by path.
func FormatChecksums(sums map[string]string) []byte {
	paths := make([]string, 0, len(sums))
	for p := range sums {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "%s  %s\n", sums[p], p)
	}
	return []byte(b.String())
}

// ParseChecksums parses CHECKSUMS.sha256 content into a path->hex map.
func ParseChecksums(data []byte) (map[string]string, error) {
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
