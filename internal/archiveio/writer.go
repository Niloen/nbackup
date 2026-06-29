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
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
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

// SlotSpec is the descriptive identity of a slot to author: what is known before
// any archive is written, independent of the archives and bytes the Writer
// assembles while streaming. NewWriter starts the slot from it and Finish returns
// the finished record.Slot — so the caller describes the slot and archiveio produces the
// artifact, never the reverse. The fields mirror record.NewSlot's parameters.
type SlotSpec struct {
	ID        string    // the slot's identity (see record.IDFromParts)
	Date      string    // run date, YYYY-MM-DD
	Sequence  int       // 1 for the day's first run, 2+ for later runs
	Generator string    // tool authoring the slot (e.g. "nbdump")
	CreatedAt time.Time // when authoring began; a copy preserves the source slot's
}

// Writer authors a single slot onto a medium via a VolumeSink. Callers stream each archive's
// payload with WriteArchive and finalize it with Commit (which writes the archive's member
// index and its commit footer — the per-archive marker). There is no slot-level seal: a slot
// is the grouping its archives carry in their headers, and a crashed run's committed archives
// survive (uncommitted parts are orphans a scan ignores). WriteArchive is safe for concurrent
// use only on an unbounded sink (disk); a bounded, spanning-capable sink rolls one shared
// volume and must be driven serially (the engine clamps archivers).
type Writer struct {
	sink VolumeSink
	lim  *ratelimit.Limiter // optional bandwidth cap on the bytes landing on the medium (nil = uncapped)

	mu      sync.Mutex // guards the records below
	slot    *record.Slot
	written []archiveRecord // one per committed archive, in Commit order
}

// archiveRecord remembers where an archive's parts, member index, and commit footer landed,
// so the catalog can index it (see Positions).
type archiveRecord struct {
	dle    string
	level  int
	parts  []record.FilePos
	commit record.FilePos
	index  record.FilePos
}

// NewWriter begins authoring a new slot, described by spec, onto sink. The Writer builds
// and owns the record.Slot from spec (the caller never hands one in); Seal returns it
// sealed. lim, when non-nil, caps the rate of bytes written to the medium (network
// politeness); a nil lim is uncapped. The same lim is shared across concurrent
// WriteArchive calls on an unbounded sink, so several workers to one medium share its
// budget.
func NewWriter(sink VolumeSink, spec SlotSpec, lim *ratelimit.Limiter) *Writer {
	slot := record.NewSlot(spec.ID, spec.Date, spec.Sequence, spec.Generator, spec.CreatedAt)
	return &Writer{sink: sink, lim: lim, slot: slot}
}

// NewArchive begins writing the archive described by meta onto the slot, pulled part-by-part by
// NextPart (the caller copies up to each part's cap into the returned writer, closes it, and asks for
// the next until the payload is exhausted). Rolling to a fresh volume happens inside NextPart, so a
// spanning medium's roll lands wherever the sink routes it. The payload is metered (sha256 + size) on
// the write path — so the metering runs on the caller's goroutine — and Commit finalizes the footer
// with the producer's totals. The descriptive fields of meta are taken as-is; tap, if non-nil, gets
// the running landed byte count.
func (w *Writer) NewArchive(meta record.Archive, tap func(landed int64)) *ArchiveWriter {
	return &ArchiveWriter{w: w, base: w.archiveHeader(meta), meta: meta, tap: tap, h: sha256.New()}
}

// ArchiveWriter is one archive's part-by-part writer (see NewArchive).
type ArchiveWriter struct {
	w     *Writer
	base  record.Header
	meta  record.Archive
	tap   func(int64)
	h     hash.Hash
	n     int64
	parts []record.FilePos
	part  int
}

// NextPart rolls to the next volume and returns a writer for the next part plus its byte cap
// (max < 0 = unbounded). The caller copies up to max bytes into it and Closes it; the part's position
// is recorded on Close. Cancel ctx before Close to abort the part (no committed file).
func (a *ArchiveWriter) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
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

