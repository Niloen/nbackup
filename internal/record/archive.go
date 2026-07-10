package record

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Archive describes a single DLE dump — the commit footer that marks the dump complete
// and the metadata a catalog caches. It is self-locating: Run, DLE, and Level together
// name it uniquely on a volume, so an archive read off the medium carries the run it
// belongs to without a separate grouping record (its physical position is held by the
// catalog, not here, so the metadata stays portable across volumes). A "run" is just the
// shared Run tag a run's archives carry — there is no run record on the medium.
type Archive struct {
	Run          string     `json:"run"`                     // the run this dump belongs to, e.g. "run-2026-06-21.031500"
	DLE          string     `json:"dle"`                     // DLE name, e.g. "app01-home"
	Host         string     `json:"host"`                    // source host
	Path         string     `json:"path"`                    // source path
	ArchiverType string     `json:"archiver"`                // archiver TYPE that produced it (which plugin reverses the stream) — the wire key stays "archiver" so every existing footer/catalog entry parses unchanged
	ArchiverName string     `json:"archiver_name,omitempty"` // the config archiver DEFINITION name the dump resolved (an inert lookup key, never a command): restore prefers it to find the definition's load-bearing options (pipe's restore_command) without scanning config by DLE slug. Additive — absent on old artifacts, which fall back to the slug scan
	Ext          string     `json:"ext,omitempty"`           // the archiver's raw-stream filename extension (gnutar: ".tar"); archive-invariant like Shape, so copies carry it. "" on pre-Ext archives (the media default it to ".tar")
	Compress     string     `json:"compress"`                // compression scheme (zstd|gzip|none); reversed on restore
	Encrypt      string     `json:"encrypt"`                 // encryption scheme (gpg|none); reversed on restore. "none" = plaintext — always concrete, the peer of Compress, so the two transforms describe their off-state identically. The key is never stored — restore resolves it from the operator's keyring.
	Level        int        `json:"level"`                   // 0 = full, >=1 = incremental
	Compressed   int64      `json:"compressed"`              // payload size on the volume
	Uncompressed int64      `json:"uncompressed"`            // archive stream size before compression
	FileCount    int        `json:"file_count"`              // number of member entries archived
	Unreadable   int        `json:"unreadable,omitempty"`    // source files the producer could not read, omitted from the archive (a PARTIAL dump — Amanda's "strange"); 0 = complete
	SHA256       string     `json:"sha256"`                  // checksum of the payload (over the whole stream, across all parts when the archive spans volumes)
	Parts        int        `json:"parts,omitempty"`         // number of parts the payload is split into across volumes (0/1 = a single whole part); the per-part index lives in each file's Header.Part
	BaseRun      string     `json:"base_run,omitempty"`      // for level>=1, the run whose state this builds on (a full omits it)
	CreatedAt    time.Time  `json:"created_at"`              // when this archive committed (landed) — per-archive, the basis for retention age and the "last archive added" display
	Shape        Shape      `json:"shape,omitempty"`         // stream shape (see Shape): how the encoded payload is laid out. Kept in the footer — a reader decodes without config — and archive-invariant across copies (the encoded bytes are carried verbatim).
	Members      []Member   `json:"members,omitempty"`       // members archived, in stream order (see Member); the raw path token is replayed to the producing archiver on extract. Stored in the per-archive index, not the commit footer — omitempty so the footer omits it.
	Frames       []Frame    `json:"frames,omitempty"`        // a framed archive's decode-restart table, in stream order (see Frame). Like Members it rides the per-archive index, never the footer; unlike part seals it is archive-invariant (encoded-stream domain), so copies carry it unchanged.
	Units        []Unit     `json:"units,omitempty"`         // the archive's content inventory in the archiver's vocabulary (see Unit), sorted by Path. Rides the per-archive index like Members, never the footer; archive-invariant, so copies carry it unchanged.
	PartSeals    []PartSeal `json:"part_seals,omitempty"`    // per-part seals, index-aligned with the parts (Header.Part order). Like Parts, a fact about THIS placement's layout (a copy re-splits and re-seals its own parts): the catalog moves them onto the placement's record and strips them from the run's medium-independent content.
	IndexSize    int64      `json:"index_size,omitempty"`    // payload bytes of THIS placement's member-index file (0 = no index). A placement-layout fact like PartSeals; with them it completes what a rebuild scan needs to re-derive a volume's fill without reading payloads (the scan skips index files).
	PartMap      []FilePos  `json:"part_map,omitempty"`      // where each part landed — the archive's TOC, index-aligned with the parts like PartSeals and equally placement-layout. It makes the volume SET self-describing: any one tape holding the footer names every tape (and position) the archive spans, so a rebuild that has not seen a part's tape still records a complete placement and a restore can prompt for the missing reel by label.
	IndexPos     FilePos    `json:"index_pos,omitzero"`      // where THIS placement's member-index file landed (zero = no index) — the TOC's last entry, so a rebuild holding only the footer's tape still knows which reel serves member browsing; sized by IndexSize above.
}

