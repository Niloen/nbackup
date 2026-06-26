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

// dump.go is the engine's write-side transfer composition (the Dumper): it builds the tar
// source and the encode filters, places each transform on the client or the server, and runs
// the transfer into a clerk-provided medium sink — then the clerk commits + records. The codec
// and tar live here, in the operation; the clerk only lands and records bytes.

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
	Codec          string
	CompressOpts   compress.Options
	CompressClient bool
	EncryptScheme  string
	EncryptOpts    crypt.Options
	EncryptClient  bool
}

// encodePlacement resolves a dumptype's encode recipe from config: the global codec, the
// per-dumptype encryption scheme/opts, and where each transform runs.
func (e *Engine) encodePlacement(dumpType string) EncodePlacement {
	encScheme, encOpts := e.encryptionFor(dumpType)
	return EncodePlacement{
		Codec:          e.codec,
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
// non-nil, receives running (uncompressed, compressed) counts. It returns a Summary.
func (e *Engine) dumpArchive(session *clerk.Session, spec BackupSpec, prog func(uncompressed, compressed int64)) (clerk.Summary, error) {
	pl := e.encodePlacement(spec.DumpType)
	compF, err := compress.Filter(pl.Codec, pl.CompressOpts)
	if err != nil {
		return clerk.Summary{}, err
	}
	encF, err := crypt.Filter(pl.EncryptScheme, pl.EncryptOpts)
	if err != nil {
		return clerk.Summary{}, err
	}

	bs, err := spec.Archiver.BackupSource(spec.Request)
	if err != nil {
		return clerk.Summary{}, err
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

	sink := &mediumSink{session: session, meta: meta}
	res, terr := xfer.Transfer(src, xfer.NewFilters(filterCmds...), sink,
		xfer.Opts{Progress: func(n int64) { comp.Store(n); report() }})
	if terr != nil {
		return clerk.Summary{}, terr
	}
	return session.Commit(sink.measured, sink.parts, res.Produced.FileCount, res.Produced.Uncompressed, res.Produced.Members)
}

// mediumSink is the operation's xfer.Sink bridge to the clerk's write endpoint: it drains the
// encoded stream into the slot writer (metering + splitting) and keeps the measured archive +
// parts for the dumpArchive's Commit. A fresh dump uses this; a copy uses copySink.
type mediumSink struct {
	session  *clerk.Session
	meta     record.Archive
	measured record.Archive
	parts    []record.FilePos
}

func (m *mediumSink) Drain(in io.Reader, progress func(int64)) (xfer.SinkResult, error) {
	arch, parts, err := m.session.WriteArchive(m.meta, in, progress)
	if err != nil {
		return xfer.SinkResult{}, err
	}
	m.measured, m.parts = arch, parts
	return xfer.SinkResult{Compressed: arch.Compressed, SHA256: arch.SHA256}, nil
}

// copySink is the operation's xfer.Sink bridge for a copy: it drains the source's raw bytes
// into the clerk's passthrough CopyArchive (verify + commit on the spot).
type copySink struct {
	session *clerk.Session
	meta    record.Archive
}

func (s *copySink) Drain(in io.Reader, _ func(int64)) (xfer.SinkResult, error) {
	return xfer.SinkResult{}, s.session.CopyArchive(s.meta, in)
}
