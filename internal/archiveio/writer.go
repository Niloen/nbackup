// Package archiveio is the block layer: it maps archives onto a media.Volume's files —
// one or more part files per archive plus a final commit footer carrying the archive's
// metadata — so the layers above supply only an already-transformed payload stream and
// descriptive metadata, never positions or filenames. archiveio knows nothing of
// compression or encryption: it meters (checksum + size) the bytes that land and splits
// them into parts. The transform pipeline (compress/encrypt) is the operations' to
// compose and run; archiveio drains its output.
//
// An archive may be split into several parts across volumes (tape spanning). The writer
// drains the payload into parts sized to fit each volume's known remaining capacity,
// rolling to the next volume (via the PartAllocator) between parts. The split is PROACTIVE —
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

// PartAllocator is the writer's device seam — the write-side dual of PartOpener,
// named like it by the exchanged unit (a part slot), not by the implementer's scope.
// It answers where the next part goes, how much fits, and rolls between volumes; no
// bytes ever flow through it (the payload goes into the media.Volume it returns), so
// it is an allocator, never a Sink — "Sink" is reserved for true byte sinks
// (xfer.Sink). The librarian's drive-bound Allocator implements it (it owns the
// medium's shape and the roll); the spool wraps one in a routed allocator so rolls
// serialize on its orchestrator. The writer never decides to roll — it asks for a
// part slot and writes it; the allocator rolls onto a fresh volume when the loaded
// one is full, so the same call serves both "this volume has room for one more part"
// and "this volume is full, here is the next one".
type PartAllocator interface {
	// NextPart returns the volume to write the next part to and the maximum payload
	// bytes for it (its remaining capacity minus a file header, capped by part_size),
	// rolling onto a fresh volume first if the loaded one is full. A max < 0 means
	// unbounded — write the whole remaining stream as a single part. It errors when a
	// roll is needed but no further writable volume is available.
	NextPart() (vol media.Volume, max int64, label string, epoch int, err error)
	// PlaceFile returns the volume to write a small whole file of the given kind
	// (record.Kind*: an archive's member index, its commit footer, or a whole-placed
	// atomic part) and payload size to, rolling first if it will not fit the loaded
	// volume. kind matters because a finite volume prices files by kind (see
	// media.FileCoster): a payload whose size the catalog records is charged as-is,
	// one it does not (the commit footer) is charged at the medium's bound — the
	// placement check must use the same price the fill arithmetic will.
	PlaceFile(kind string, size int64) (vol media.Volume, label string, epoch int, err error)
	// Bounded reports whether this allocator ever caps a part's size — by a configured
	// part_size or by a finite volume's remaining capacity (the dual of NextPart's "max < 0
	// means unbounded"). When true an archive may land as several parts (cloud splitting
	// under a part_size cap, or a finite reel spanning volumes mid-archive), so the writer
	// stamps every part's Header.Split to mark it a slice of one whole — named and read as
	// such, not as a standalone file. When false (disk: no cap, infinite room) each archive
	// is a single, standalone part. It is a property of the medium, constant for the write,
	// so the writer asks once. (This is the same predicate as the medium's spanning
	// capability.)
	Bounded() bool
}

// Recorder is the writer's commit seam — where a finished archive's assembled
// CommitResult crosses back to the file layer (Amanda's taper "DONE"). The fs Session
// implements it (the single catalog write per archive); the spool wraps one in a
// routed recorder so placements serialize on its orchestrator with the rolls. It is
// the whole worker→coordinator crossing: one value, passed by copy, never shared
// writer state. Allocation and recording are deliberately two seams, not one glued
// interface: they have different owners (the device side allocates, the fs side
// records), exactly as a real fs's block allocator is not part of the file handle.
type Recorder interface {
	Record(result CommitResult) error
}

