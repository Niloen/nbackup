// Package slotio authors and reads slots on a media.Volume. It owns how a slot
// maps onto a volume's files — one or more part files per archive plus a final seal
// record carrying the slot's metadata — so the engine supplies only backup streams
// and descriptive metadata, never positions or filenames. Compression is delegated
// to package filter (an external child process); slotio meters (checksums + counts)
// the compressed bytes on their way to the volume.
//
// An archive may be split into several parts across volumes (tape spanning). The
// writer streams the compressed payload through a pipe and drains it into parts
// sized to fit each volume's known remaining capacity, rolling to the next volume
// (via a VolumeSink) between parts. The split is PROACTIVE — each part is bounded
// before it is written — so a volume is never overfilled in the normal path; the
// media.ErrVolumeFull backstop only fires when an estimate came up short, and then
// the write fails rather than recovering.
package slotio

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Niloen/nbackup/internal/crypt"
	"github.com/Niloen/nbackup/internal/filter"
	"github.com/Niloen/nbackup/internal/format"
	"github.com/Niloen/nbackup/internal/hostexec"
	"github.com/Niloen/nbackup/internal/media"
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
	// PlaceSeal returns the volume to write the slot's seal record (one whole file of
	// the given payload size) to, rolling first if it will not fit the loaded volume.
	PlaceSeal(size int64) (vol media.Volume, volume string, epoch int, err error)
}

// Produced reports the raw-stream statistics of one archive, returned by the source's
// Finish hook after the pipeline drains.
type Produced struct {
	Uncompressed int64
	FileCount    int
	Members      []string
}

// Source is the producing side of one archive: either a program stage (the archiver's
// `tar --create`, run on Exec) or, when Stage is empty, an in-process byte stream (Stdin,
// used by tests and raw injectors). Finish gathers the raw-stream stats after the
// pipeline has drained; Cleanup releases any scratch the source created. The Writer
// appends the compress and encrypt stages and groups them with the source by host, so a
// fully client-side dump keeps plaintext on the client.
type Source struct {
	Stage   hostexec.Cmd
	Exec    hostexec.Executor
	Stdin   io.Reader
	Finish  func() (Produced, error)
	Cleanup func()
}

// SlotSpec is the descriptive identity of a slot to author: what is known before
// any archive is written, independent of the archives and bytes the Writer
// assembles while streaming. NewWriter starts the slot from it and Seal returns
// the finished format.Slot — so, like ArchiveSpec for an archive, the caller
// describes the slot and slotio produces the artifact, never the reverse. The
// fields mirror format.NewSlot's parameters.
type SlotSpec struct {
	ID        string    // the slot's identity (see format.IDFromParts)
	Date      string    // run date, YYYY-MM-DD
	Sequence  int       // 1 for the day's first run, 2+ for later runs
	Generator string    // tool authoring the slot (e.g. "nbdump")
	CreatedAt time.Time // when authoring began; a copy preserves the source slot's
}

// ArchiveSpec is the descriptive metadata of an archive, known independently of
// the bytes the Writer measures while streaming it. Encrypt/EncOpts select the
// per-archive encryption (resolved from the DLE's dumptype); the scheme name is
// recorded, the key reference in EncOpts is used only while writing.
type ArchiveSpec struct {
	DLE      string
	Host     string
	Path     string
	Archiver string
	Level    int
	BaseSlot string
	Encrypt  string        // encryption scheme name ("" or "none" = plaintext)
	EncOpts  crypt.Options // key reference + nice for the encryptor child

	// CompressExec/EncryptExec choose the host each transform runs on (nil = Local).
	// Setting them to the source's client executor keeps plaintext on the client
	// (compress/encrypt fuse with tar there); the meter that follows is always
	// server-side, so the seal covers the bytes that land.
	CompressExec hostexec.Executor
	EncryptExec  hostexec.Executor
}

// PartPosition is one part's volume and file position, for the catalog to index.
type PartPosition struct {
	Volume string
	Epoch  int
	Pos    int
}

// ArchivePositions is one archive's identity and the ordered positions of its parts.
type ArchivePositions struct {
	DLE   string
	Level int
	Parts []PartPosition
}

