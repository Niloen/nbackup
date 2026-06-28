package dumper

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
	"github.com/Niloen/nbackup/internal/xfer"
)

// encode.go is the producer's write-side scheme work, the mirror of the engine's decode: it builds
// the tar source and the encode filters, places each transform on the client or the server, and
// runs the transfer into a clerk-provided medium sink — then the clerk commits + records. The
// scheme and tar live here; the clerk only lands and records bytes.

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
// schemes to apply, their invocation options, and where each runs. A transform `at: client` rides
// in the SOURCE (fused with tar on the client, so plaintext never leaves it); otherwise it is a
// local Filter (server-side).
type EncodePlacement struct {
	CompressScheme string
	CompressOpts   compress.Options
	CompressClient bool
	EncryptScheme  string
	EncryptOpts    crypt.Options
	EncryptClient  bool
}

// dumpItem archives a single DLE into the open slot session, returning the committed archive and
// its on-medium position for the consumer to record (the producer writes the bytes; only the
// consumer touches the catalog). It owns the run-tracker lifecycle and describes the backup; the
// session moves the bytes.
func (d *Dumper) dumpItem(session *clerk.Session, item planner.Item, tr *progress.Tracker, logf func(format string, args ...any)) (arch record.Archive, pos record.ArchivePos, err error) {
	// The progress tracker keys and displays DLEs by their host:path identity; the
	// seal and filenames keep the internal slug.
	pname := item.DLE.ID()
	tr.StartDLE(pname)
	var sum clerk.Summary
	defer func() {
		if err != nil {
			tr.FinishDLE(pname, 0, 0, 0, err)
		} else {
			tr.FinishDLE(pname, sum.FileCount, sum.Uncompressed, sum.Compressed, nil)
		}
	}()

	spec, err := d.backupSpec(item)
	if err != nil {
		return record.Archive{}, record.ArchivePos{}, err
	}

	logf("archiving %s (L%d)", item.DLE.ID(), item.Level)
	sum, arch, pos, err = d.dumpArchive(session, spec, func(uncompressed, compressed int64) { tr.AddBytes(pname, uncompressed, compressed) })
	if err != nil {
		// An unreadable file makes tar exit fatally (it never silently ships a partial
		// archive — that would betray recoverability), so name the likely cause and fix
		// rather than leaving the operator with a bare "exit status 2".
		if strings.Contains(err.Error(), "Permission denied") {
			return record.Archive{}, record.ArchivePos{}, fmt.Errorf("archive %s: %w\n(a source file is unreadable — run nb as a user that can read every file under %s, e.g. via sudo/root, or exclude it in the dumptype)", item.DLE.ID(), err, item.DLE.Path)
		}
		return record.Archive{}, record.ArchivePos{}, fmt.Errorf("archive %s: %w", item.DLE.ID(), err)
	}

	sizeLabel := "compressed"
	if sum.Compress == "none" {
		sizeLabel = "stored" // no compressor in the pipe; "compressed" would be a lie
	}
	if sum.FileCount == 0 {
		// An incremental with nothing changed still writes tar's structural overhead
		// (archive header/footer + directory census); say so rather than the puzzling
		// "0 file(s), 10.24 kB stored".
		logf("  no changed files (%s of tar metadata)", sizeutil.FormatBytes(sum.Compressed))
	} else {
		logf("  %d file(s), %s %s", sum.FileCount, sizeutil.FormatBytes(sum.Compressed), sizeLabel)
	}
	return arch, pos, nil
}

// backupSpec describes the backup of one planned item: it resolves the archiver and builds the
// request (with the dumptype's excludes), and for an incremental requires the base incremental
// state to be present. It is pure intent — the schemes, transform placement, and storage record
// are derived downstream.
func (d *Dumper) backupSpec(item planner.Item) (BackupSpec, error) {
	ar, err := d.archiverFor(item.DLE.DumpTypeName(), item.DLE.Host)
	if err != nil {
		return BackupSpec{}, err
	}
	req := archiver.BackupRequest{
		DLE:        item.Name,
		SourcePath: item.DLE.Path,
		Level:      item.Level,
		BaseLevel:  -1,
		Exclude:    d.exclude(item.DLE.DumpTypeName()),
	}
	if item.Level >= 1 {
		req.BaseLevel = item.BaseLevel
		if !ar.HasBase(item.Name, item.BaseLevel) {
			return BackupSpec{}, fmt.Errorf("DLE %s: incremental L%d needs the L%d incremental state but it is missing — "+
				"the prior dump wrote it under the host's state_dir; if that path moved (e.g. a relative state_dir/workdir while `nb` ran from a different directory), "+
				"set state_dir to an absolute path and re-run a full (L0)",
				item.DLE.ID(), item.Level, item.BaseLevel)
		}
	}
	return BackupSpec{
		Archiver: ar,
		Request:  req,
		Host:     item.DLE.Host,
		BaseSlot: item.BaseSlot,
		DumpType: item.DLE.DumpTypeName(),
	}, nil
}

// dumpArchive composes the encode transfer for one archive — the archiver's tar source (on its
// host) → the encode filters placed per the dumptype (client-side fused into the source,
// server-side as local Filters) → the slot's medium sink — then commits the archive. prog, if
// non-nil, receives running (uncompressed, compressed) counts. It returns a Summary and the
// committed archive's metadata + on-medium position, for the caller to hand to the consumer.
func (d *Dumper) dumpArchive(session *clerk.Session, spec BackupSpec, prog func(uncompressed, compressed int64)) (clerk.Summary, record.Archive, record.ArchivePos, error) {
	pl := d.placement(spec.DumpType)
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
	fused, filters := xfer.SplitTransforms(
		xfer.Transform{Cmd: compF.Forward, Fused: pl.CompressClient},
		xfer.Transform{Cmd: encF.Forward, Fused: pl.EncryptClient},
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
	sum, committed, pos, cerr := session.Commit(sink.measured, sink.parts, produced.FileCount, produced.Uncompressed, produced.Members)
	if cerr != nil {
		return clerk.Summary{}, record.Archive{}, record.ArchivePos{}, cerr
	}
	// The archive is durably committed to the dump medium; only now promote the archiver's
	// new incremental state into its library (Amanda's rename-on-success). Until here the
	// dump wrote a ".new" side file, so the transfer or commit failing above left the base
	// a retry builds on untouched — a killed tar can never corrupt the chain.
	if bs.Promote != nil {
		if err := bs.Promote(); err != nil {
			return clerk.Summary{}, record.Archive{}, record.ArchivePos{}, fmt.Errorf("promote incremental state: %w", err)
		}
	}
	return sum, committed, pos, nil
}

// mediumSink is the producer's xfer.Sink bridge to the clerk's write endpoint: it drains the
// encoded stream into the slot writer (metering + splitting) and keeps the measured archive + parts
// for dumpArchive's Commit.
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