// RunSpec is the identity of a run to author: the run id every archive in the run is
// tagged with, plus when authoring began (stamped into each file's header). A run is just
// that shared tag — the archives carry it and the catalog groups them back — so there is no
// run record for the Writer to produce.
type RunSpec struct {
	ID        string    // the run's identity (see record.IDFromTime)
	CreatedAt time.Time // when authoring began; a copy preserves the source run's
}

// Writer is one run's write end — the mirror of Reader (one medium's read end). It is
// bound at construction to its two seams: alloc places parts on volumes, rec receives
// each committed archive. Callers stream each archive's payload with NewArchive and
// finalize it with Commit (which writes the archive's member index and its commit
// footer — the per-archive marker — then reports it via rec.Record). There is no
// run-level seal: a run is the grouping its archives carry in their headers, and a
// crashed run's committed archives survive (uncommitted parts are orphans a scan
// ignores). The Writer accumulates no run state — each archive is durable and recorded
// the moment it commits — so NewArchive is safe for concurrent use on an unbounded
// medium (disk); a bounded, spanning-capable allocator rolls one shared volume and must
// be driven serially.
type Writer struct {
	alloc     PartAllocator
	rec       Recorder
	lim       *ratelimit.Limiter // optional bandwidth cap on the bytes landing on the medium (nil = uncapped)
	now       func() time.Time   // clock for per-archive commit timestamps (nil → time.Now)
	runID     string             // the run tag every archive in this run carries; read-only after construction
	createdAt time.Time          // when authoring began, stamped into each file's header
}

// NewWriter begins authoring a new run, described by spec, over its two seams: alloc (the
// opened write medium's part allocator — serial: the librarian's drive-bound Allocator;
// concurrent: the spool's routed one) and rec (the fs Session recording each commit —
// inline to the catalog, or routed to the spool's orchestrator). The Writer holds only the
// run tag and creation time from spec. lim, when non-nil, caps the rate of bytes written
// to the medium (network politeness); a nil lim is uncapped. The same lim is shared across
// concurrent NewArchive writers on an unbounded medium, so several workers to one medium
// share its budget.
func NewWriter(alloc PartAllocator, rec Recorder, spec RunSpec, lim *ratelimit.Limiter, now func() time.Time) *Writer {
	if now == nil {
		now = time.Now
	}
	return &Writer{alloc: alloc, rec: rec, lim: lim, now: now, runID: spec.ID, createdAt: spec.CreatedAt}
}

// NewArchive begins writing the archive described by spec onto the run, pulled part-by-part by
// NextPart (the caller copies up to each part's cap into the returned writer, closes it, and asks for
// the next until the payload is exhausted). Rolling to a fresh volume happens inside NextPart, so a
// spanning medium's roll lands wherever the allocator routes it. The payload is metered (sha256 + size)
// on the write path — so the metering runs on the caller's goroutine — and Commit finalizes the
// footer with the producer's totals and reports the placement to the recorder. To observe the running
// landed byte count for live progress, attach a tap with MeterArchive.
func (w *Writer) NewArchive(spec ArchiveSpec) *ArchiveWriter {
	meta := record.Archive{
		Run:      w.runID,
		DLE:      spec.DLE,
		Host:     spec.Host,
		Path:     spec.Path,
		Archiver: spec.Archiver,
		Ext:      spec.Ext,
		Compress: spec.Compress,
		Encrypt:  spec.Encrypt,
		Shape:    spec.Shape,
		Level:    spec.Level,
		BaseRun:  spec.BaseRun,
	}
	aw := &ArchiveWriter{w: w, base: w.archiveHeader(meta), meta: meta, h: sha256.New()}
	if spec.Shape == record.ShapeAtomic {
		// Each atom is placed whole against this bound: the bundle packs compressed
		// frames up to AtomSize, and the seal (gpg) adds a small envelope on top —
		// a proportional margin plus a floor covers it with room to spare.
		aw.atomBound = spec.AtomSize + spec.AtomSize/32 + 64<<10
	}
	return aw
}

