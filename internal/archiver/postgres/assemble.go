package postgres

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/Niloen/nbackup/internal/archiver"
)

// This file is the browse-time chain assembler: it reconstructs ONE relation
// file from its chain versions, so `nb mount` and selected-file recovery can
// read a file out of an incremental chain without running pg_combinebackup
// (which merges whole clusters, not single files). Whole-DLE restore still
// uses pg_combinebackup itself — the database's own tool stays authoritative —
// and the integration test cross-validates this assembler byte-for-byte
// against its output.
//
// The incremental file format (PostgreSQL 17, src/backend/backup/
// basebackup_incremental.c / pg_combinebackup's reconstruct.c) is:
//
//	uint32 magic (0xd3ae1f0d)
//	uint32 num_blocks
//	uint32 truncation_block_length
//	uint32 block_number[num_blocks]
//	zero padding to the next BLCKSZ boundary (only when num_blocks > 0 —
//	    verified against real PG17 output: a 6-block delta's data starts at
//	    offset 8192; a zero-block stub is the bare 12-byte header)
//	BLCKSZ block[num_blocks]        (in block_number order)
//
// Fields are in the server's native byte order and blocks are BLCKSZ (8192,
// the stock build) — this assembler assumes the common little-endian/8k
// build, and the cross-validation test is what keeps that assumption honest.

const (
	incrementalMagic = 0xd3ae1f0d
	blckSz           = 8192
	incrPrefix       = "INCREMENTAL."
)

// Assembler: incremental dumps store changed relation files as block deltas,
// so chain browse-reads need assembly.
func (p *postgres) Assembler() archiver.Assembler { return assembler{} }

type assembler struct{}

// Logical strips the INCREMENTAL. marker off a delta member's basename:
// "base/16384/INCREMENTAL.2619" names versions of the logical file
// "base/16384/2619". Everything else is stored whole and is itself.
func (assembler) Logical(member string) (string, bool) {
	dir, name := path.Split(member)
	if rest, ok := strings.CutPrefix(name, incrPrefix); ok {
		return dir + rest, true
	}
	return member, false
}

// Assemble reconstructs the file from its chain versions, oldest→newest: a
// whole version replaces the accumulated content outright, a delta truncates
// or extends it to the delta's length and overlays the changed blocks —
// pg_combinebackup's reconstruction, one file at a time. The first version
// must be whole (a file new since the base is stored whole, so a leading
// delta means the chain is broken for this file).
func (assembler) Assemble(versions []archiver.Version) (io.ReadCloser, error) {
	if len(versions) == 0 {
		return nil, fmt.Errorf("postgres assemble: no versions")
	}
	var content []byte
	for i, v := range versions {
		if !v.Delta {
			whole, err := io.ReadAll(v.R)
			if err != nil {
				return nil, fmt.Errorf("postgres assemble: read whole version %d: %w", i, err)
			}
			content = whole
			continue
		}
		if i == 0 {
			return nil, fmt.Errorf("postgres assemble: the oldest chain version is a delta — its whole base is missing from the chain")
		}
		var err error
		content, err = applyDelta(content, v.R)
		if err != nil {
			return nil, fmt.Errorf("postgres assemble: version %d: %w", i, err)
		}
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

// applyDelta overlays one INCREMENTAL delta onto base: the result is
// truncation_block_length blocks of base (zero-extended if base was shorter),
// grown to cover the delta's highest block, with the delta's blocks written
// at their positions.
func applyDelta(base []byte, delta io.Reader) ([]byte, error) {
	var hdr [3]uint32
	if err := binary.Read(delta, binary.LittleEndian, hdr[:]); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if hdr[0] != incrementalMagic {
		return nil, fmt.Errorf("bad magic %#x (not an incremental file)", hdr[0])
	}
	numBlocks, truncTo := int(hdr[1]), int64(hdr[2])
	blocks := make([]uint32, numBlocks)
	if err := binary.Read(delta, binary.LittleEndian, blocks); err != nil {
		return nil, fmt.Errorf("read block numbers: %w", err)
	}
	// Block data starts at the next BLCKSZ boundary after the header (writer
	// pads with zeros) — but only when there are blocks at all.
	if numBlocks > 0 {
		hdrLen := int64(12 + 4*numBlocks)
		if pad := (blckSz - hdrLen%blckSz) % blckSz; pad > 0 {
			if _, err := io.CopyN(io.Discard, delta, pad); err != nil {
				return nil, fmt.Errorf("skip header padding: %w", err)
			}
		}
	}
	lenBlocks := truncTo
	for _, b := range blocks {
		if int64(b)+1 > lenBlocks {
			lenBlocks = int64(b) + 1
		}
	}
	out := make([]byte, lenBlocks*blckSz)
	// Base survives only below the truncation point: the file shrank to truncTo
	// blocks between the backups, so older content past it is dead even where
	// the file has since re-grown (those blocks arrive in the delta, or are
	// genuinely new zero pages).
	keep := int64(len(base))
	if truncTo*blckSz < keep {
		keep = truncTo * blckSz
	}
	copy(out, base[:keep])
	for _, b := range blocks {
		if _, err := io.ReadFull(delta, out[int64(b)*blckSz:(int64(b)+1)*blckSz]); err != nil {
			return nil, fmt.Errorf("read block %d: %w", b, err)
		}
	}
	return out, nil
}
