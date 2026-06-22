// Package media is NBackup's storage abstraction, analogous to Amanda's Device
// API. A Volume is a linear medium: an ordered sequence of self-describing files,
// each a Header followed by a payload, addressed by position (file number). This
// one shape maps to a local directory, an object store, or a tape (file marks +
// fast-forward). The medium owns its physical layout — callers never construct
// filenames — so slots can be streamed between volumes (disk <-> tape) uniformly.
// Implementations register themselves, so selecting a medium is a registry lookup.
package media

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
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
	Method    string    `json:"method,omitempty"`
	Codec     string    `json:"codec,omitempty"`
	Level     int       `json:"level,omitempty"`
	BaseSlot  string    `json:"base_slot,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// FileInfo is a file's position and header, as returned by Files().
type FileInfo struct {
	Pos    int
	Header Header
}

// Options carries medium-specific configuration to a factory as generic
// key/value parameters (e.g. "path" for local-disk, "bucket" for s3).
type Options map[string]string

// Get returns the value for a parameter key, or "".
func (o Options) Get(key string) string { return o[key] }

// Volume is a medium holding an ordered sequence of header-framed files.
type Volume interface {
	Name() string
	// AppendFile writes h, then the payload produced by write, and returns the
	// file's position. The Volume owns concurrency and position assignment
	// (local-disk allows concurrent appends; tape serializes).
	AppendFile(h Header, write func(w io.Writer) error) (pos int, err error)
	// ReadFile positions to pos and returns its header and a payload stream the
	// caller must close.
	ReadFile(pos int) (Header, io.ReadCloser, error)
	// Files returns every file's position and header in order — the volume's
	// self-index, used to rebuild the catalog.
	Files() ([]FileInfo, error)
	// RemoveSlot reclaims every file belonging to a slot.
	RemoveSlot(slot string) error
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
	if i := indexByte(block, '\n'); i >= 0 {
		line = block[:i]
	}
	var h Header
	if err := json.Unmarshal(line, &h); err != nil {
		return Header{}, fmt.Errorf("parse file header: %w", err)
	}
	return h, nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// VolumeFactory constructs a Volume from options.
type VolumeFactory func(Options) (Volume, error)

var volumeFactories = map[string]VolumeFactory{}

// RegisterVolume registers a Volume implementation under a medium type name.
func RegisterVolume(typ string, f VolumeFactory) { volumeFactories[typ] = f }

// OpenVolume constructs the Volume registered for the given medium type.
func OpenVolume(typ string, opts Options) (Volume, error) {
	f, ok := volumeFactories[typ]
	if !ok {
		return nil, fmt.Errorf("unknown medium %q (known: %v)", typ, VolumeTypes())
	}
	return f(opts)
}

// VolumeTypes lists registered medium types.
func VolumeTypes() []string {
	out := make([]string, 0, len(volumeFactories))
	for k := range volumeFactories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrNotImplemented is returned by registered-but-incomplete media.
var ErrNotImplemented = fmt.Errorf("not implemented in this version")

// CopySlot streams every file of a slot from src to dst, in position order
// (archives then the seal record), so the slot lands sealed on dst. Slot metadata
// is position-free, so dst assigns its own positions — this is the one mechanism
// for moving slots between any two media (disk <-> tape). Returns the number of
// files copied.
func CopySlot(dst, src Volume, slotID string) (int, error) {
	files, err := src.Files()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		if f.Header.Slot != slotID {
			continue
		}
		if err := copyOne(dst, src, f); err != nil {
			return n, err
		}
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("slot %s not found on source volume %q", slotID, src.Name())
	}
	return n, nil
}

func copyOne(dst, src Volume, f FileInfo) error {
	_, rc, err := src.ReadFile(f.Pos)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = dst.AppendFile(f.Header, func(w io.Writer) error {
		_, e := io.Copy(w, rc)
		return e
	})
	return err
}