// NewCopy begins re-authoring an already-committed archive onto the run — the same path as
// NewArchive (pulled part-by-part by NextPart), but for a copy: the bytes are carried raw, so on
// Commit the writer verifies the metered checksum against arch.SHA256 and preserves arch's stats,
// members, and CreatedAt (a copy keeps the source's identity and age); only the part layout, sized to
// this medium's volumes, is new. The spool's holding->backing drain, `nb copy`, and crash-recovery
// Flush all share this one copy path. Attach a tap with MeterArchive for live progress.
func (w *Writer) NewCopy(arch record.Archive) *ArchiveWriter {
	// arch keeps its own run — a copy preserves the source archive's identity, so one writer can
	// re-author archives from several source runs (a cross-run sync) and file each under its own run.
	// For a per-run copy the writer's run is that same id, so this is unchanged there.
	aw := &ArchiveWriter{w: w, base: w.archiveHeader(arch), meta: arch, expectSHA: arch.SHA256, h: sha256.New()}
	if !arch.Shape.Resplittable() && len(arch.PartSeals) == arch.Parts && arch.Parts > 0 {
		// An atomic copy carries the atoms 1:1, never re-splitting (re-cutting needs
		// the key): the source's per-part seals drive the cut — each part's cap is
		// exactly its atom's size, so the plain Transfer loop reproduces the source's
		// parts bit-for-bit. The caller loads the seals from any placement (they are
		// archive-invariant for atoms).
		aw.copySeals = arch.PartSeals
	}
	return aw
}

// MeterArchive attaches a progress tap to aw and returns it for chaining — the
// free-function form of Meter, for call sites that hold the concrete writer.
func MeterArchive(aw *ArchiveWriter, tap func(landed int64)) *ArchiveWriter {
	aw.Meter(tap)
	return aw
}

// Meter attaches a progress tap to the writer — the running count of landed bytes, reported after
// each write on the writing goroutine. A nil tap is a no-op. The count
// is the same one the writer already meters for the archive's size, so this only exposes it; the
// writer, session, spool, and fs stay otherwise observability-free.
func (a *ArchiveWriter) Meter(tap func(landed int64)) {
	a.tap = tap
}

// ArchiveWriter is one archive's part-by-part write SDK (see NewArchive / NewCopy). A transfer drives
// it as an xfer.Sink: NextPart/Commit do all the byte I/O — headers, payload, footer, member index,
// SHA and size — on the caller's goroutine, and Commit assembles the record and reports it to the
// Writer's Recorder. It is a concrete client of the Writer's two seams; the serial-vs-concurrent
// choice lives entirely in which allocator and recorder the Writer holds, never here.
type ArchiveWriter struct {
	w         *Writer
	base      record.Header
	meta      record.Archive
	expectSHA string // a copy's known digest to verify against ("" for a fresh dump)
	h         hash.Hash
	n         int64
	ph        hash.Hash // the in-flight part's own hash (reset per part); with pn it seals each part
	pn        int64     // the in-flight part's landed bytes
	parts     []FilePos
	seals     []record.PartSeal // one seal per committed part, index-aligned with parts
	atomBound int64             // atomic fresh dump: each part is one atom placed whole against this bound (0 = not atomic)
	copySeals []record.PartSeal // atomic copy: the source's seals — each drives its part's exact cut and carries RawSize
	part      int
	tap       func(int64)   // optional progress tap (MeterArchive); fired on the writing goroutine
	committed *CommitResult // the assembled result, stashed by Commit (nil until then); read via Committed
	onClose   func() error  // optional cleanup run by Close (the spool's per-write run release); nil = none
}

var _ xfer.Sink = (*ArchiveWriter)(nil)

// Committed returns the archive's assembled result and true once Commit has succeeded, or the zero
// value and false before that (or after a faulted transfer). A wrapper that needs to act on a
// finished archive — the spool queuing a holding->landing drain — reads it in the close hook.
func (a *ArchiveWriter) Committed() (CommitResult, bool) {
	if a.committed == nil {
		return CommitResult{}, false
	}
	return *a.committed, true
}

