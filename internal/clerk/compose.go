package clerk

import (
	"errors"
	"io"
	"sync/atomic"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
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

// listSink consumes the decoded stream by listing its members (`tar -t`) — the verify path's
// structural check. A bad stream (truncated decode, not-a-tar) fails the archiver's List; the
// members feed the seal comparison.
type listSink struct{ arch archiver.Archiver }

func (s listSink) Drain(in io.Reader, _ func(int64)) (xfer.SinkResult, error) {
	members, err := s.arch.List(in)
	return xfer.SinkResult{Members: members}, err
}

// --- shared filter builders (the one home for record-transforms → pipeline) ---

// DecodeFilters returns the decrypt and decompress commands that reverse an archive's
// recorded transforms, keyed by the engine's default decode options. A none scheme yields an
// empty Cmd, which a transfer skips. Shared by the restore/verify verbs and by drill.
func (c *Clerk) DecodeFilters(codec, encrypt string) (decrypt, decompress programs.Cmd, err error) {
	cf, err := compress.Filter(codec, c.deps.CompressOpts())
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	ef, err := crypt.Filter(encrypt, c.deps.DecryptOpts())
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	return ef.Reverse, cf.Reverse, nil
}

// LocalDecode builds a local Filters chain (decrypt then decompress) from DecodeFilters'
// commands, skipping the none/identity ones.
func LocalDecode(decrypt, decompress programs.Cmd) xfer.Filters {
	f := xfer.NewFilters()
	if decrypt.Name != "" {
		f = f.Add(decrypt)
	}
	if decompress.Name != "" {
		f = f.Add(decompress)
	}
	return f
}

// --- the write-side slot session ---

// Session authors one slot: the engine opens it over a archiveio.Writer, backs up (or copies)
// each archive into it, and seals via the writer. It is the write peer of the read verbs —
// the single place a record.Archive and its parts are assembled, so the engine describes a
// backup (intent) and the session produces the artifact, never the reverse.
type Session struct {
	clerk *Clerk
	w     *archiveio.Writer
}

// OpenSlot starts a write session over an open slot writer.
func (c *Clerk) OpenSlot(w *archiveio.Writer) *Session { return &Session{clerk: c, w: w} }

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

// VerifyChecksum hashes an archive's raw stream (from a plan job's Source) and reports whether
// it matches the recorded sha. It is a transfer with no decode: source → Hash sink. A clean
// read whose hash differs returns (false, nil); a read fault returns (false, err).
func (c *Clerk) VerifyChecksum(src xfer.Source, sha string) (bool, error) {
	_, terr := xfer.Transfer(src, xfer.NewFilters(), xfer.Hash(sha), xfer.Opts{})
	if terr != nil {
		// A mismatch is the Hash sink's (clean-read) failure; anything else is a read fault.
		var xe *xfer.Error
		if errors.As(terr, &xe) && xe.Role == xfer.RoleSink {
			return false, nil
		}
		return false, terr
	}
	return true, nil
}

// ListMembers decodes an archive's stream (from a plan job's Source, server-side Filters) and
// lists the members (`tar -t`) — the verify path's structural check. It returns the listed
// members and the raw transfer error (role-tagged) for the caller to classify and hint.
func (c *Clerk) ListMembers(src xfer.Source, codec, encrypt string, arch archiver.Archiver) ([]string, error) {
	decrypt, decompress, err := c.DecodeFilters(codec, encrypt)
	if err != nil {
		return nil, err
	}
	res, terr := xfer.Transfer(src, LocalDecode(decrypt, decompress), listSink{arch: arch}, xfer.Opts{})
	return res.SinkResult.Members, terr
}
