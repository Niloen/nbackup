package clerk

import (
	"errors"
	"io"
	"sync/atomic"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/slotio"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
	"github.com/Niloen/nbackup/internal/xfer"
)

// EncodePlacement is the write-side transform recipe for one dumptype: the compress/encrypt
// invocation options plus where each runs. A transform `at: client` rides in the SOURCE
// (fused with tar on the client, so plaintext never leaves it); otherwise it is a local
// Filter (server-side). The schemes themselves come from the record being written.
type EncodePlacement struct {
	CompressOpts   compress.Options
	CompressClient bool
	EncryptOpts    crypt.Options
	EncryptClient  bool
}

// --- the two slotio-coupled endpoints ---

// mediumSink is the xfer.Sink that lands an archive's bytes onto a slot's volumes via the
// open slotio.Writer: it meters (sha256 + size) and splits the stream into parts, keeping
// the measured archive + part positions for the caller to record once the producer's stats
// are merged in. It is the write peer of the archive Source.
type mediumSink struct {
	w    *slotio.Writer
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
// (slotio.Writer.CopyArchive). It is the raw-passthrough peer of mediumSink.
type copySink struct {
	w    *slotio.Writer
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

// --- operation verbs ---

// Backup composes a dump as one transfer: the archiver's tar source (on its host), the
// encode filters placed per the dumptype's EncodePlacement (client-side ones fused into the
// source, server-side ones as local Filters), into the slot writer's medium sink. It returns
// the fully measured archive (producer + sink stats merged) and its part positions for the
// caller to record; prog, if non-nil, receives running (uncompressed, compressed) counts.
func (c *Clerk) Backup(w *slotio.Writer, bs *archiver.BackupSource, meta record.Archive, dumpType string, prog func(uncompressed, compressed int64)) (record.Archive, []record.FilePos, error) {
	pl := c.deps.EncodePlacement(dumpType)
	compF, err := compress.Filter(meta.Compress, pl.CompressOpts)
	if err != nil {
		cleanup(bs)
		return record.Archive{}, nil, err
	}
	encF, err := crypt.Filter(meta.Encrypt, pl.EncryptOpts)
	if err != nil {
		cleanup(bs)
		return record.Archive{}, nil, err
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

	sink := &mediumSink{w: w, meta: meta}
	res, terr := xfer.Transfer(src, xfer.NewFilters(filterCmds...), sink,
		xfer.Opts{Progress: func(n int64) { comp.Store(n); report() }})
	if terr != nil {
		return record.Archive{}, nil, terr
	}
	arch := sink.arch
	arch.Uncompressed = res.Uncompressed
	arch.FileCount = res.FileCount
	arch.Members = res.Produced.Members
	return arch, sink.parts, nil
}

// cleanup runs a BackupSource's scratch cleanup if it has one (for Backup's pre-transfer
// error paths, before the transfer would own the cleanup).
func cleanup(bs *archiver.BackupSource) {
	if bs.Cleanup != nil {
		bs.Cleanup()
	}
}

// Copy re-authors one archive onto the target writer's volumes: the same on-medium bytes
// re-split with no transform (copySink re-checksums against the seal, never recompresses).
// The source is built from the caller's opener (threaded across a copy's archives).
func (c *Clerk) Copy(w *slotio.Writer, parts []record.FilePos, want slotio.Expect, opener slotio.PartOpener, meta record.Archive) error {
	src, err := c.partsSource(parts, want, opener)
	if err != nil {
		return err
	}
	_, err = xfer.Transfer(src, xfer.NewFilters(), &copySink{w: w, meta: meta}, xfer.Opts{})
	return err
}

// VerifyChecksum re-reads an archive's raw parts and hashes them, reporting whether the hash
// matches the seal's sha. It is a transfer with no decode: source → Hash sink. A clean read
// whose hash differs returns (false, nil); a read fault returns (false, err).
func (c *Clerk) VerifyChecksum(parts []record.FilePos, want slotio.Expect, sha string, opener slotio.PartOpener) (bool, error) {
	src, err := c.partsSource(parts, want, opener)
	if err != nil {
		return false, err
	}
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

// ListMembers reads an archive's parts, decodes them (server-side Filters), and lists the
// members (`tar -t`) — the verify path's structural check. It returns the listed members and
// the raw transfer error (role-tagged) for the caller to classify and hint.
func (c *Clerk) ListMembers(parts []record.FilePos, want slotio.Expect, codec, encrypt string, opener slotio.PartOpener, arch archiver.Archiver) ([]string, error) {
	decrypt, decompress, err := c.DecodeFilters(codec, encrypt)
	if err != nil {
		return nil, err
	}
	src, err := c.partsSource(parts, want, opener)
	if err != nil {
		return nil, err
	}
	res, terr := xfer.Transfer(src, LocalDecode(decrypt, decompress), listSink{arch: arch}, xfer.Opts{})
	return res.SinkResult.Members, terr
}
