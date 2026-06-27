package engine

import (
	"io"
	"sync/atomic"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
	"github.com/Niloen/nbackup/internal/xfer"
)

// dump.go is NBackup's write-side scheme operation (the Dumper), the mirror of decode.go: it
// builds the tar source and the encode filters, places each transform on the client or the
// server, and runs the transfer into a clerk-provided medium sink — then the clerk commits +
// records. The scheme and tar live here, in the operation; the clerk only lands and records
// bytes. The encoder depends on just one slice of the orchestrator — how to resolve a
// dumptype's encode recipe — not the whole engine.
type encoder struct {
	placement func(dumpType string) EncodePlacement // a dumptype's resolved encode recipe
}

// newEncoder wires an encoder to the engine's dumptype-recipe resolution.
func (e *Engine) newEncoder() *encoder {
	return &encoder{placement: e.encodePlacement}
}

// BackupSpec describes one archive to back up: the resolved archiver and its request, plus the
// identity bits not in the request (the DLE's host, the base slot for an incremental, and the
// dumptype that selects the transform placement). Pure intent — no storage record.
type BackupSpec struct {
	Archiver archiver.Archiver
	Request  archiver.BackupRequest
	Host     string
	BaseSlot string
	DumpType string
}

// EncodePlacement is the write-side transform recipe for one dumptype: which compress/encrypt
// schemes to apply, their invocation options, and where each runs. A transform `at: client`
// rides in the SOURCE (fused with tar on the client, so plaintext never leaves it); otherwise
// it is a local Filter (server-side).
type EncodePlacement struct {
	CompressScheme string
	CompressOpts   compress.Options
	CompressClient bool
	EncryptScheme  string
	EncryptOpts    crypt.Options
	EncryptClient  bool
}

// encodePlacement resolves a dumptype's encode recipe from config: the global compression
// scheme, the per-dumptype encryption scheme/opts, and where each transform runs.
func (e *Engine) encodePlacement(dumpType string) EncodePlacement {
	encScheme, encOpts := e.encryptionFor(dumpType)
	return EncodePlacement{
		CompressScheme: e.compressScheme,
		CompressOpts:   e.fopts,
		CompressClient: e.cfg.ResolveDumpType(dumpType).Compress == "client",
		EncryptScheme:  encScheme,
		EncryptOpts:    encOpts,
		EncryptClient:  e.cfg.EncryptionFor(dumpType).At == "client",
	}
}

// dumpArchive composes the encode transfer for one archive — the archiver's tar source (on its
// host) → the encode filters placed per the dumptype (client-side fused into the source,
// server-side as local Filters) → the slot's medium sink — then commits the archive. prog, if
// non-nil, receives running (uncompressed, compressed) counts. It returns a Summary and the
// committed archive's metadata + on-medium position, for the caller to hand to the orchestrator.
func (enc *encoder) dumpArchive(session *clerk.Session, spec BackupSpec, prog func(uncompressed, compressed int64)) (clerk.Summary, record.Archive, record.ArchivePos, error) {
	pl := enc.placement(spec.DumpType)
	compF, err := compress.Filter(pl.CompressScheme, pl.CompressOpts)
	if err != nil {
		return clerk.Summary{}, record.Archive{}, record.ArchivePos{}, err
	}
	encF, err := crypt.Filter(pl.EncryptScheme, pl.EncryptOpts)
	if err != nil {
		return clerk.Summary{}, record.Archive{}, record.ArchivePos{}, err
	}

	bs, err := spec.Archiver.BackupSource(spec.Request)
	if err != nil {
		return clerk.Summary{}, record.Archive{}, record.ArchivePos{}, err
	}
	meta := record.Archive{
		DLE:      spec.Request.DLE,
		Host:     spec.Host,
		Path:     spec.Request.SourcePath,
		Archiver: spec.Archiver.Name(),
		Compress: pl.CompressScheme,
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
	// Place each encode step: a client-side transform fuses with tar in the source (plaintext
	// never leaves the client); a server-side one lands in the local filters.
	fused, filters := splitTransforms(
		transform{cmd: compF.Forward, fused: pl.CompressClient},
		transform{cmd: encF.Forward, fused: pl.EncryptClient},
	)
	src := xfer.NewPrograms(srcExec).Add(tarCmd).Add(fused...)
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

	// The medium writer meters the bytes that land (it must, for the checksum + size); tap
	// that running count for live `nb status`, symmetric with tarCmd.Tap's uncompressed side.
	sink := &mediumSink{session: session, meta: meta, tap: func(n int64) { comp.Store(n); report() }}
	produced, terr := xfer.Transfer(src, filters, sink)
	if terr != nil {
		return clerk.Summary{}, record.Archive{}, record.ArchivePos{}, terr
	}
	return session.Commit(sink.measured, sink.parts, produced.FileCount, produced.Uncompressed, produced.Members)
}

// mediumSink is the operation's xfer.Sink bridge to the clerk's write endpoint: it drains the
// encoded stream into the slot writer (metering + splitting) and keeps the measured archive +
// parts for the dumpArchive's Commit. A fresh dump uses this; a copy uses copySink.
type mediumSink struct {
	session  *clerk.Session
	meta     record.Archive
	tap      func(landed int64) // running count of bytes that have landed, for live status
	measured record.Archive
	parts    []record.FilePos
}

func (m *mediumSink) Drain(in io.Reader) error {
	arch, parts, err := m.session.WriteArchive(m.meta, in, m.tap)
	if err != nil {
		return err
	}
	m.measured, m.parts = arch, parts
	return nil
}

// copySink is the operation's xfer.Sink bridge for a copy: it drains the source's raw bytes
// into the clerk's passthrough CopyArchive (verify + commit on the spot).
type copySink struct {
	session *clerk.Session
	meta    record.Archive
}

func (s *copySink) Drain(in io.Reader) error {
	return s.session.CopyArchive(s.meta, in)
}
