// Package slot defines NBackup's primary artifact format: the metadata of an
// immutable, self-contained backup run. It is pure data plus (de)serialization;
// it makes no assumptions about where the bytes live (that is a media concern).
package slot

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// StatusOpen marks a slot whose creation is in progress.
	StatusOpen = "open"
	// StatusSealed marks an immutable, complete slot.
	StatusSealed = "sealed"
)

// Slot is a run's metadata. It is persisted as the payload of the per-slot seal
// record (the last file written to a volume); its presence marks the slot sealed.
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

// Archive describes a single DLE dump within a slot. It is identified on a volume
// by (Slot, DLE, Level); its physical position is held by the catalog, not here,
// so a slot's metadata is portable across volumes.
type Archive struct {
	DLE          string   `json:"dle"`               // DLE name, e.g. "app01-home"
	Host         string   `json:"host"`              // source host
	Path         string   `json:"path"`              // source path
	Archiver     string   `json:"archiver"`          // archiver type that produced it
	Codec        string   `json:"codec"`             // compression codec (zstd|gzip|none); reversed on restore
	Encrypt      string   `json:"encrypt,omitempty"` // encryption scheme (gpg); reversed on restore. "" = plaintext. The key is never stored — restore resolves it from the operator's keyring.
	Level        int      `json:"level"`             // 0 = full, >=1 = incremental
	Compressed   int64    `json:"compressed"`        // payload size on the volume
	Uncompressed int64    `json:"uncompressed"`      // archive stream size before compression
	FileCount    int      `json:"file_count"`        // number of member entries archived
	SHA256       string   `json:"sha256"`            // checksum of the payload (over the whole stream, across all parts when the archive spans volumes)
	Parts        int      `json:"parts,omitempty"`   // number of parts the payload is split into across volumes (0/1 = a single whole part); the per-part index lives in each file's media.Header.Part
	BaseSlot     string   `json:"base_slot"`         // for level>=1, the slot whose state this builds on
	Members      []string `json:"members"`           // member paths archived: slash-separated, directories with a trailing slash (the archiver-neutral convention recovery browses); the raw token is replayed to the producing archiver on extract (was MANIFEST)
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

// ParseSlot deserializes a slot's seal-record payload.
func ParseSlot(data []byte) (*Slot, error) {
	var s Slot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse slot metadata: %w", err)
	}
	if s.Sequence == 0 {
		s.Sequence = 1
	}
	return &s, nil
}

func marshalJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
