package archiveio

import "github.com/Niloen/nbackup/internal/record"

// pos.go holds the block layer's identity and location value objects — the call
// vocabulary of reading and writing archives, distinct from what a catalog
// persists (package catalog owns its own serialized shape). The location atom
// itself (FilePos) lives in package record — the commit footer's part map made it
// on-medium format — and is aliased here as call vocabulary.

// Ref is the logical identity of one archive — the archive fs's "filename": which
// run, DLE, and level. The write side records it (part headers, catalog); the read
// side resolves it to physical parts and asserts it against each part file's actual
// header before its bytes are trusted — the cheap catch-all against a swapped
// volume or a stale catalog (the header is decoded anyway).
type Ref struct {
	Run   string
	DLE   string
	Level int
}

// FilePos is the shared location atom — one file's volume label (+epoch) and
// position. It lives in package record (the commit footer's part map made it part
// of the on-medium format); this alias keeps it equally the block layer's call
// vocabulary, so writers, readers, and the catalog all spell a location one way.
type FilePos = record.FilePos

// ArchivePos is where one archive landed: the ordered locations of its parts, plus
// where its commit footer and member index went. Pure position — the archive's
// identity (which run/DLE/level) is a Ref, carried separately. An archive that fits
// one volume has a single part; a spanned archive has its compressed payload split
// into several parts across volumes, in order. Commit is the per-archive marker
// (written last, after the index); Index locates the gzip'd member list, read lazily
// for browse (the zero FilePos = no members).
type ArchivePos struct {
	Parts  []FilePos
	Commit FilePos
	Index  FilePos
}
