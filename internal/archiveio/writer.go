// Package archiveio authors and reads slots on a media.Volume. It owns how a slot maps
// onto a volume's files — one or more part files per archive plus a final seal record
// carrying the slot's metadata — so the engine supplies only an already-transformed
// payload stream and descriptive metadata, never positions or filenames. archiveio knows
// nothing of compression or encryption: it meters (checksum + size) the bytes that land
// and splits them into parts. The transform pipeline (compress/encrypt) is the engine's
// to compose and run; archiveio drains its output.
//
// An archive may be split into several parts across volumes (tape spanning). The writer
// drains the payload into parts sized to fit each volume's known remaining capacity,
// rolling to the next volume (via a VolumeSink) between parts. The split is PROACTIVE —
// each part is bounded before it is written — so a volume is never overfilled in the
// normal path; the media.ErrVolumeFull backstop only fires when an estimate came up short,
// and then the write fails rather than recovering.
package archiveio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// VolumeSink is the writer's view of a medium's changer: where the next part goes,
// how much fits, and how to roll between volumes. The librarian implements it (it
// owns the medium's shape and the roll). The writer never decides to roll — it asks
// for a part and writes it; the sink rolls onto a fresh volume when the loaded one is
// full, so the same call serves both "this volume has room for one more part" and
// "this volume is full, here is the next one".
type VolumeSink interface {
	// NextPart returns the volume to write the next part to and the maximum payload
	// bytes for it (its remaining capacity minus a file header, capped by part_size),
	// rolling onto a fresh volume first if the loaded one is full. A max < 0 means
	// unbounded — write the whole remaining stream as a single part. It errors when a
	// roll is needed but no further writable volume is available.
	NextPart() (vol media.Volume, max int64, volume string, epoch int, err error)
	// PlaceRecord returns the volume to write a small whole record (an archive's member
	// index or its commit footer) of the given payload size to, rolling first if it will
	// not fit the loaded volume.
	PlaceRecord(size int64) (vol media.Volume, volume string, epoch int, err error)
}

// SlotSpec is the identity of a slot to author: the slot id every archive in the run is
// tagged with, plus when authoring began (stamped into each file's header). A slot is just
// that shared tag — the archives carry it and the catalog groups them back — so there is no
// slot record for the Writer to produce.
type SlotSpec struct {
	ID        string    // the slot's identity (see record.IDFromParts)
	CreatedAt time.Time // when authoring began; a copy preserves the source slot's
}

// Writer authors a single slot onto a medium via a VolumeSink. Callers stream each archive's
// payload with NewArchive and finalize it with Commit (which writes the archive's member
// index and its commit footer — the per-archive marker). There is no slot-level seal: a slot
// is the grouping its archives carry in their headers, and a crashed run's committed archives
// survive (uncommitted parts are orphans a scan ignores). The Writer accumulates no slot state —
// each archive is durable and catalogued the moment it commits — so NewArchive is safe for
// concurrent use on an unbounded sink (disk); a bounded, spanning-capable sink rolls one shared
// volume and must be driven serially (the engine clamps archivers).
type Writer struct {
	sink      VolumeSink
	lim       *ratelimit.Limiter // optional bandwidth cap on the bytes landing on the medium (nil = uncapped)
	now       func() time.Time   // clock for per-archive commit timestamps (nil → time.Now)
	slotID    string             // the slot tag every archive in this run carries; read-only after construction
	createdAt time.Time          // when authoring began, stamped into each file's header
}

// NewWriter begins authoring a new slot, described by spec, onto sink. The Writer holds only the
// slot tag and creation time from spec. lim, when non-nil, caps the rate of bytes written to the
// medium (network politeness); a nil lim is uncapped. The same lim is shared across concurrent
// NewArchive writers on an unbounded sink, so several workers to one medium share its budget.
func NewWriter(sink VolumeSink, spec SlotSpec, lim *ratelimit.Limiter, now func() time.Time) *Writer {
	if now == nil {
		now = time.Now
	}
	return &Writer{sink: sink, lim: lim, now: now, slotID: spec.ID, createdAt: spec.CreatedAt}
}