// FilePos is the location of one file on a volume: the label of the volume it is
// on (with the epoch it carried, guarding against a relabel since) plus a file
// position. Label is empty for address-identified media (disk, s3), which carry
// no label — there the medium is its own sole volume. It is the ONE location atom
// every layer shares: the block layer's call vocabulary (archiveio aliases it),
// the catalog's persisted placements, and — since the commit footer carries the
// archive's part map — the on-medium artifact format, which is why it lives here.
type FilePos struct {
	Label string `json:"label,omitempty"`
	Epoch int    `json:"epoch,omitempty"`
	Pos   int    `json:"pos"`
}

// Shape is an archive's stream shape (Archive.Shape, Header.Shape): how the encoded
// payload is laid out. The shape is recorded, never re-derived, so a reader needs no
// config to decode; the zero value (ShapeStream) means exactly the pre-shape behavior
// and selects the unchanged streaming read path. What a shape implies for copying,
// naming, and ranged decoding is read through its properties (Resplittable,
// StandaloneParts, RestartTable), so shape-dependent code states the fact it relies
// on rather than naming a shape.
type Shape string

const (
	// ShapeStream is the default: one opaque encoded stream; parts are slices of it.
	// It is the empty string so every pre-shape record is a stream by construction.
	ShapeStream Shape = ""
	// ShapeFramed is FRAMED-INVISIBLE: the encoder restarted every frame_size of raw
	// input, so the stream carries invisible decode-restart points — byte-identical
	// to a stream for every whole-stream reader and the stock one-liner — and the
	// per-archive index records the frame table enabling ranged reads.
	ShapeFramed Shape = "framed"
	// ShapeAtomic is FRAMED-ATOMIC: a FrameSafe pipeline (a PerFrame stage — gpg —
	// over Full inner stages) whose parts are indivisible sealed atoms, each ONE
	// complete encrypted message on every medium. Copies carry atoms 1:1 and never
	// re-split (re-cutting needs the key); reads decrypt per atom; the stock recovery
	// is a file loop (`for p in …; do gpg -d "$p"; done | zstd -d | tar x`). There is
	// no separate frame table: the per-part seals' cumulative RawSize IS the
	// member→atom map.
	ShapeAtomic Shape = "atomic"
)

// Resplittable reports whether a copy may re-cut the archive's payload into its own
// part layout. Stream and framed parts are slices of one opaque encoded stream, so a
// copy re-splits them to fit its medium's volumes and re-seals its own parts. Atomic
// parts are sealed messages — re-cutting one needs the key — so a copy must carry the
// atoms 1:1, seal for seal, and refuses when it cannot (a target part ceiling below
// an atom's size, or no aligned seals to cut by).
func (s Shape) Resplittable() bool { return s != ShapeAtomic }

// StandaloneParts reports whether each part file is a complete valid file of its type
// (an atom: one whole gpg message) rather than a slice of a multi-part whole. It
// drives the part's on-medium name (the .pNNN index goes before the extensions, so
// tools recognize the file) and the stock recovery style (a per-file loop instead of
// concatenate-then-decode).
func (s Shape) StandaloneParts() bool { return s == ShapeAtomic }

// RestartTable returns the shape's decode-restart table — where a fresh decode may
// start, the basis of ranged reads — or nil when the shape has none (a plain stream)
// or the ingredient it needs was never recorded:
//
//   - framed: the per-archive index's frame table, recorded at encode time.
//   - atomic: the per-part seals' cumulative (RawSize, Size) sums — each atom is a
//     restart point, so the seals ARE the table and no separate one rides the index.
//
// Callers treat nil uniformly as "whole-stream reads only".
func (s Shape) RestartTable(frames []Frame, seals []PartSeal) []Frame {
	switch s {
	case ShapeFramed:
		if len(frames) == 0 {
			return nil
		}
		return frames
	case ShapeAtomic:
		if len(seals) == 0 {
			return nil
		}
		table := make([]Frame, len(seals))
		var raw, enc int64
		for i, seal := range seals {
			if seal.RawSize <= 0 {
				return nil // RawSize never recorded: no member→atom map
			}
			table[i] = Frame{Raw: raw, Enc: enc}
			raw += seal.RawSize
			enc += seal.Size
		}
		return table
	default:
		return nil
	}
}

