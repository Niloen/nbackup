// Package format defines NBackup's self-describing on-medium artifact format:
// the framing header at the start of every file, the volume label record, and the
// per-slot seal metadata (Slot/Archive) — plus their (de)serialization. It is pure
// data and codec; it makes no assumptions about where the bytes live (that is a
// media concern) or how a run is driven (that is the engine's). A volume is
// recoverable on its own because every file leads with one of these records:
// scanning them reconstructs the catalog. Amanda analogue: dumpfile_t + amar.
package format

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// File kinds carried in a Header.
const (
	KindArchive = "archive" // one DLE dump
	KindSeal    = "seal"    // the per-slot metadata record, written last
	KindLabel   = "label"   // a volume label (first file); not part of any slot
)

// Header is the self-describing block at the start of every file on a volume
// (Amanda's dumpfile_t). It carries only identity — what is known before the
// payload is streamed. Measured data (sizes, checksum, member listing) lives in
// the per-slot seal record, not here. A volume is therefore recoverable on its
// own: scanning headers reconstructs the catalog.
type Header struct {
	Slot      string    `json:"slot"`
	Kind      string    `json:"kind"`
	DLE       string    `json:"dle,omitempty"`
	Host      string    `json:"host,omitempty"`
	Path      string    `json:"path,omitempty"`
	Archiver  string    `json:"archiver,omitempty"`
	Codec     string    `json:"codec,omitempty"`
	Encrypt   string    `json:"encrypt,omitempty"` // encryption scheme name (gpg); reversed on restore. "" = plaintext. The key is never recorded — gpg resolves it from the ciphertext + keyring.
	Level     int       `json:"level,omitempty"`
	BaseSlot  string    `json:"base_slot,omitempty"`
	Part      int       `json:"part,omitempty"` // 0-based index of this part within its archive (0 = first/only); the archive's total part count lives in the seal (Archive.Parts), not here
	CreatedAt time.Time `json:"created_at"`
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
// file-0 label record. It is to a volume what a seal record is to a slot: the
// on-medium fact a catalog caches. Only media whose physical mount is ambiguous
// (tape — the reel behind the drive can be swapped) carry one; address-identified
// media (disk, s3) do not. The (Pool, Name) pair is the volume's location-
// independent identity — it travels on the cartridge, so moving a tape between
// drives does not change which volume it is.
type Label struct {
	Magic     string    `json:"magic"`              // LabelMagic — proves the volume is ours
	Name      string    `json:"name"`               // unique, human-facing (e.g. "lto-0007")
	Pool      string    `json:"pool"`               // the medium/pool name; blocks cross-pool clobber
	Sequence  int       `json:"sequence,omitempty"` // ordinal within the pool (optional)
	Epoch     int       `json:"epoch"`              // bumped on every (re)label; detects a stale catalog
	WrittenAt time.Time `json:"written_at"`
}

// HeaderBlock is the fixed size of the leading header block on every file. A
// fixed block (as on Amanda tapes) makes payload extraction uniform across media
// and keeps stock-tool recovery simple: `dd bs=32k skip=1 < file | zstd -dc`.
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