// SetCloser attaches a cleanup fn run by Close on every path — the spool uses it to release the
// per-archive run it leased, so a faulted transfer (which never reaches Commit) still frees it. A
// leaf writer sets none.
func (a *ArchiveWriter) SetCloser(fn func() error) { a.onClose = fn }

// NextPart rolls to the next volume and returns a writer for the next part plus its byte cap
// (max < 0 = unbounded). The caller copies up to max bytes into it and Closes it; the part's position
// is recorded on Close. Cancel ctx before Close to abort the part (no committed file). Only the
// PartAllocator.NextPart alloc may cross to the orchestrator; the AppendFile header and the payload
// writes stay on this goroutine.
func (a *ArchiveWriter) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	vol, max, label, epoch, err := a.nextSlot()
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
	a.ph, a.pn = sha256.New(), 0 // each part carries its own seal beside the whole-archive hash
	return &archivePartWriter{a: a, fw: fw, dst: a.w.lim.Writer(fw), label: label, epoch: epoch}, max, nil
}

// nextSlot allocates the next part's volume and cap per the archive's shape. A slice
// (stream/framed) takes the medium's cut: NextPart's remaining-capacity/part_size cap.
// An atom is placed WHOLE: PlaceFile with its known bound rolls a finite volume
// proactively (remaining < bound → roll first, no split buffer), and the cap is the
// atom's own — a fresh dump's atoms end at the source's seal boundary (uncapped here;
// TransferAtoms drives one part per atom), a copy's at exactly the source part's size.
func (a *ArchiveWriter) nextSlot() (vol media.Volume, max int64, label string, epoch int, err error) {
	switch {
	case a.copySeals != nil: // atomic copy: cut exactly at the source's atom sizes
		if a.part >= len(a.copySeals) {
			return nil, 0, "", 0, fmt.Errorf("atomic copy of %s L%d: stream longer than its %d recorded atom(s) (source corrupt?)", a.meta.DLE, a.meta.Level, len(a.copySeals))
		}
		size := a.copySeals[a.part].Size
		vol, label, epoch, err = a.w.alloc.PlaceFile(record.KindArchive, size)
		return vol, size, label, epoch, err
	case a.atomBound > 0: // atomic fresh dump: whole-atom placement against the bound
		vol, label, epoch, err = a.w.alloc.PlaceFile(record.KindArchive, a.atomBound)
		return vol, -1, label, epoch, err
	}
	return a.w.alloc.NextPart()
}

// archivePartWriter meters the bytes (sha + size, on the caller's goroutine) then writes them
// through — rate-limited — to the volume. Close records the part's position.
type archivePartWriter struct {
	a     *ArchiveWriter
	fw    media.FileWriter
	dst   io.Writer
	label string
	epoch int
}