// Writer authors a single slot onto a medium via a VolumeSink. Callers create
// archives with WriteArchive and finalize with Seal. WriteArchive is safe for
// concurrent use only on an unbounded sink (disk); a bounded, spanning-capable sink
// rolls one shared volume and must be driven serially (the engine clamps archivers).
type Writer struct {
	sink  VolumeSink
	codec string
	fopts filter.Options
	lim   *xfer.Limiter // optional bandwidth cap on the bytes landing on the medium (nil = uncapped)

	mu       sync.Mutex // guards the records below
	slot     *format.Slot
	written  []archiveRecord // one per archive, in WriteArchive order
	sealPart PartPosition    // where the seal landed (set by Seal)
}

// archiveRecord remembers an archive's parts so the catalog can index where each
// archive's bytes landed (see Positions).
type archiveRecord struct {
	dle   string
	level int
	parts []PartPosition
}

// NewWriter begins authoring a new slot, described by spec, onto sink, compressing
// archives with the named codec. The Writer builds and owns the format.Slot from
// spec (the caller never hands one in); Seal returns it sealed. lim, when non-nil,
// caps the rate of bytes written to the medium (network politeness); a nil lim is
// uncapped. The same lim is shared across concurrent WriteArchive calls on an
// unbounded sink, so several workers to one medium share its budget (Amanda's
// netusage).
func NewWriter(sink VolumeSink, spec SlotSpec, codec string, fopts filter.Options, lim *xfer.Limiter) (*Writer, error) {
	if _, err := filter.Ext(codec); err != nil { // validate the codec name early
		return nil, err
	}
	slot := format.NewSlot(spec.ID, spec.Date, spec.Sequence, spec.Generator, spec.CreatedAt)
	return &Writer{sink: sink, codec: codec, fopts: fopts, lim: lim, slot: slot}, nil
}

// WriteArchive appends one archive to the slot, split into as many part files as the
// loaded volumes' capacities require. It pipes the produced raw stream through the
// codec's compressor child, metering (checksum + size) the whole compressed stream,
// and drains that stream into parts: each part is bounded by the current volume's
// Room, and when a part fills the sink rolls to the next volume. It records the
// archive (with its part count) in the slot and returns the recorded metadata.
//
// progress, if non-nil, is called as the stream flows with the running
// (uncompressed, compressed) byte counts — the live signal for `nb status`. It runs
// on the producing goroutine, so it must be cheap.
func (w *Writer) WriteArchive(spec ArchiveSpec, progress func(uncompressed, compressed int64), src Source) (format.Archive, error) {
	// One pipeline: source -> compress -> encrypt, each a program stage carrying its
	// host; adjacent same-host stages fuse so intermediate bytes never leave that host.
	// The grouped pipeline's final reader is metered (the checksum/size cover the
	// ciphertext that lands) and drained into volume parts.
	pr, pw := io.Pipe()
	meter := xfer.NewMeter(pw)

	stages, stdin, err := w.pipelineStages(spec, src, meter, progress)
	if err != nil {
		return format.Archive{}, err
	}

	base := w.archiveHeader(spec.DLE, spec.Host, spec.Path, spec.Archiver, spec.Level, spec.BaseSlot, w.codec, spec.Encrypt)

	var produced Produced
	var produceErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		final, wait, runErr := hostexec.RunGrouped(stdin, stages...)
		if runErr != nil {
			produceErr = runErr
			pw.CloseWithError(runErr)
			return
		}
		_, copyErr := io.Copy(meter, final)
		final.Close()     // drains; SIGPIPEs the producers if the consumer stopped early
		waitErr := wait() // reap every group, first failure wins
		// A pipeline failure is the producer's own error; copyErr only when the consumer
		// (drainParts) closed the pipe early, which the drain-error path already reports.
		e := waitErr
		if e == nil {
			e = copyErr
		}
		if e == nil && src.Finish != nil {
			produced, e = src.Finish() // reads scratch (the member index) — before Cleanup
		}
		if src.Cleanup != nil {
			src.Cleanup() // remove scratch after Finish, even on error
		}
		if e != nil {
			produceErr = e
			pw.CloseWithError(e)
			return
		}
		pw.Close() // EOF to the consumer
	}()

	parts, drainErr := w.drainParts(base, pr)
	if drainErr != nil {
		pr.CloseWithError(drainErr) // unblock the producer goroutine
	}
	<-done // producer finished: meter is complete, produced/produceErr are visible
	if drainErr != nil {
		return format.Archive{}, drainErr
	}
	if produceErr != nil {
		return format.Archive{}, produceErr
	}

	arch := format.Archive{
		DLE:          spec.DLE,
		Host:         spec.Host,
		Path:         spec.Path,
		Archiver:     spec.Archiver,
		Codec:        w.codec,
		Encrypt:      spec.Encrypt,
		Level:        spec.Level,
		Compressed:   meter.Bytes(),
		Uncompressed: produced.Uncompressed,
		FileCount:    produced.FileCount,
		SHA256:       meter.SHA256(),
		Parts:        len(parts),
		BaseSlot:     spec.BaseSlot,
		Members:      produced.Members,
	}

	w.mu.Lock()
	w.slot.AddArchive(arch)
	w.written = append(w.written, archiveRecord{dle: spec.DLE, level: spec.Level, parts: parts})
	w.mu.Unlock()
	return arch, nil
}

