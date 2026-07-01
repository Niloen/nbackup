// Package archiveio authors and reads runs on a media.Volume. It owns how a run maps
// onto a volume's files — one or more part files per archive plus a final seal record
// carrying the run's metadata — so the engine supplies only an already-transformed
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
	// Bounded reports whether this sink ever caps a part's size — by a configured part_size
	// or by a finite volume's remaining capacity (the dual of NextPart's "max < 0 means
	// unbounded"). When true an archive may land as several parts (cloud splitting under a
	// part_size cap, or a finite reel spanning volumes mid-archive), so the writer stamps
	// every part's Header.Split to mark it a slice of one whole — named and read as such,
	// not as a standalone file. When false (disk: no cap, infinite room) each archive is a
	// single, standalone part. It is a property of the medium, constant for the write, so the
	// writer asks once. (This is the same predicate as the medium's spanning capability.)
	Bounded() bool
}

// WriteStore is an archive writer's whole write store: it allocates volumes (VolumeSink) and
// records each finished archive's placement. The clerk implements it serially — alloc reaches the
// librarian, Record writes the catalog, both inline on the caller's goroutine — while the spool
// wraps N of them, routing alloc and Record to its single orchestrator so a slow drive never blocks
// the catalog. The ArchiveWriter SDK below is written once over this interface and is oblivious to
// which implementation it holds; that is the whole serial-vs-concurrent seam. (Name provisional.)
type WriteStore interface {
	VolumeSink
	// Record commits one finished archive's placement onto this store's medium — reported once
	// Commit has assembled the record (Amanda's taper "DONE"). Inline for the serial impl; routed to
	// the orchestrator for the concurrent one. It is the whole worker→coordinator crossing: one value.
	Record(result CommitResult) error
}

// RunSpec is the identity of a run to author: the run id every archive in the run is
// tagged with, plus when authoring began (stamped into each file's header). A run is just
// that shared tag — the archives carry it and the catalog groups them back — so there is no
// run record for the Author to produce.
type RunSpec struct {
	ID        string    // the run's identity (see record.IDFromParts)
	CreatedAt time.Time // when authoring began; a copy preserves the source run's
}

// Author authors a single run onto a medium via a WriteStore. Callers stream each archive's
// payload with NewArchive and finalize it with Commit (which writes the archive's member
// index and its commit footer — the per-archive marker — then reports it via WriteStore.Record).
// There is no run-level seal: a run is the grouping its archives carry in their headers, and a
// crashed run's committed archives survive (uncommitted parts are orphans a scan ignores). The
// Author accumulates no run state — each archive is durable and recorded the moment it commits —
// so NewArchive is safe for concurrent use on an unbounded store (disk); a bounded,
// spanning-capable store rolls one shared volume and must be driven serially.
type Author struct {
	store     WriteStore
	lim       *ratelimit.Limiter // optional bandwidth cap on the bytes landing on the medium (nil = uncapped)
	now       func() time.Time   // clock for per-archive commit timestamps (nil → time.Now)
	runID     string             // the run tag every archive in this run carries; read-only after construction
	createdAt time.Time          // when authoring began, stamped into each file's header
}

// NewAuthor begins authoring a new run, described by spec, onto store. The Author holds only the
// run tag and creation time from spec. lim, when non-nil, caps the rate of bytes written to the
// medium (network politeness); a nil lim is uncapped. The same lim is shared across concurrent
// NewArchive writers on an unbounded store, so several workers to one medium share its budget.
func NewAuthor(store WriteStore, spec RunSpec, lim *ratelimit.Limiter, now func() time.Time) *Author {
	if now == nil {
		now = time.Now
	}
	return &Author{store: store, lim: lim, now: now, runID: spec.ID, createdAt: spec.CreatedAt}
}

// NewArchive begins writing the archive described by spec onto the run, pulled part-by-part by
// NextPart (the caller copies up to each part's cap into the returned writer, closes it, and asks for
// the next until the payload is exhausted). Rolling to a fresh volume happens inside NextPart, so a
// spanning medium's roll lands wherever the store routes it. The payload is metered (sha256 + size)
// on the write path — so the metering runs on the caller's goroutine — and Commit finalizes the
// footer with the producer's totals and reports the placement to the store. To observe the running
// landed byte count for live progress, attach a tap with MeterArchive.
func (w *Author) NewArchive(spec ArchiveSpec) *ArchiveWriter {
	meta := record.Archive{
		Run:      w.runID,
		DLE:      spec.DLE,
		Host:     spec.Host,
		Path:     spec.Path,
		Archiver: spec.Archiver,
		Compress: spec.Compress,
		Encrypt:  spec.Encrypt,
		Level:    spec.Level,
		BaseRun:  spec.BaseRun,
	}
	return &ArchiveWriter{w: w, base: w.archiveHeader(meta), meta: meta, h: sha256.New()}
}