// NewArchive begins writing the archive described by spec onto the slot, pulled part-by-part by
// NextPart (the caller copies up to each part's cap into the returned writer, closes it, and asks for
// the next until the payload is exhausted). Rolling to a fresh volume happens inside NextPart, so a
// spanning medium's roll lands wherever the sink routes it. The payload is metered (sha256 + size) on
// the write path — so the metering runs on the caller's goroutine — and Commit finalizes the footer
// with the producer's totals. archiveio turns the spec plus the metered bytes into the record.Archive;
// tap, if non-nil, gets the running landed byte count.
func (w *Writer) NewArchive(spec ArchiveSpec, tap func(landed int64)) ArchiveWriter {
	meta := record.Archive{
		Slot:     w.slotID,
		DLE:      spec.DLE,
		Host:     spec.Host,
		Path:     spec.Path,
		Archiver: spec.Archiver,
		Compress: spec.Compress,
		Encrypt:  spec.Encrypt,
		Level:    spec.Level,
		BaseSlot: spec.BaseSlot,
	}
	return &archiveWriter{w: w, base: w.archiveHeader(meta), meta: meta, tap: tap, h: sha256.New()}
}

// NewCopy begins re-authoring an already-committed archive onto the slot — the same path as
// NewArchive (pulled part-by-part by NextPart), but for a copy: the bytes are carried raw, so on
// Commit the writer verifies the metered checksum against arch.SHA256 and preserves arch's stats,
// members, and CreatedAt (a copy keeps the source's identity and age); only the part layout, sized to
// this medium's volumes, is new. The spool's holding->backing drain, `nb copy`, and crash-recovery
// Flush all share this one copy path. tap, if non-nil, gets the running landed byte count.
func (w *Writer) NewCopy(arch record.Archive, tap func(landed int64)) ArchiveWriter {
	arch.Slot = w.slotID // re-authored under this writer's slot (the same id, by construction)
	return &archiveWriter{w: w, base: w.archiveHeader(arch), meta: arch, expectSHA: arch.SHA256, tap: tap, h: sha256.New()}
}

// archiveWriter is one archive's part-by-part writer (see NewArchive / NewCopy) — the concrete
// ArchiveWriter. A transfer drives NextPart/Commit, then the caller reads the committed archive and
// its on-medium position from Result to record the placement.
type archiveWriter struct {
	w         *Writer
	base      record.Header
	meta      record.Archive
	expectSHA string // a copy's known digest to verify against ("" for a fresh dump)
	tap       func(int64)
	h         hash.Hash
	n         int64
	parts     []record.FilePos
	part      int

	arch record.Archive    // the committed archive, set by Commit (read via Result)
	pos  record.ArchivePos // its on-medium position, set by Commit (read via Result)
}

var _ ArchiveWriter = (*archiveWriter)(nil)

// NextPart rolls to the next volume and returns a writer for the next part plus its byte cap
// (max < 0 = unbounded). The caller copies up to max bytes into it and Closes it; the part's position
// is recorded on Close. Cancel ctx before Close to abort the part (no committed file).
func (a *archiveWriter) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	vol, max, volName, epoch, err := a.w.sink.NextPart()
	if err != nil {
		return nil, 0, err
	}
	h := a.base
	h.Part = a.part
	a.part++
	fw, err := vol.AppendFile(ctx, h)
	if err != nil {
		return nil, 0, err
	}
	return &archivePartWriter{a: a, fw: fw, dst: a.w.lim.Writer(fw), volName: volName, epoch: epoch}, max, nil
}

// archivePartWriter meters the bytes (sha + size, on the caller's goroutine) then writes them
// through — rate-limited — to the volume. Close records the part's position.
type archivePartWriter struct {
	a       *archiveWriter
	fw      media.FileWriter
	dst     io.Writer
	volName string
	epoch   int
}

func (p *archivePartWriter) Write(b []byte) (int, error) {
	n, err := p.dst.Write(b)
	if n > 0 {
		p.a.h.Write(b[:n])
		p.a.n += int64(n)
		if p.a.tap != nil {
			p.a.tap(p.a.n)
		}
	}
	return n, err
}

func (p *archivePartWriter) Close() error {
	if err := p.fw.Close(); err != nil {
		return err
	}
	p.a.parts = append(p.a.parts, record.FilePos{Label: p.volName, Epoch: p.epoch, Pos: p.fw.Pos()})
	return nil
}

