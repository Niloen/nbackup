package dumper

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
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
// transfers the stream into an ingestion Sink the store hands out — then Sink.Commit lands and
// records it. The scheme and tar live here; the store only lands and records bytes.

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

// dumpItem archives a single DLE into the store: it acquires an ingestion Sink, transfers the
// encoded archive into it, and commits it — driving the run tracker from the committed record. It
// owns the run-tracker lifecycle and describes the backup; the store lands and records the bytes.
func (d *Dumper) dumpItem(ctx context.Context, fs archiveio.ArchiveWriteStore, item planner.Item, tr *progress.Tracker, logf func(format string, args ...any)) (err error) {
	// The progress tracker keys and displays DLEs by their host:path identity; the
	// seal and filenames keep the internal slug.
	pname := item.DLE.ID()
	tr.StartDLE(pname)
	var committed record.Archive
	defer func() {
		if err != nil {
			tr.FinishDLE(pname, 0, 0, 0, err)
		} else {
			tr.FinishDLE(pname, committed.FileCount, committed.Uncompressed, committed.Compressed, nil)
		}
	}()

	spec, err := d.backupSpec(item)
	if err != nil {
		return err
	}

	logf("archiving %s (L%d)", item.DLE.ID(), item.Level)
	committed, err = d.dumpArchive(ctx, fs, item.EstBytes, spec, func(uncompressed, compressed int64) { tr.AddBytes(pname, uncompressed, compressed) })
	if err != nil {
		// An unreadable file makes tar exit fatally (it never silently ships a partial
		// archive — that would betray recoverability), so name the likely cause and fix
		// rather than leaving the operator with a bare "exit status 2".
		if strings.Contains(err.Error(), "Permission denied") {
			return fmt.Errorf("archive %s: %w\n(a source file is unreadable — run nb as a user that can read every file under %s, e.g. via sudo/root, or exclude it in the dumptype)", item.DLE.ID(), err, item.DLE.Path)
		}
		return fmt.Errorf("archive %s: %w", item.DLE.ID(), err)
	}

	sizeLabel := "compressed"
	if committed.Compress == "none" {
		sizeLabel = "stored" // no compressor in the pipe; "compressed" would be a lie
	}
	if committed.FileCount == 0 {
		// An incremental with nothing changed still writes tar's structural overhead
		// (archive header/footer + directory census); say so rather than the puzzling
		// "0 file(s), 10.24 kB stored".
		logf("  no changed files (%s of tar metadata)", sizeutil.FormatBytes(committed.Compressed))
	} else {
		logf("  %d file(s), %s %s", committed.FileCount, sizeutil.FormatBytes(committed.Compressed), sizeLabel)
	}
	return nil
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
// server-side as local Filters) → an ingestion xfer.Sink the store hands out, which the transfer
// seals on commit. prog, if non-nil, receives running (uncompressed, compressed) counts. It returns
// the archive record with its final sizes + file count for the caller's tracker and log.
func (d *Dumper) dumpArchive(ctx context.Context, fs archiveio.ArchiveWriteStore, est int64, spec BackupSpec, prog func(uncompressed, compressed int64)) (record.Archive, error) {
	pl := d.placement(spec.DumpType)
	compF, err := compress.Filter(pl.CompressScheme, pl.CompressOpts)
	if err != nil {
		return record.Archive{}, err
	}
	encF, err := crypt.Filter(pl.EncryptScheme, pl.EncryptOpts)
	if err != nil {
		return record.Archive{}, err
	}

	bs, err := spec.Archiver.BackupSource(spec.Request)
	if err != nil {
		return record.Archive{}, err
	}
	aspec := archiveio.ArchiveSpec{
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
	src := xfer.NewProgramChain(srcExec).Add(tarCmd).Add(fused...)
	src.Finishing(func() (xfer.SourceStats, error) {
		res, ferr := bs.Finish()
		if ferr != nil {
			return xfer.SourceStats{}, ferr
		}
		if res == nil {
			return xfer.SourceStats{}, nil
		}
		return xfer.SourceStats{Uncompressed: res.Uncompressed, FileCount: res.FileCount, Members: res.Members}, nil
	}).OnCleanup(bs.Cleanup)

	// Create the ingestion Sink before the transfer spawns tar, so back-pressure (a full holding
	// disk, or a busy backing medium) gates the dump before any heavy work starts. The store meters
	// the bytes that land itself (it must, for the checksum + size).
	sink, err := fs.NewArchive(aspec, est)
	if err != nil {
		return record.Archive{}, err
	}
	// Progress is layered on here, by the one caller that wants it: wrap the sink to tap the running
	// compressed count for live `nb status` (symmetric with tarCmd.Tap's uncompressed side). The store
	// stays observability-free.
	sink = archiveio.MeterArchive(sink, func(n int64) { comp.Store(n); report() })
	// Release the sink's resources (for a direct landing write, its backing permit) on every exit
	// path — success, a faulted transfer, or a promote failure. The permit is held from NewArchive,
	// so without this a failed dump would leak it and stall the next direct writer; Close is the
	// symmetric counterpart to that acquire, independent of whether Commit ran.
	defer sink.Close()
	// Transfer drives the whole ingestion: it streams the encoded archive into the sink and, on a
	// clean transfer, has the sink seal it (footer + placement) against the producer's totals,
	// returning those totals.
	if _, terr := xfer.Transfer(ctx, src, filters, sink); terr != nil {
		return record.Archive{}, terr
	}
	// The archive is durably committed to the store; only now promote the archiver's new
	// incremental state into its library (Amanda's rename-on-success). Until here the dump wrote a
	// ".new" side file, so the transfer failing above left the base a retry builds on untouched —
	// a killed tar can never corrupt the chain.
	if bs.Promote != nil {
		if err := bs.Promote(); err != nil {
			return record.Archive{}, fmt.Errorf("promote incremental state: %w", err)
		}
	}
	// The store sealed the archive and recorded the authoritative catalog record itself; the caller
	// needs only the final tallies for its tracker + log, so read them straight off the committed
	// record rather than rebuilding it from the progress counters.
	arch, _ := sink.Result()
	return arch, nil
}