// NewCopy begins re-authoring an already-committed archive onto the run — the same path as
// NewArchive (pulled part-by-part by NextPart), but for a copy: the bytes are carried raw, so on
// Commit the writer verifies the metered checksum against arch.SHA256 and preserves arch's stats,
// members, and CreatedAt (a copy keeps the source's identity and age); only the part layout, sized to
// this medium's volumes, is new. The spool's holding->backing drain, `nb copy`, and crash-recovery
// Flush all share this one copy path. Attach a tap with MeterArchive for live progress.
func (w *Author) NewCopy(arch record.Archive) *ArchiveWriter {
	arch.Run = w.runID // re-authored under this writer's run (the same id, by construction)
	return &ArchiveWriter{w: w, base: w.archiveHeader(arch), meta: arch, expectSHA: arch.SHA256, h: sha256.New()}
}

// MeterArchive attaches a progress tap to aw — the running count of landed bytes, reported after
// each write on the writing goroutine — and returns aw for chaining. A nil tap is a no-op. The count
// is the same one the writer already meters for the archive's size, so this only exposes it; the
// writer, store, spool, and clerk stay otherwise observability-free.
func MeterArchive(aw *ArchiveWriter, tap func(landed int64)) *ArchiveWriter {
	aw.tap = tap
	return aw
}

// ArchiveWriter is one archive's part-by-part write SDK (see NewArchive / NewCopy). A transfer drives
// it as an xfer.Sink: NextPart/Commit do all the byte I/O — headers, payload, footer, member index,
// SHA and size — on the caller's goroutine, and Commit assembles the record and reports it to the
// Author's WriteStore (WriteStore.Record). It is a concrete client of WriteStore; the serial-vs-concurrent
// choice lives entirely in which WriteStore the Author holds, never here.
type ArchiveWriter struct {
	w         *Author
	base      record.Header
	meta      record.Archive
	expectSHA string // a copy's known digest to verify against ("" for a fresh dump)
	h         hash.Hash
	n         int64
	parts     []record.FilePos
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
// WriteStore.NextPart alloc may cross to the orchestrator; the AppendFile header and the payload writes
// stay on this goroutine.
func (a *ArchiveWriter) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	vol, max, volName, epoch, err := a.w.store.NextPart()
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
	a       *ArchiveWriter
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
// count, writes the footer + member index, assembles the CommitResult, and reports it to the
// Author's WriteStore — inline to the catalog for the serial clerk, routed to the orchestrator for the
// concurrent spool. That report is the whole worker→coordinator crossing: one value.
//
// A fresh dump derives the rest from this writer and the producer's raw-stream totals (sha, file
// count, uncompressed size, members) and stamps CreatedAt now. A copy (NewCopy) instead verifies the
// metered checksum against the source's known digest and preserves the source's stats, members, and
// CreatedAt — the producer totals are ignored, the bytes being carried raw.
func (a *ArchiveWriter) Commit(ctx context.Context, p xfer.SourceStats) error {
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
	res := CommitResult{Archive: arch, Pos: pos}
	if err := a.w.store.Record(res); err != nil {
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

// Commit durably finalizes an archive (all fields final): it writes the member index (the
// gzip'd Members) then the commit footer (the metadata without members) — the footer last,
// so a crash before it leaves orphan parts a scan ignores. It returns the archive's on-medium
// position (parts/footer/index) for the caller to record. The Author keeps no run state — the
// footer makes the archive durable and the caller records it from the returned position — so
// concurrent Commits on an unbounded store need no coordination here. Call it once the caller has
// merged the producer's stats (FileCount/Uncompressed/Members) into the archive.
func (w *Author) Commit(ctx context.Context, arch record.Archive, parts []record.FilePos) (record.ArchivePos, error) {
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
func (w *Author) writeRecord(ctx context.Context, kind string, a record.Archive, payload []byte) (record.FilePos, error) {
	vol, volName, epoch, err := w.store.PlaceRecord(int64(len(payload)))
	if err != nil {
		return record.FilePos{}, fmt.Errorf("place %s record: %w", kind, err)
	}
	h := record.Header{Run: w.runID, Kind: kind, DLE: a.DLE, Level: a.Level, CreatedAt: w.createdAt}
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
// archive's descriptive metadata; Split is the store's Bounded posture (constant for the
// write), stamped so a multi-part payload's parts are named and read as slices, not standalone files.
func (w *Author) archiveHeader(a record.Archive) record.Header {
	return record.Header{
		Run:       w.runID,
		Kind:      record.KindArchive,
		DLE:       a.DLE,
		Host:      a.Host,
		Path:      a.Path,
		Archiver:  a.Archiver,
		Compress:  a.Compress,
		Encrypt:   a.Encrypt,
		Level:     a.Level,
		BaseRun:   a.BaseRun,
		Split:     w.store.Bounded(),
		CreatedAt: w.createdAt,
	}
}

// RunID returns the id of the run being authored.
func (w *Author) RunID() string { return w.runID }
