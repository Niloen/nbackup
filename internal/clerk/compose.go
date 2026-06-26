package clerk

import (
	"io"
	"sync/atomic"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
	"github.com/Niloen/nbackup/internal/xfer"
)

// EncodePlacement is the write-side transform recipe for one dumptype: which compress/encrypt
// schemes to apply, their invocation options, and where each runs. A transform `at: client`
// rides in the SOURCE (fused with tar on the client, so plaintext never leaves it); otherwise
// it is a local Filter (server-side). The clerk records the schemes onto the archive and
// builds the filters from the options — so the engine never assembles the storage record.
type EncodePlacement struct {
	Codec          string // compression scheme name ("none", "gzip", "zstd", ...)
	CompressOpts   compress.Options
	CompressClient bool
	EncryptScheme  string // encryption scheme name ("" = plaintext)
	EncryptOpts    crypt.Options
	EncryptClient  bool
}

// --- the two archiveio-coupled endpoints ---

// mediumSink is the xfer.Sink that lands an archive's bytes onto a slot's volumes via the
// open archiveio.Writer: it meters (sha256 + size) and splits the stream into parts, keeping
// the measured archive + part positions for the caller to record once the producer's stats
// are merged in. It is the write peer of the archive Source.
type mediumSink struct {
	w    *archiveio.Writer
	meta record.Archive

	arch  record.Archive // measured (Compressed/SHA256/Parts), filled by Drain
	parts []record.FilePos
}

func (m *mediumSink) Drain(in io.Reader, progress func(int64)) (xfer.SinkResult, error) {
	arch, parts, err := m.w.WriteArchive(m.meta, in, progress)
	if err != nil {
		return xfer.SinkResult{}, err
	}
	m.arch, m.parts = arch, parts
	return xfer.SinkResult{Compressed: arch.Compressed, SHA256: arch.SHA256}, nil
}

// copySink re-splits a source copy's already-compressed bytes onto the target's volumes
// without recompressing, verifying the stream against the seal's checksum
// (archiveio.Writer.CopyArchive). It is the raw-passthrough peer of mediumSink.
type copySink struct {
	w    *archiveio.Writer
	meta record.Archive
}

func (s *copySink) Drain(in io.Reader, _ func(int64)) (xfer.SinkResult, error) {
	_, err := s.w.CopyArchive(s.meta, in)
	return xfer.SinkResult{}, err
}

// --- the write-side slot session ---

// Session authors one slot onto medium: the engine opens it over an archiveio.Writer, backs up
// (or copies) each archive into it, and Finishes — which records the run in the map. It is the
// write peer of the read verbs — the single place a record.Archive, its parts, and its
// placement are assembled, so the engine describes a backup (intent) and the session produces
// (and records) the artifact, never the reverse.
type Session struct {
	clerk  *Clerk
	w      *archiveio.Writer
	medium string
}

// OpenSlot starts a write session over an open slot writer landing on medium.
func (c *Clerk) OpenSlot(w *archiveio.Writer, medium string) *Session {
	return &Session{clerk: c, w: w, medium: medium}
}

// Finish closes the slot and records the run in the map: it seals the in-memory slot and
// records its placement (the archives' on-medium positions) under the session's medium. The
// clerk owns this map write, so every caller that authors a slot gets it recorded the same
// way. It returns the sealed slot.
func (s *Session) Finish(now time.Time) (*record.Slot, error) {
	sealed, err := s.w.Finish(now)
	if err != nil {
		return nil, err
	}
	placement := catalog.Placement{Medium: s.medium, Archives: s.w.Positions()}
	if err := s.clerk.cat.Record(sealed, placement); err != nil {
		return nil, err
	}
	return sealed, nil
}

// BackupSpec describes one archive to back up: the resolved archiver and its request, plus
// the bits of identity not in the request (the DLE's host, the base slot for an incremental,
// and the dumptype that selects the transform placement). The schemes and options come from
// the clerk's EncodePlacement, so this is pure intent — no storage record.
type BackupSpec struct {
	Archiver archiver.Archiver
	Request  archiver.BackupRequest
	Host     string
	BaseSlot string
	DumpType string
}

// Summary is what the engine needs back to track and log a finished archive — never its
// parts or storage record.
type Summary struct {
	FileCount    int
	Uncompressed int64
	Compressed   int64
	Codec        string // the compression scheme applied ("none" => stored, not compressed)
}

