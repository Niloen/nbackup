package record

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
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

// Archive describes a single DLE dump — the commit footer that marks the dump complete
// and the metadata a catalog caches. It is self-locating: Slot, DLE, and Level together
// name it uniquely on a volume, so an archive read off the medium carries the slot it
// belongs to without a separate grouping record (its physical position is held by the
// catalog, not here, so the metadata stays portable across volumes). A "slot" is just the
// shared Slot tag a run's archives carry — there is no slot record on the medium.
type Archive struct {
	Slot         string    `json:"slot"`                // the slot (run) this dump belongs to, e.g. "slot-2026-06-21.001"
	DLE          string    `json:"dle"`                 // DLE name, e.g. "app01-home"
	Host         string    `json:"host"`                // source host
	Path         string    `json:"path"`                // source path
	Archiver     string    `json:"archiver"`            // archiver type that produced it
	Compress     string    `json:"compress"`            // compression scheme (zstd|gzip|none); reversed on restore
	Encrypt      string    `json:"encrypt"`             // encryption scheme (gpg|none); reversed on restore. "none" = plaintext — always concrete, the peer of Compress, so the two transforms describe their off-state identically. The key is never stored — restore resolves it from the operator's keyring.
	Level        int       `json:"level"`               // 0 = full, >=1 = incremental
	Compressed   int64     `json:"compressed"`          // payload size on the volume
	Uncompressed int64     `json:"uncompressed"`        // archive stream size before compression
	FileCount    int       `json:"file_count"`          // number of member entries archived
	SHA256       string    `json:"sha256"`              // checksum of the payload (over the whole stream, across all parts when the archive spans volumes)
	Parts        int       `json:"parts,omitempty"`     // number of parts the payload is split into across volumes (0/1 = a single whole part); the per-part index lives in each file's Header.Part
	BaseSlot     string    `json:"base_slot,omitempty"` // for level>=1, the slot whose state this builds on (a full omits it)
	CreatedAt    time.Time `json:"created_at"`          // when this archive committed (landed) — per-archive, the basis for retention age and the "last archive added" display
	Members      []string  `json:"members,omitempty"`   // member paths archived: slash-separated, directories with a trailing slash (the archiver-neutral convention recovery browses); the raw token is replayed to the producing archiver on extract. Stored in the per-archive index, not the commit footer — omitempty so the footer omits it.
}

// DLEID returns the host:path identity for display, falling back to
// the internal DLE slug when host/path were not recorded.
func (a Archive) DLEID() string {
	if a.Host == "" && a.Path == "" {
		return a.DLE
	}
	return a.Host + ":" + a.Path
}

// A slot is a run's grouping of archives, named by a slot id the run's archives all carry
// (Archive.Slot). It is not a record on the medium: each archive is made durable by its own
// commit footer, so a run is reconstructed by grouping committed archives that share a slot id
// (a crashed run keeps its committed archives; uncommitted parts are orphans). The id is the
// slot's whole identity: "slot-" + Date + a fixed-width ".NNN" sequence (".001" for the day's
// first run) — see IDFromParts. The natural key is a date, so the "slot-" tag is what keeps it
// from reading as a plain date wherever it appears bare: catalog JSON, logs, Archive.BaseSlot
// references, and the on-disk slots/<id>/ directory. The system's other ids need no such tag
// because they are already distinctive words (labels "<medium>-<date>", DLEs "<host>-<path>"),
// not bare dates.

// IDFromParts builds a slot ID from a date string and sequence number. Every run
// is suffixed with a fixed-width, zero-padded sequence (".001" for the day's first
// run) so the ids sort chronologically under a plain lexical compare — even as an
// object-store key with a trailing "/". A bare "slot-DATE" first run would instead
// sort *after* its same-day reruns there, since "." (0x2E) precedes "/" (0x2F); the
// fixed width likewise keeps ".10" from sorting before ".2". The three digits cap a
// day at 999 runs, which a daily backup never approaches. The "slot-" prefix tags an
// otherwise date-shaped key so it never reads as a plain date; ParseID strips it back off.
func IDFromParts(date string, seq int) string {
	return fmt.Sprintf("slot-%s.%03d", date, seq)
}

// DateString formats a date the way slots use it.
func DateString(date time.Time) string {
	return date.Format("2006-01-02")
}

// ParseDateField parses a slot's date (YYYY-MM-DD).
func ParseDateField(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

// ParseID extracts the date and sequence from a slot ID. Every slot id carries an
// explicit, zero-padded sequence (IDFromParts is the sole producer), so a
// sequence-less "slot-DATE" is not a valid id and is rejected — there is one
// canonical id shape, not a tolerated short form.
func ParseID(id string) (date string, seq int, err error) {
	rest, ok := strings.CutPrefix(id, "slot-")
	if !ok {
		return "", 0, fmt.Errorf("not a slot id: %q", id)
	}
	date, seqStr, hasSeq := strings.Cut(rest, ".")
	if !hasSeq {
		return "", 0, fmt.Errorf("slot id %q has no sequence (want slot-DATE.NNN)", id)
	}
	seq, err = strconv.Atoi(seqStr)
	if err != nil {
		return "", 0, fmt.Errorf("bad sequence in slot id %q: %w", id, err)
	}
	return date, seq, nil
}

// SlotDate returns the date (YYYY-MM-DD) encoded in a slot id, or "" if it does not parse.
func SlotDate(id string) string {
	date, _, err := ParseID(id)
	if err != nil {
		return ""
	}
	return date
}

// SlotIDLess reports whether slot id a comes before b in run order, keyed by date then
// sequence (so "slot-DATE.10" correctly follows "slot-DATE.2"). The padded ids sort this
// way lexically too; parsing makes the intent explicit. An id that does not parse (not a
// canonical slot id) falls back to a plain lexical compare.
func SlotIDLess(a, b string) bool {
	da, sa, ea := ParseID(a)
	db, sb, eb := ParseID(b)
	if ea != nil || eb != nil {
		return a < b
	}
	if da != db {
		return da < db
	}
	return sa < sb
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