// pipelineStages assembles the producing stages (source, then the codec compressor, then
// the encryptor) with their executors, plus the in-process stdin when the source is not a
// program. The source stage is tapped for the live uncompressed byte count (honored only
// when it runs locally; a remote source falls back to compressed-against-estimate).
func (w *Writer) pipelineStages(spec ArchiveSpec, src Source, meter *xfer.Meter, progress func(uncompressed, compressed int64)) ([]hostexec.Stage, io.Reader, error) {
	tap := func(n int64) {}
	if progress != nil {
		tap = func(n int64) { progress(n, meter.Bytes()) }
	}

	var stages []hostexec.Stage
	var stdin io.Reader
	if src.Stage.Name != "" {
		s := src.Stage
		if progress != nil {
			s.Tap = tap
		}
		stages = append(stages, hostexec.Stage{Cmd: s, Exec: execOr(src.Exec)})
	} else {
		stdin = src.Stdin
		if progress != nil && stdin != nil {
			stdin = &countingReader{r: stdin, f: tap}
		}
	}

	if ccmd, ok, err := filter.CompressCmd(w.codec, w.fopts); err != nil {
		return nil, nil, err
	} else if ok {
		stages = append(stages, hostexec.Stage{Cmd: ccmd, Exec: execOr(spec.CompressExec)})
	}
	if ecmd, ok, err := crypt.EncryptCmd(spec.Encrypt, spec.EncOpts); err != nil {
		return nil, nil, err
	} else if ok {
		stages = append(stages, hostexec.Stage{Cmd: ecmd, Exec: execOr(spec.EncryptExec)})
	}
	return stages, stdin, nil
}

// execOr returns ex, or the local executor when ex is nil (the default, all-local case).
func execOr(ex hostexec.Executor) hostexec.Executor {
	if ex == nil {
		return hostexec.Local()
	}
	return ex
}

// countingReader counts bytes read and reports the running total — the uncompressed
// progress signal when the source is an in-process stream rather than a program stage.
type countingReader struct {
	r io.Reader
	n int64
	f func(int64)
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 {
		c.n += int64(n)
		c.f(c.n)
	}
	return n, err
}

// archiveHeader builds the base format.Header an archive's parts share (drainParts
// clones it per part with an ascending Part index). The codec is passed explicitly
// because a fresh dump stamps the writer's codec while a copy preserves the source
// archive's recorded codec — every other descriptive field comes straight through.
func (w *Writer) archiveHeader(dle, host, path, archiver string, level int, baseSlot, codec, encrypt string) format.Header {
	return format.Header{
		Slot:      w.slot.ID,
		Kind:      format.KindArchive,
		DLE:       dle,
		Host:      host,
		Path:      path,
		Archiver:  archiver,
		Codec:     codec,
		Encrypt:   encrypt,
		Level:     level,
		BaseSlot:  baseSlot,
		CreatedAt: w.slot.CreatedAt,
	}
}

