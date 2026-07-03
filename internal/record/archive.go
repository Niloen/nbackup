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
// and the metadata a catalog caches. It is self-locating: Run, DLE, and Level together
// name it uniquely on a volume, so an archive read off the medium carries the run it
// belongs to without a separate grouping record (its physical position is held by the
// catalog, not here, so the metadata stays portable across volumes). A "run" is just the
// shared Run tag a run's archives carry — there is no run record on the medium.
type Archive struct {
	Run          string    `json:"run"`                  // the run this dump belongs to, e.g. "run-2026-06-21.031500"
	DLE          string    `json:"dle"`                  // DLE name, e.g. "app01-home"
	Host         string    `json:"host"`                 // source host
	Path         string    `json:"path"`                 // source path
	Archiver     string    `json:"archiver"`             // archiver type that produced it
	Compress     string    `json:"compress"`             // compression scheme (zstd|gzip|none); reversed on restore
	Encrypt      string    `json:"encrypt"`              // encryption scheme (gpg|none); reversed on restore. "none" = plaintext — always concrete, the peer of Compress, so the two transforms describe their off-state identically. The key is never stored — restore resolves it from the operator's keyring.
	Level        int       `json:"level"`                // 0 = full, >=1 = incremental
	Compressed   int64     `json:"compressed"`           // payload size on the volume
	Uncompressed int64     `json:"uncompressed"`         // archive stream size before compression
	FileCount    int       `json:"file_count"`           // number of member entries archived
	Unreadable   int       `json:"unreadable,omitempty"` // source files the producer could not read, omitted from the archive (a PARTIAL dump — Amanda's "strange"); 0 = complete
	SHA256       string    `json:"sha256"`               // checksum of the payload (over the whole stream, across all parts when the archive spans volumes)
	Parts        int       `json:"parts,omitempty"`      // number of parts the payload is split into across volumes (0/1 = a single whole part); the per-part index lives in each file's Header.Part
	BaseRun      string    `json:"base_run,omitempty"`   // for level>=1, the run whose state this builds on (a full omits it)
	CreatedAt    time.Time `json:"created_at"`           // when this archive committed (landed) — per-archive, the basis for retention age and the "last archive added" display
	Members      []string  `json:"members,omitempty"`    // member paths archived: slash-separated, directories with a trailing slash (the archiver-neutral convention recovery browses); the raw token is replayed to the producing archiver on extract. Stored in the per-archive index, not the commit footer — omitempty so the footer omits it.
}

// Partial reports whether the archive omitted source files it could not read (a PARTIAL
// dump): valid and restorable for what was readable, but not a complete image of the source.
func (a Archive) Partial() bool { return a.Unreadable > 0 }

// DLEID returns the host:path identity for display, falling back to
// the internal DLE slug when host/path were not recorded.
func (a Archive) DLEID() string {
	if a.Host == "" && a.Path == "" {
		return a.DLE
	}
	return a.Host + ":" + a.Path
}

// A run is a grouping of archives, named by a run id the run's archives all carry
// (Archive.Run). It is not a record on the medium: each archive is made durable by its own
// commit footer, so a run is reconstructed by grouping committed archives that share a run id
// (a crashed run keeps its committed archives; uncommitted parts are orphans). The id is the
// run's whole identity: "run-" + the run's local calendar date + a fixed-width ".HHMMSS"
// start-of-run time — see IDFromTime. Like Amanda's dump datestamp (YYYYMMDDHHMMSS), the id
// comes from the clock, so it is never allocated against existing state and never reused:
// a pruned run's id stays retired, and ids sort as time by construction. The natural key is
// a timestamp, so the "run-" tag is what keeps it from reading as a plain date wherever it
// appears bare: catalog JSON, logs, Archive.BaseRun references, and the on-disk runs/<id>/
// directory. The system's other ids need no such tag because they are already distinctive
// words (labels "<medium>-<date>", DLEs "<host>-<path>"), not bare dates.

// IDFromTime builds a run ID from the run's start instant, in that instant's own
// location (the caller picks the zone; runs use the operator's local wall clock).
// Every id carries a fixed-width ".HHMMSS" suffix so ids sort chronologically under
// a plain lexical compare — even as an object-store key with a trailing "/". A bare
// "run-DATE" form would instead sort *after* its same-day peers there, since "."
// (0x2E) precedes "/" (0x2F); the fixed width keeps every suffix the same length.
// The "run-" prefix tags an otherwise date-shaped key so it never reads as a plain
// date; ParseID strips it back off.
func IDFromTime(t time.Time) string {
	return "run-" + t.Format("2006-01-02.150405")
}

// IDTime parses a run id back to the instant IDFromTime built it from, interpreted
// in loc (ids carry no zone; runs stamp them in the operator's local zone).
func IDTime(id string, loc *time.Location) (time.Time, error) {
	rest, ok := strings.CutPrefix(id, "run-")
	if !ok {
		return time.Time{}, fmt.Errorf("not a run id: %q", id)
	}
	t, err := time.ParseInLocation("2006-01-02.150405", rest, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad run id %q: %w", id, err)
	}
	return t, nil
}

// DateString formats a date the way runs use it.
func DateString(date time.Time) string {
	return date.Format("2006-01-02")
}

// ParseDateField parses a run's date (YYYY-MM-DD).
func ParseDateField(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

// ParseID extracts the date and the time-of-day suffix (HHMMSS as an integer, so
// 02:00:05 parses as 20005) from a run ID. Every run id carries an explicit,
// fixed-width suffix (IDFromTime is the sole producer), so a suffix-less
// "run-DATE" is not a valid id and is rejected — there is one canonical id
// shape, not a tolerated short form.
func ParseID(id string) (date string, timeOfDay int, err error) {
	rest, ok := strings.CutPrefix(id, "run-")
	if !ok {
		return "", 0, fmt.Errorf("not a run id: %q", id)
	}
	date, suffix, hasSuffix := strings.Cut(rest, ".")
	if !hasSuffix {
		return "", 0, fmt.Errorf("run id %q has no time suffix (want run-DATE.HHMMSS)", id)
	}
	timeOfDay, err = strconv.Atoi(suffix)
	if err != nil {
		return "", 0, fmt.Errorf("bad time suffix in run id %q: %w", id, err)
	}
	return date, timeOfDay, nil
}

// RunDate returns the date (YYYY-MM-DD) encoded in a run id, or "" if it does not parse.
func RunDate(id string) string {
	date, _, err := ParseID(id)
	if err != nil {
		return ""
	}
	return date
}

// RunIDLess reports whether run id a comes before b in run order, keyed by date then
// time-of-day suffix. The fixed-width ids sort this way lexically too; parsing makes
// the intent explicit. An id that does not parse (not a canonical run id) falls back
// to a plain lexical compare.
func RunIDLess(a, b string) bool {
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