// Commit finalizes the archive: it merges the producer's raw-stream totals into the metered archive
// (sha + compressed size are this writer's), writes the footer + member index, and returns the
// committed archive + its on-medium position. It records no placement — the caller does.
func (a *ArchiveWriter) Commit(ctx context.Context, fileCount int, uncompressed int64, members []string) (record.Archive, record.ArchivePos, error) {
	arch := a.meta
	arch.Compressed = a.n
	arch.SHA256 = hex.EncodeToString(a.h.Sum(nil))
	arch.Parts = len(a.parts)
	arch.FileCount = fileCount
	arch.Uncompressed = uncompressed
	arch.Members = members
	pos, err := a.w.Commit(ctx, arch, a.parts)
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, err
	}
	return arch, pos, nil
}

// Commit durably finalizes an archive (all fields final): it writes the member index (the
// gzip'd Members) then the commit footer (the metadata without members) — the footer last,
// so a crash before it leaves orphan parts a scan ignores. It then records the archive in the
// in-memory slot and returns its on-medium position (parts/footer/index) for the caller to
// catalog. Call it once the caller has merged the producer's stats
// (FileCount/Uncompressed/Members) into the archive WriteArchive returned.
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
	w.mu.Lock()
	w.slot.AddArchive(arch)
	w.written = append(w.written, archiveRecord{dle: arch.DLE, level: arch.Level, parts: parts, commit: commit, index: index})
	w.mu.Unlock()
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
	h := record.Header{Slot: w.slot.ID, Kind: kind, DLE: a.DLE, Level: a.Level, CreatedAt: w.slot.CreatedAt}
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

// meteredReader counts and hashes the bytes read through it — the streaming sha256 and
// size of the payload that lands on the medium — and reports the running total. It is
// robust to drainParts re-prepending a peeked byte: the re-prepended byte comes from a
// separate reader, so it is never hashed twice.
type meteredReader struct {
	r   io.Reader
	h   hash.Hash
	n   int64
	tap func(int64)
}

func (m *meteredReader) Read(p []byte) (int, error) {
	k, err := m.r.Read(p)
	if k > 0 {
		m.h.Write(p[:k])
		m.n += int64(k)
		if m.tap != nil {
			m.tap(m.n)
		}
	}
	return k, err
}

// archiveHeader builds the base record.Header an archive's parts share (drainParts
// clones it per part with an ascending Part index). Every framing field comes straight
// from the archive's descriptive metadata.
func (w *Writer) archiveHeader(a record.Archive) record.Header {
	return record.Header{
		Slot:      w.slot.ID,
		Kind:      record.KindArchive,
		DLE:       a.DLE,
		Host:      a.Host,
		Path:      a.Path,
		Archiver:  a.Archiver,
		Compress:  a.Compress,
		Encrypt:   a.Encrypt,
		Level:     a.Level,
		BaseSlot:  a.BaseSlot,
		CreatedAt: w.slot.CreatedAt,
	}
}

// drainParts reads the payload from src and writes it as one or more part files
// (headers cloned from base, with an ascending Part index), rolling the sink to the
// next volume whenever a part fills. It returns the ordered part positions. On error
// it returns it for the caller to handle (closing the producer); the partial files
// left behind are unsealed and ignored by a scan.
func (w *Writer) drainParts(ctx context.Context, base record.Header, src io.Reader) ([]record.FilePos, error) {
	var (
		parts []record.FilePos
		part  int
	)
	for {
		vol, max, volName, epoch, err := w.sink.NextPart()
		if err != nil {
			return nil, err
		}

		h := base
		h.Part = part

		fw, err := vol.AppendFile(ctx, h)
		if err != nil {
			return nil, err
		}
		r := src
		if max >= 0 {
			r = io.LimitReader(src, max)
		}
		// Pace the write to the medium's cap. Throttling here back-pressures the
		// producer (tar → compress → encrypt) through the pipe, so the one-pass
		// pipeline slows without buffering. A nil lim leaves the writer untouched.
		wrote, cerr := io.Copy(w.lim.Writer(fw), r)
		if clErr := fw.Close(); cerr == nil {
			cerr = clErr
		}
		if cerr != nil {
			return nil, cerr
		}
		parts = append(parts, record.FilePos{Label: volName, Epoch: epoch, Pos: fw.Pos()})
		part++

		// Producer exhausted within this part (or the sink is unbounded): done.
		if max < 0 || wrote < max {
			return parts, nil
		}
		// Filled the part exactly at the cap — peek one byte to tell "exactly done"
		// from "more to come". On EOF the archive is complete; otherwise continue with
		// the peeked byte re-prepended (the next NextPart rolls if the volume is full,
		// or stays put when the cap was a part_size split within a volume).
		var b [1]byte
		n, perr := io.ReadFull(src, b[:])
		if n == 0 {
			if perr == io.EOF || perr == io.ErrUnexpectedEOF {
				return parts, nil
			}
			return nil, perr
		}
		src = io.MultiReader(bytes.NewReader(b[:1]), src)
	}
}

