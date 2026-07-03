package record

// Ref is the logical identity of one archive — the archive fs's "filename": which
// run, DLE, and level. The write side records it (part headers, catalog); the read
// side resolves it to physical parts and asserts it against each part file's actual
// header before its bytes are trusted — the cheap catch-all against a swapped
// volume or a stale catalog (the header is decoded anyway). It lives in record, the
// shared bottom, because it is exactly the identity every part Header carries: the
// block layer (archiveio) asserts it and the file layer (archivefs) resolves it.
type Ref struct {
	Run   string
	DLE   string
	Level int
}