// Backup composes a dump as one transfer — the archiver's tar source (on its host) → the
// encode filters placed per the dumptype (client-side ones fused into the source, server-side
// ones as local Filters) → the slot's medium sink — then records the measured archive (the
// producer's and the sink's stats merged) into the slot. prog, if non-nil, receives running
// (uncompressed, compressed) counts. It returns a Summary for the engine to track and log.
func (s *Session) Backup(spec BackupSpec, prog func(uncompressed, compressed int64)) (Summary, error) {
	pl := s.clerk.deps.EncodePlacement(spec.DumpType)
	compF, err := compress.Filter(pl.Codec, pl.CompressOpts)
	if err != nil {
		return Summary{}, err
	}
	encF, err := crypt.Filter(pl.EncryptScheme, pl.EncryptOpts)
	if err != nil {
		return Summary{}, err
	}

	bs, err := spec.Archiver.BackupSource(spec.Request)
	if err != nil {
		return Summary{}, err
	}
	meta := record.Archive{
		DLE:      spec.Request.DLE,
		Host:     spec.Host,
		Path:     spec.Request.SourcePath,
		Archiver: spec.Archiver.Name(),
		Compress: pl.Codec,
		Encrypt:  pl.EncryptScheme,
		Level:    spec.Request.Level,
		BaseSlot: spec.BaseSlot,
	}

	var unc, comp atomic.Int64
	report := func() {
		if prog != nil {
			prog(unc.Load(), comp.Load())
		}
	}

	srcExec := bs.Exec
	if srcExec == nil {
		srcExec = programs.Local()
	}
	tarCmd := bs.Stage
	tarCmd.Tap = func(n int64) { unc.Store(n); report() } // uncompressed (honored when tar runs locally)
	src := xfer.NewPrograms(srcExec).Add(tarCmd)
	if pl.CompressClient && compF.Forward.Name != "" {
		src.Add(compF.Forward)
	}
	if pl.EncryptClient && encF.Forward.Name != "" {
		src.Add(encF.Forward)
	}
	src.Finishing(func() (xfer.Produced, error) {
		res, ferr := bs.Finish()
		if ferr != nil {
			return xfer.Produced{}, ferr
		}
		if res == nil {
			return xfer.Produced{}, nil
		}
		return xfer.Produced{Uncompressed: res.Uncompressed, FileCount: res.FileCount, Members: res.Members}, nil
	}).OnCleanup(bs.Cleanup)

	var filterCmds []programs.Cmd
	if !pl.CompressClient && compF.Forward.Name != "" {
		filterCmds = append(filterCmds, compF.Forward)
	}
	if !pl.EncryptClient && encF.Forward.Name != "" {
		filterCmds = append(filterCmds, encF.Forward)
	}

	sink := &mediumSink{w: s.w, meta: meta}
	res, terr := xfer.Transfer(src, xfer.NewFilters(filterCmds...), sink,
		xfer.Opts{Progress: func(n int64) { comp.Store(n); report() }})
	if terr != nil {
		return Summary{}, terr
	}
	arch := sink.arch
	arch.Uncompressed = res.Uncompressed
	arch.FileCount = res.FileCount
	arch.Members = res.Produced.Members
	if err := s.w.Commit(arch, sink.parts); err != nil {
		return Summary{}, err
	}
	// Cache the member list server-side so browse/structural-verify read it without media.
	if len(arch.Members) > 0 {
		_ = s.clerk.mindex.Store(s.w.SlotID(), arch.DLE, arch.Level, arch.Members)
	}
	return Summary{FileCount: arch.FileCount, Uncompressed: arch.Uncompressed, Compressed: arch.Compressed, Codec: arch.Compress}, nil
}

// Copy re-authors one already-stored archive (read from a plan job's Source) onto this slot's
// volumes: the same on-medium bytes re-split with no transform (copySink re-checksums against
// the recorded sha, never recompresses). meta is the source archive's record (member-free, as
// the catalog holds it); Copy loads the members itself (keyed by ref) so the target writes a
// real member index. It commits the archive into the slot (CopyArchive does).
func (s *Session) Copy(src xfer.Source, ref Ref, meta record.Archive) error {
	meta.Members, _ = s.clerk.Members(ref)
	_, err := xfer.Transfer(src, xfer.NewFilters(), &copySink{w: s.w, meta: meta}, xfer.Opts{})
	return err
}
