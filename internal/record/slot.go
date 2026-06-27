package record

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

// FilePos is the location of one file on a volume: the label of the volume it is on
// plus a file position. Label is the volume's global, device-independent identity (the
// name on the cartridge); it is empty for address-identified media (disk, s3), which
// carry no label — there the medium is its own sole volume, so no per-file volume id is
// needed. It locates both an archive part (as the archiveio writer emits it) and a
// placement's seal record (as the catalog persists it) — one type both layers share.
type FilePos struct {
	Label string `json:"label,omitempty"` // volume label name; "" for address-identified media
	Epoch int    `json:"epoch,omitempty"` // label epoch when recorded; staleness check on read
	Pos   int    `json:"pos"`
}

// ArchivePos is one archive's identity and the ordered locations of its parts, plus
// where its commit footer and member index landed. An archive that fits one volume has a
// single part; a spanned archive has its compressed payload split into several parts across
// volumes, in order. Commit is the per-archive marker (written last, after the index);
// Index locates the gzip'd member list, read lazily for browse.
type ArchivePos struct {
	DLE    string    `json:"dle"`
	Level  int       `json:"level"`
	Parts  []FilePos `json:"parts"`
	Commit FilePos   `json:"commit"`          // the commit footer's location (the archive's marker)
	Index  FilePos   `json:"index,omitempty"` // the member index's location ("" position = no members)
}

// Slot is a run's grouping of archives — the in-memory unit retention and display work on.
// It is NOT a record on the medium: each archive carries the slot id in its header and is
// made durable by its own commit footer, so a slot is reconstructed by grouping committed
// archives (a crashed run keeps its committed archives; uncommitted parts are orphans). The
// writer builds one up during a run (NewSlot -> AddArchive via Commit -> Finish) and the
// catalog assembles one from a scan; "sealed" now means only "the run finished" (in memory).
//
// The ID is the slot's identity: "slot-" + Date (+ ".Sequence" for the 2nd+ run
// of a day) — see IDFromParts. The natural key is a date, so the "slot-" tag is
// what keeps it from reading as a plain date wherever it appears bare: catalog
// JSON (an id beside an identical date), logs, Archive.BaseSlot references, and
// the on-disk slots/<id>/ directory. The system's other ids need no such tag
// because they are already distinctive words (labels "<medium>-<date>", DLEs
// "<host>-<path>"), not bare dates.
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
	Compress     string   `json:"compress"`          // compression scheme (zstd|gzip|none); reversed on restore
	Encrypt      string   `json:"encrypt,omitempty"` // encryption scheme (gpg); reversed on restore. "" = plaintext. The key is never stored — restore resolves it from the operator's keyring.
	Level        int      `json:"level"`             // 0 = full, >=1 = incremental
	Compressed   int64    `json:"compressed"`        // payload size on the volume
	Uncompressed int64    `json:"uncompressed"`      // archive stream size before compression
	FileCount    int      `json:"file_count"`        // number of member entries archived
	SHA256       string   `json:"sha256"`            // checksum of the payload (over the whole stream, across all parts when the archive spans volumes)
	Parts        int      `json:"parts,omitempty"`   // number of parts the payload is split into across volumes (0/1 = a single whole part); the per-part index lives in each file's Header.Part
	BaseSlot     string   `json:"base_slot"`         // for level>=1, the slot whose state this builds on
	Members      []string `json:"members,omitempty"` // member paths archived: slash-separated, directories with a trailing slash (the archiver-neutral convention recovery browses); the raw token is replayed to the producing archiver on extract. Stored in the per-archive index, not the commit footer — omitempty so the footer omits it.
}

// DLEID returns the host:path identity for display, falling back to
// the internal DLE slug when host/path were not recorded.
func (a Archive) DLEID() string {
	if a.Host == "" && a.Path == "" {
		return a.DLE
	}
	return a.Host + ":" + a.Path
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

// IDFromParts builds a slot ID from a date string and sequence number. The
// "slot-" prefix tags an otherwise date-shaped key so it never reads as a plain
// date (see the Slot.ID doc); ParseID strips it back off.
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

// MarshalCommit serializes an archive's commit footer — its metadata (Members omitempty, so
// clear it first since the member list rides in the separate per-archive index).
func MarshalCommit(a Archive) ([]byte, error) { return marshalJSON(a) }

// ParseCommit deserializes an archive commit footer's payload.
func ParseCommit(data []byte) (*Archive, error) {
	var a Archive
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parse archive commit: %w", err)
	}
	return &a, nil
}

func marshalJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