// Frame is one decode-restart boundary of a framed archive: the raw-stream offset and
// the encoded-stream offset at which a fresh decode may start. Frames are recorded in
// stream order starting at {0, 0}; frame i spans [Raw_i, Raw_{i+1}) raw and
// [Enc_i, Enc_{i+1}) encoded (the last frame runs to the stream's end) — like a member
// list, the order is load-bearing. The JSON form is a compact two-element array
// [raw, enc], so an index document reads as {"frames":[[0,0],[268435456,80530636],…]}.
type Frame struct {
	Raw int64
	Enc int64
}

// MarshalJSON encodes the frame as its documented [raw, enc] pair.
func (f Frame) MarshalJSON() ([]byte, error) { return json.Marshal([2]int64{f.Raw, f.Enc}) }

// UnmarshalJSON decodes the [raw, enc] pair form.
func (f *Frame) UnmarshalJSON(b []byte) error {
	var a [2]int64
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	f.Raw, f.Enc = a[0], a[1]
	return nil
}

// Member is one archive member: its path (slash-separated, directories with a trailing
// slash — the archiver-neutral convention) and its byte offset in the raw archive stream
// (-1 when the producing archiver cannot report offsets). Off points at the FIRST byte
// of everything the stream stores for the member — for tar, the whole header set,
// longname records included — so a splice starting at Off reconstructs the member whole;
// the producing archiver owns delivering that (gnutar repairs tar's one deviation in
// normalizeCreateIndex). A member list is in stream order, and member i's extent is
// [Off_i, Off_{i+1}) — offset-consumers (selective restore's range planning,
// offset-aware structural verify) rely on both invariants, so never reorder a recorded
// list. The JSON keys are terse (p/o) because member lists are the bulk of an archive's
// metadata.
type Member struct {
	Path string `json:"p"`
	Off  int64  `json:"o"`
}

// Unit is one named thing an archive contains, in the producing archiver's own
// vocabulary ("table.postgres.public.users") — an archive-level CONTENT fact,
// distinct from the stream-layout facts (Members, Frames) it rides beside in
// the per-archive index. Units power `nb recover --inventory`, unit-pointing
// selection (`--path`/`add` fall back to unit names), and unit export; they
// are advisory metadata outside every structural comparison (verify's List
// cannot reproduce them from the stream).
type Unit struct {
	// Path is the unit's stable FLAT identity — a kind-first dotted name
	// ("table.<db>.<schema>.<table>"), not a filesystem path — unique within
	// the archive, DISJOINT from the archive's member namespace (the archiver
	// names both, so a pointed-at name is never ambiguous), and built from
	// NAMES (never oids/relfilenodes — those are cluster-lifetime accidents),
	// so inventories diff across runs. An exported unit lands as exactly this
	// identity plus the exporter's extension ("table.postgres.public.users.sql").
	// The vocabulary is the archiver's; the generic layers render, sort, and
	// match it — never parse.
	Path string `json:"path"`
	// Size is the unit's TOTAL size as of this dump, in archiver-defined terms
	// (postgres: pg_table_size — heap+toast+fsm+vm); 0 = unreported. It
	// describes the thing, not its bytes in this archive — an incremental's
	// delta members are small while the table stays its full size.
	Size int64 `json:"size,omitempty"`
	// Members names the raw members in THIS archive carrying the unit's bytes
	// (an incremental lists its delta files) — the cross-reference an expert
	// pulls with `--path`. Empty when the unit is not file-shaped.
	Members []string `json:"members,omitempty"`
}

// PartSeal is the seal of one part file as it lies on a placement: its size and the
// SHA256 of its payload. The whole-archive SHA256 spans every part; per-part seals let a
// sampling check (a drill's cheapest tier) verify one part without reading the rest —
// bounded egress on a cloud copy regardless of archive size.
type PartSeal struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// RawSize is the raw (tar-stream) bytes this part covers — set only for an atomic
	// archive, where each part is a sealed atom: the cumulative raw sizes are the
	// shape's frame table (member offset → covering atom), so no separate index table
	// exists. Unlike Size/SHA256 (per-placement facts), it is archive-invariant —
	// atoms are carried 1:1 by every copy.
	RawSize int64 `json:"raw_size,omitempty"`
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