// CopyArchive writes an already-compressed archive payload (src is the source copy's
// parts concatenated) to the slot, re-split into parts sized to the target's volumes.
// It does NOT compress or re-checksum the stream — the same bytes are written, so the
// recorded checksum is unchanged — and only the part layout (and Parts count) is new.
// The header carries the archive's original scheme, so restore reverses the right one.
//
// tap, if non-nil, is called with the running count of bytes copied as the payload
// drains — the live drain signal for `nb status`, riding the metering this does anyway
// (size + checksum). It runs on the draining goroutine, so it must be cheap.
func (w *Writer) CopyArchive(ctx context.Context, meta record.Archive, src io.Reader, tap func(copied int64)) (record.Archive, record.ArchivePos, error) {
	mr := &meteredReader{r: src, h: sha256.New(), tap: tap}
	parts, err := w.drainParts(ctx, w.archiveHeader(meta), mr)
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, err
	}
	if got := hex.EncodeToString(mr.h.Sum(nil)); got != meta.SHA256 {
		return record.Archive{}, record.ArchivePos{}, fmt.Errorf("copy of %s L%d checksum mismatch (source corrupt?)", meta.DLE, meta.Level)
	}
	arch := meta
	arch.Parts = len(parts)
	pos, err := w.Commit(ctx, arch, parts)
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, err
	}
	return arch, pos, nil
}

// SlotID returns the id of the slot being authored.
func (w *Writer) SlotID() string { return w.slot.ID }

// SlotMeta returns the slot's identity without its archives — the grouping a caller records
// each archive under (the catalog creates the entry from it). It is the writer's seam for the
// inline-recording callers (a copy/sync's CopyArchive, the orchestrator's drain), which hold a
// position but not the slot.
func (w *Writer) SlotMeta() *record.Slot {
	w.mu.Lock()
	defer w.mu.Unlock()
	ident := *w.slot
	ident.Archives, ident.TotalBytes = nil, 0
	return &ident
}

// ArchiveCount reports how many archives have been recorded so far.
func (w *Writer) ArchiveCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.slot.Archives)
}

// Positions returns the on-medium positions of every committed archive — its parts, its
// commit footer, and its member index — for the catalog to index. Call after all
// WriteArchive/Commit calls have completed.
func (w *Writer) Positions() []record.ArchivePos {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]record.ArchivePos, len(w.written))
	for i, a := range w.written {
		out[i] = record.ArchivePos{
			DLE:    a.dle,
			Level:  a.level,
			Parts:  append([]record.FilePos(nil), a.parts...),
			Commit: a.commit,
			Index:  a.index,
		}
	}
	return out
}

// Finish closes the slot in memory and returns it — the run's grouping of committed
// archives. There is no slot-level record on the medium: each archive is already durable via
// its own commit footer (written inline as it finished), so Finish only stamps the in-memory
// slot's completion (for the catalog and the run summary) and never touches the volume.
//
// The write path reads nothing back: each archive was hashed inline as it streamed out
// (the streaming-meter sha256 recorded in the catalog), so integrity rests on that
// checksum, not a re-read. Verifying the bytes actually landed is the job of the
// explicit, operator-invoked `nb verify`.
func (w *Writer) Finish(now time.Time) (*record.Slot, error) {
	if err := w.slot.Seal(now); err != nil {
		return nil, err
	}
	return w.slot, nil
}