// Commit (xfer.Sink) finalizes the archive: it sets the metered compressed size and the new part
// count, writes the footer + member index, and stashes the committed archive + its on-medium position
// for Result. It records no placement — the caller does, from Result.
//
// A fresh dump derives the rest from this writer and the producer's raw-stream totals (sha, file
// count, uncompressed size, members) and stamps CreatedAt now. A copy (NewCopy) instead verifies the
// metered checksum against the source's known digest and preserves the source's stats, members, and
// CreatedAt — the producer totals are ignored, the bytes being carried raw.
func (a *archiveWriter) Commit(ctx context.Context, p xfer.Produced) error {
	arch := a.meta
	arch.Compressed = a.n
	arch.Parts = len(a.parts)
	if a.expectSHA != "" {
		if got := hex.EncodeToString(a.h.Sum(nil)); got != a.expectSHA {
			return fmt.Errorf("copy of %s L%d checksum mismatch (source corrupt?)", arch.DLE, arch.Level)
		}
		// arch already carries the source's SHA256 / FileCount / Uncompressed / Members / CreatedAt.
	} else {
		arch.SHA256 = hex.EncodeToString(a.h.Sum(nil))
		arch.FileCount = p.FileCount
		arch.Uncompressed = p.Uncompressed
		arch.Members = p.Members
		arch.CreatedAt = a.w.now() // the archive's landing time (per-archive)
	}
	pos, err := a.w.Commit(ctx, arch, a.parts)
	if err != nil {
		return err
	}
	a.arch, a.pos = arch, pos
	return nil
}

// Result returns the committed archive and its on-medium position (parts/footer/index). It is valid
// only after a successful Commit; the caller records the placement from it.
func (a *archiveWriter) Result() (record.Archive, record.ArchivePos) { return a.arch, a.pos }

// Commit durably finalizes an archive (all fields final): it writes the member index (the
// gzip'd Members) then the commit footer (the metadata without members) — the footer last,
// so a crash before it leaves orphan parts a scan ignores. It returns the archive's on-medium
// position (parts/footer/index) for the caller to catalog. The Writer keeps no slot state — the
// footer makes the archive durable and the caller catalogues it from the returned position — so
// concurrent Commits on an unbounded sink need no coordination here. Call it once the caller has
// merged the producer's stats (FileCount/Uncompressed/Members) into the archive.
func (w *Writer) Commit(ctx context.Context, arch record.Archive, parts []record.FilePos) (record.ArchivePos, error) {
	var index record.FilePos
	if len(arch.Members) > 0 {
		var buf bytes.Buffer
		if err := record.EncodeIndex(&buf, arch.Members); err != nil {
			return record.ArchivePos{}, err
		}
		pos, err := w.writeRecord(ctx, record.KindIndex, arch, buf.Bytes())
		if err != nil {
			return record.ArchivePos{}, err
		}
		index = pos
	}
	// The footer omits the member list (it rides in the index); marshal a memberless copy.
	footer := arch
	footer.Members = nil
	data, err := record.MarshalCommit(footer)
	if err != nil {
		return record.ArchivePos{}, err
	}
	commit, err := w.writeRecord(ctx, record.KindCommit, arch, data)
	if err != nil {
		return record.ArchivePos{}, err
	}
	return record.ArchivePos{
		DLE:    arch.DLE,
		Level:  arch.Level,
		Parts:  append([]record.FilePos(nil), parts...),
		Commit: commit,
		Index:  index,
	}, nil
}

// writeRecord places and writes one small whole record (an index or a commit footer) for an
// archive, returning where it landed. The header identifies the archive it belongs to so a
// scan can correlate it with the archive's parts (which may be on other volumes).
func (w *Writer) writeRecord(ctx context.Context, kind string, a record.Archive, payload []byte) (record.FilePos, error) {
	vol, volName, epoch, err := w.sink.PlaceRecord(int64(len(payload)))
	if err != nil {
		return record.FilePos{}, fmt.Errorf("place %s record: %w", kind, err)
	}
	h := record.Header{Slot: w.slotID, Kind: kind, DLE: a.DLE, Level: a.Level, CreatedAt: w.createdAt}
	fw, err := vol.AppendFile(ctx, h)
	if err != nil {
		return record.FilePos{}, err
	}
	_, werr := fw.Write(payload)
	if cerr := fw.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return record.FilePos{}, werr
	}
	return record.FilePos{Label: volName, Epoch: epoch, Pos: fw.Pos()}, nil
}

// archiveHeader builds the base record.Header an archive's parts share (NextPart clones
// it per part with an ascending Part index). Every framing field comes straight from the
// archive's descriptive metadata.
func (w *Writer) archiveHeader(a record.Archive) record.Header {
	return record.Header{
		Slot:      w.slotID,
		Kind:      record.KindArchive,
		DLE:       a.DLE,
		Host:      a.Host,
		Path:      a.Path,
		Archiver:  a.Archiver,
		Compress:  a.Compress,
		Encrypt:   a.Encrypt,
		Level:     a.Level,
		BaseSlot:  a.BaseSlot,
		CreatedAt: w.createdAt,
	}
}

// SlotID returns the id of the slot being authored.
func (w *Writer) SlotID() string { return w.slotID }
