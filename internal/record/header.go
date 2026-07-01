// Package record defines NBackup's self-describing on-medium artifact records:
// the framing header at the start of every file, the volume label record, the
// per-archive commit footer (Archive) that marks an archive complete, and the
// per-archive member index — plus their (de)serialization. It is pure data and
// scheme; it makes no assumptions about where the bytes live (that is a media
// concern) or how a run is driven (that is the engine's). A volume is recoverable
// on its own because every file leads with one of these records: scanning them
// reconstructs the catalog. There is no per-run seal — a run is the grouping its
// archives carry in their headers.
//
// Alongside the on-medium records it also defines the location types that *point at*
// them — FilePos (a file's volume+position) and ArchivePos (an archive's parts,
// commit, and index positions). These are catalog placements, not bytes on the
// medium: the catalog persists them in its rebuildable workdir cache, and the scanner
// reconstructs them by reading the records above. They live here so the writer (which
// emits the positions) and the catalog (which stores them) share one type.
package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// File kinds carried in a Header.
const (
	KindArchive = "archive" // one DLE dump (one or more part files)
	KindCommit  = "commit"  // a per-archive commit footer, written last — the archive's marker
	KindIndex   = "index"   // a per-archive member index (gzip), written before the commit
	KindLabel   = "label"   // a volume label (first file); not part of any run
)

// Header is the self-describing block at the start of every file on a volume. It is
// a *complete standalone identity record*: it carries the full identity of the file —
// which run/DLE/level it is, the schemes needed to reverse the payload, and the part
// index — so a single payload file is forensically self-describing on its own, even
// detached from its commit footer. A human (or a stock-tool recovery) can read the
// block and know exactly how to restore the bytes that follow: e.g. for an encrypted,
// compressed part, `dd bs=32k skip=1 < file | gpg -d | zstd -dc`, the Encrypt and
// Compress fields naming each stage in order.
//
// Only the *measured* data is deliberately kept out — sizes and checksum live in the
// commit footer (Archive), the member listing in the per-archive index — because none
// of it is known before the payload is streamed. Everything that IS known up front is
// recorded here in full.
//
// The footer (Archive) repeats most of these identity fields: that duplication is by
// design, not redundancy to trim. NBackup's own read path groups parts and reconstructs
// the catalog from a few header fields (Run, Kind, DLE, Level, Part, and Compress for
// the payload extension) and reads the rest of an archive's metadata from the footer;
// the remaining header fields (Host, Path, Archiver, Encrypt, BaseRun, Split, CreatedAt) exist
// for the standalone/forensic story above, so a lone part file is never a mystery.
type Header struct {
	Run       string    `json:"run"`
	Kind      string    `json:"kind"`
	DLE       string    `json:"dle,omitempty"`
	Host      string    `json:"host,omitempty"`
	Path      string    `json:"path,omitempty"`
	Archiver  string    `json:"archiver,omitempty"`
	Compress  string    `json:"compress,omitempty"`
	Encrypt   string    `json:"encrypt,omitempty"` // encryption scheme name (gpg|none); reversed on restore. "none" = plaintext (the peer of Compress, which is likewise always concrete). The key is never recorded — gpg resolves it from the ciphertext + keyring.
	Level     int       `json:"level,omitempty"`
	BaseRun   string    `json:"base_run,omitempty"`
	Part      int       `json:"part,omitempty"`  // 0-based index of this part within its archive (0 = first/only); the archive's total part count lives in its commit footer (Archive.Parts), not here
	Split     bool      `json:"split,omitempty"` // true when this archive is written in size-bounded parts — under a part_size cap (cloud) or across a finite volume's capacity (a spanning reel) — so its payload is one slice (see Part) of a possibly-multi-part whole; concatenate the siblings in Part order before any stock-tool reversal. Set even when the archive needed only one part (the total is not known up front; it lands in the commit footer's Archive.Parts). false = a single standalone payload on an unbounded medium (disk).
	CreatedAt time.Time `json:"created_at"`      // when the run that authored this file began — a per-run stamp shared by every file of a run. NOT this archive's own landing time: that is Archive.CreatedAt in the commit footer (per-archive, the retention-age basis), which for a copied run can differ from the source run's start recorded here.
}

// FileInfo is a file's position and header, as returned by Files().
type FileInfo struct {
	Pos    int
	Header Header
}

// LabelMagic marks a label record as NBackup's, so a foreign or blank volume is
// never mistaken for one of ours (and so never silently overwritten).
const LabelMagic = "nbackup"

// Label is a volume's self-describing identity, stored as the payload of the
// file-0 label record. It is to a volume what a seal record is to a run: the
// on-medium fact a catalog caches. Only media whose physical mount is ambiguous
// (tape — the reel behind the drive can be swapped) carry one; address-identified
// media (disk, s3) do not. The (Pool, Name) pair is the volume's location-
// independent identity — it travels on the cartridge, so moving a tape between
// drives does not change which volume it is.
type Label struct {
	Magic     string    `json:"magic"` // LabelMagic — proves the volume is ours
	Name      string    `json:"name"`  // unique, human-facing (e.g. "lto-0007")
	Pool      string    `json:"pool"`  // the medium/pool name; blocks cross-pool clobber
	Epoch     int       `json:"epoch"` // bumped on every (re)label; detects a stale catalog
	WrittenAt time.Time `json:"written_at"`
}

// HeaderBlock is the fixed size of the leading header block on every file. A
// fixed block makes payload extraction uniform across media and keeps stock-tool
// recovery simple: `dd bs=32k skip=1 < file | zstd -dc`.
const HeaderBlock = 32 * 1024

// EncodeHeader writes h as a fixed-size, newline-terminated JSON block — the
// framing every Volume implementation puts at the start of a file.
func EncodeHeader(w io.Writer, h Header) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > HeaderBlock {
		return fmt.Errorf("header too large (%d > %d)", len(b), HeaderBlock)
	}
	block := make([]byte, HeaderBlock)
	copy(block, b)
	_, err = w.Write(block)
	return err
}

// DecodeHeader reads the fixed header block from r, leaving r positioned at the
// payload.
func DecodeHeader(r io.Reader) (Header, error) {
	block := make([]byte, HeaderBlock)
	if _, err := io.ReadFull(r, block); err != nil {
		return Header{}, err
	}
	line := block
	if i := bytes.IndexByte(block, '\n'); i >= 0 {
		line = block[:i]
	}
	var h Header
	if err := json.Unmarshal(line, &h); err != nil {
		return Header{}, fmt.Errorf("parse file header: %w", err)
	}
	return h, nil
}