// drainParts reads the payload from src and writes it as one or more part files
// (headers cloned from base, with an ascending Part index), rolling the sink to the
// next volume whenever a part fills. It returns the ordered part positions. On error
// it returns it for the caller to handle (closing the producer); the partial files
// left behind are unsealed and ignored by a scan.
func (w *Writer) drainParts(base format.Header, src io.Reader) ([]PartPosition, error) {
	var (
		parts []PartPosition
		part  int
	)
	for {
		vol, max, volName, epoch, err := w.sink.NextPart()
		if err != nil {
			return nil, err
		}

		h := base
		h.Part = part

		var wrote int64
		pos, err := vol.AppendFile(h, func(out io.Writer) error {
			r := src
			if max >= 0 {
				r = io.LimitReader(src, max)
			}
			// Pace the write to the medium's cap. Throttling here back-pressures the
			// producer (tar → compress → encrypt) through the pipe, so the one-pass
			// pipeline slows without buffering. A nil lim leaves out untouched.
			n, e := io.Copy(w.lim.Writer(out), r)
			wrote = n
			return e
		})
		if err != nil {
			return nil, err
		}
		parts = append(parts, PartPosition{Volume: volName, Epoch: epoch, Pos: pos})
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
// The header carries the archive's original codec, so restore reverses the right one.
func (w *Writer) CopyArchive(meta format.Archive, src io.Reader) (format.Archive, error) {
	base := w.archiveHeader(meta.DLE, meta.Host, meta.Path, meta.Archiver, meta.Level, meta.BaseSlot, meta.Codec, meta.Encrypt)
	h := sha256.New()
	parts, err := w.drainParts(base, io.TeeReader(src, h))
	if err != nil {
		return format.Archive{}, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != meta.SHA256 {
		return format.Archive{}, fmt.Errorf("copy of %s L%d checksum mismatch (source corrupt?)", meta.DLE, meta.Level)
	}
	arch := meta
	arch.Parts = len(parts)
	w.mu.Lock()
	w.slot.AddArchive(arch)
	w.written = append(w.written, archiveRecord{dle: meta.DLE, level: meta.Level, parts: parts})
	w.mu.Unlock()
	return arch, nil
}

// ArchiveCount reports how many archives have been written so far.
func (w *Writer) ArchiveCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.slot.Archives)
}

// Positions returns the part positions of every archive written, for the catalog to
// index. Call after all WriteArchive calls have completed.
func (w *Writer) Positions() []ArchivePositions {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]ArchivePositions, len(w.written))
	for i, a := range w.written {
		out[i] = ArchivePositions{DLE: a.dle, Level: a.level, Parts: append([]PartPosition(nil), a.parts...)}
	}
	return out
}

// SealPosition returns where the seal record landed (its volume and position).
func (w *Writer) SealPosition() PartPosition {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sealPart
}

// Seal seals the slot and appends the seal record (the slot's metadata) as the final
// file — the marker that makes the slot complete. The sealed slot is returned.
//
// Like Amanda's taper, sealing does not read the medium back: each archive was hashed
// inline as it streamed out (the streaming-meter sha256 recorded in the catalog), so
// the write path's integrity rests on that checksum, not a re-read. Verifying the bytes
// actually landed on the medium is the job of the explicit, operator-invoked `nb verify`
// (the amcheckdump analogue), kept out of the dump path so a single drive never has to
// re-read — or reload swapped-out volumes — just to close a slot.
func (w *Writer) Seal(now time.Time) (*format.Slot, error) {
	if err := w.slot.Seal(now); err != nil {
		return nil, err
	}
	data, err := w.slot.Marshal()
	if err != nil {
		return nil, err
	}
	vol, sealVol, sealEpoch, err := w.sink.PlaceSeal(int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("place the seal record: %w", err)
	}
	seal := format.Header{Slot: w.slot.ID, Kind: format.KindSeal, CreatedAt: now}
	pos, err := vol.AppendFile(seal, func(out io.Writer) error {
		_, e := out.Write(data)
		return e
	})
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.sealPart = PartPosition{Volume: sealVol, Epoch: sealEpoch, Pos: pos}
	w.mu.Unlock()
	return w.slot, nil
}
