package archiveio

// pos.go holds the block layer's identity and location value objects — the call
// vocabulary of reading and writing archives, distinct from the on-medium records
// (package record) and from what a catalog persists (package catalog owns its own
// serialized shape). None of these is itself bytes on a medium.

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

// FilePos is the location of one file on a volume: the label of the volume it is on
// plus a file position. Label is the volume's global, device-independent identity (the
// name on the cartridge); it is empty for address-identified media (disk, s3), which
// carry no label — there the medium is its own sole volume, so no per-file volume id is
// needed. It locates both an archive part (as the writer emits it) and a placement's
// file (as the catalog persists it) — the one location atom both layers share, so it
// carries JSON tags for the catalog's cache.
type FilePos struct {
	Label string `json:"label,omitempty"` // volume label name; "" for address-identified media
	Epoch int    `json:"epoch,omitempty"` // label epoch when recorded; staleness check on read
	Pos   int    `json:"pos"`
}

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