func (p *archivePartWriter) Write(b []byte) (int, error) {
	n, err := p.dst.Write(b)
	if n > 0 {
		p.a.h.Write(b[:n])
		p.a.ph.Write(b[:n])
		p.a.n += int64(n)
		p.a.pn += int64(n)
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
	p.a.parts = append(p.a.parts, FilePos{Label: p.label, Epoch: p.epoch, Pos: p.fw.Pos()})
	p.a.seals = append(p.a.seals, record.PartSeal{Size: p.a.pn, SHA256: hex.EncodeToString(p.a.ph.Sum(nil))})
	return nil
}

// Commit (xfer.Sink) finalizes the archive: it sets the metered compressed size and the new part
// count, writes the footer + member index, assembles the CommitResult, and reports it to the
// Writer's Recorder — inline to the catalog for the serial fs Session, routed to the orchestrator
// for the concurrent spool. That report is the whole worker→coordinator crossing: one value.
//
// A fresh dump derives the rest from this writer and the producer's raw-stream totals (sha, file
// count, uncompressed size, members) and stamps CreatedAt now. A copy (NewCopy) instead verifies the
// metered checksum against the source's known digest and preserves the source's stats, members, and
// CreatedAt — the producer totals are ignored, the bytes being carried raw.
func (a *ArchiveWriter) Commit(ctx context.Context, p xfer.SourceStats) error {
	arch := a.meta
	arch.Compressed = a.n
	arch.Parts = len(a.parts)
	// The seals describe THIS placement's part layout — like Parts, set for a copy too
	// (overwriting the source's: a copy re-splits to its own medium and re-seals).
	arch.PartSeals = append([]record.PartSeal(nil), a.seals...)
	if a.expectSHA != "" {
		if got := hex.EncodeToString(a.h.Sum(nil)); got != a.expectSHA {
			return fmt.Errorf("copy of %s L%d checksum mismatch (source corrupt?)", arch.DLE, arch.Level)
		}
		// arch already carries the source's SHA256 / FileCount / Uncompressed / Members / CreatedAt.
	} else {
		arch.SHA256 = hex.EncodeToString(a.h.Sum(nil))
		arch.FileCount = p.FileCount
		arch.Unreadable = len(p.Unreadable) // a PARTIAL dump's omitted-file count, durable in the footer so a rebuild preserves it
		arch.Uncompressed = p.Uncompressed
		arch.Members = p.Members
		arch.Frames = p.Frames
		arch.Units = p.Units
		if p.FileCount == 0 {
			// A zero-change incremental still carries tar's directory census (e.g.
			// "./docs/") as members, but nothing actually changed. Per the documented
			// artifact shape it records no member index — recover reads the base full's
			// index for it — so drop the census members (and the frame table riding the
			// same index) here and write payload+commit only.
			arch.Members = nil
			arch.Frames = nil
			arch.Units = nil
		}
		arch.CreatedAt = a.w.now() // the archive's landing time (per-archive)
	}
	switch {
	case arch.Shape == record.ShapeAtomic && a.copySeals != nil && len(a.copySeals) == len(arch.PartSeals):
		// A copy's atoms are the source's bit-for-bit, so the archive-invariant half
		// of each seal — the raw span the atom covers — carries over unchanged.
		for i := range arch.PartSeals {
			arch.PartSeals[i].RawSize = a.copySeals[i].RawSize
		}
	case arch.Shape == record.ShapeAtomic && len(p.Frames) == len(arch.PartSeals):
		// A fresh dump's atom boundaries arrived as producer Frames (per-atom start
		// offsets); folded into per-part RawSize they ARE the atomic shape's frame
		// table (member offset → covering atom), so no table rides the index.
		for i := range arch.PartSeals {
			end := p.Uncompressed
			if i+1 < len(p.Frames) {
				end = p.Frames[i+1].Raw
			}
			arch.PartSeals[i].RawSize = end - p.Frames[i].Raw
		}
		arch.Frames = nil
	}
	pos, err := a.w.finalize(ctx, &arch, a.parts)
	if err != nil {
		return err
	}
	res := CommitResult{Archive: arch, Pos: pos}
	if err := a.w.rec.Record(res); err != nil {
		return err
	}
	a.committed = &res
	return nil
}

// Close runs the optional close hook (the spool's run release) and returns its error; a leaf writer
// holds no resources between parts (each part self-closes, an aborted part is dropped via its canceled
// ctx, and Commit is terminal), so with no hook it is a no-op. The producer defers Close on every
// path, so a faulted transfer still runs the hook.
func (a *ArchiveWriter) Close() error {
	if a.onClose != nil {
		return a.onClose()
	}
	return nil
}

// finalize durably finalizes an archive (all fields final): it writes the member index (the
// gzip'd Members) then the commit footer (the metadata without members) — the footer last,
// so a crash before it leaves orphan parts a scan ignores. It returns the archive's on-medium
// position (parts/footer/index) for the caller to record, and sets arch.IndexSize — a
// placement-layout fact the footer carries so a volume's fill stays derivable from the
// catalog without reading index payloads. The Writer keeps no run state — the footer makes
// the archive durable and the caller records it from the returned position — so concurrent
// Commits on an unbounded medium need no coordination here. Call it once the caller has
// merged the producer's stats (FileCount/Uncompressed/Members) into the archive.
func (w *Writer) finalize(ctx context.Context, arch *record.Archive, parts []FilePos) (ArchivePos, error) {
	var index FilePos
	if len(arch.Members) > 0 || len(arch.Units) > 0 {
		var buf bytes.Buffer
		if err := record.EncodeIndex(&buf, record.Index{Members: arch.Members, Frames: arch.Frames, Units: arch.Units}); err != nil {
			return ArchivePos{}, err
		}
		arch.IndexSize = int64(buf.Len())
		pos, err := w.writeRecord(ctx, record.KindIndex, *arch, buf.Bytes())
		if err != nil {
			return ArchivePos{}, err
		}
		index = pos
	}
	// The part map — the archive's TOC — makes the footer name every volume the
	// archive spans (parts and member index alike), so a rebuild holding only this
	// tape still learns where the other files live (and a restore can prompt for
	// them by label).
	arch.PartMap = append([]record.FilePos(nil), parts...)
	arch.IndexPos = index
	// The footer omits the member list and frame table (they ride in the index);
	// marshal a copy without them.
	footer := *arch
	footer.Members = nil
	footer.Frames = nil
	footer.Units = nil
	data, err := record.MarshalCommit(footer)
	if err != nil {
		return ArchivePos{}, err
	}
	commit, err := w.writeRecord(ctx, record.KindCommit, *arch, data)
	if err != nil {
		return ArchivePos{}, err
	}
	return ArchivePos{
		Parts:  append([]FilePos(nil), parts...),
		Commit: commit,
		Index:  index,
	}, nil
}

// writeRecord places and writes one small whole record (an index or a commit footer) for an
// archive, returning where it landed. The header identifies the archive it belongs to so a
// scan can correlate it with the archive's parts (which may be on other volumes).
func (w *Writer) writeRecord(ctx context.Context, kind string, a record.Archive, payload []byte) (FilePos, error) {
	vol, label, epoch, err := w.alloc.PlaceFile(kind, int64(len(payload)))
	if err != nil {
		return FilePos{}, fmt.Errorf("place %s record: %w", kind, err)
	}
	h := record.Header{Run: a.Run, Kind: kind, DLE: a.DLE, Level: a.Level, CreatedAt: w.createdAt}
	fw, err := vol.AppendFile(ctx, h)
	if err != nil {
		return FilePos{}, err
	}
	_, werr := fw.Write(payload)
	if cerr := fw.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return FilePos{}, werr
	}
	return FilePos{Label: label, Epoch: epoch, Pos: fw.Pos()}, nil
}

// archiveHeader builds the base record.Header an archive's parts share (NextPart clones
// it per part with an ascending Part index). Every framing field comes straight from the
// archive's descriptive metadata; Split is the allocator's Bounded posture (constant for the
// write), stamped so a multi-part payload's parts are named and read as slices, not standalone files.
func (w *Writer) archiveHeader(a record.Archive) record.Header {
	return record.Header{
		Run:      a.Run,
		Kind:     record.KindArchive,
		DLE:      a.DLE,
		Host:     a.Host,
		Path:     a.Path,
		Archiver: a.Archiver,
		Ext:      a.Ext,
		Compress: a.Compress,
		Encrypt:  a.Encrypt,
		Shape:    a.Shape,
		Level:    a.Level,
		BaseRun:  a.BaseRun,
		// Split (the "not a standalone artifact" marker driving the
		// .pNNN-after-extension name) is a slice concept, so only a resplittable
		// payload carries it; a standalone part (an atom — a whole valid file,
		// never a slice) puts its part index BEFORE the extension instead.
		Split:     w.alloc.Bounded() && a.Shape.Resplittable(),
		CreatedAt: w.createdAt,
	}
}
