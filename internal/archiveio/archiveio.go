// Package archiveio is NBackup's archive data path — the nuts and bolts of moving an
// archive's bytes between the catalog/media and a destination. It selects a copy, mounts
// the volumes (via the librarian), composes the payload's decode pipeline from config +
// the record, places each stage on an executor, and drives slotio. The engine above it
// orchestrates (what to back up, when, retention, drill selection); archiveio does the
// byte-moving — the Amanda driver-vs-dumper/taper split.
//
// It owns only mechanics. Everything it needs from the orchestrator — catalog placement,
// librarian mounting, executor/archiver resolution, and the config-derived transform
// options — comes through Deps. That interface IS the boundary between deciding and doing;
// where it should sit (engine-owned, as now, or relocated into the data path) is the open
// question this layer is meant to make visible.
//
// Read verbs only for now; the write path (Produce) follows once the read shape settles.
package archiveio

import (
	"errors"
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/slotio"
	"github.com/Niloen/nbackup/internal/transform"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
	"github.com/Niloen/nbackup/internal/xfer"
)

// Ref is the logical identity of one archive: which slot, DLE, and level. The data path
// resolves it to physical parts on a copy.
type Ref struct {
	Slot  string
	DLE   string
	Level int
}

func (r Ref) expect() slotio.Expect { return slotio.Expect{Slot: r.Slot, DLE: r.DLE, Level: r.Level} }

// Deps is what the data path needs from the orchestrator. The engine implements it; this
// interface is the explicit boundary between the control plane (deciding) and the data
// plane (doing).
type Deps interface {
	// PlacementsFor returns the copies of a slot, in read-preference order (the engine's
	// own medium first).
	PlacementsFor(slotID string) []catalog.Placement
	// LibrarianFor returns a librarian that can mount and read a medium's volumes.
	LibrarianFor(medium string) (*librarian.Librarian, error)
	// Limiter returns the medium's shared bandwidth cap (nil = uncapped).
	Limiter(medium string) *xfer.Limiter
	// Executor returns the transport that runs programs on a host (Local or remote).
	Executor(host string) programs.Executor
	// RestoreArchiver resolves the archiver plugin that extracts on the given host.
	RestoreArchiver(typeName, host string) (archiver.Archiver, error)
	// CompressOpts / DecryptOpts are the config-derived invocation options for the
	// decode pipeline's decompress and decrypt stages.
	CompressOpts() compress.Options
	DecryptOpts() crypt.Options
}

// ErrMissingCopy marks a read failure where the catalog knows of no available copy of the
// requested slot/archive. Callers classify it via errors.Is, so classification does not
// depend on the message wording.
var ErrMissingCopy = errors.New("no available copy")

// IO is the archive data path. Construct one with New, sharing the orchestrator's Deps.
type IO struct {
	deps   Deps
	reader *slotio.Reader
}

// New returns a data path backed by deps.
func New(deps Deps) *IO { return &IO{deps: deps, reader: slotio.NewReader()} }

// OpenRaw opens an archive's raw (undecoded, on-medium) part stream, with copy selection
// and fail-over: medium "" tries every copy (preferring the engine's own), a set medium
// reads only that copy so a fault on it is not masked by another. The caller closes it and
// feeds it into a decode pipeline (or re-splits it, for a copy).
func (a *IO) OpenRaw(ref Ref, medium string) (io.ReadCloser, error) {
	return a.eachPlacement(ref, medium, func(parts []record.FilePos, p catalog.Placement) (io.ReadCloser, error) {
		return a.OpenPartsOn(parts, ref.expect(), p.Medium)
	})
}

// OpenPartsOn opens a specific copy's parts (no copy selection) on a named medium.
func (a *IO) OpenPartsOn(parts []record.FilePos, want slotio.Expect, medium string) (io.ReadCloser, error) {
	opener, err := a.PartOpener(medium)
	if err != nil {
		return nil, err
	}
	return a.OpenParts(parts, want, opener)
}

// OpenParts opens an archive's parts via a caller-held opener — the primitive the verify
// path uses, threading one opener per copy across all of that copy's archives.
func (a *IO) OpenParts(parts []record.FilePos, want slotio.Expect, opener slotio.PartOpener) (io.ReadCloser, error) {
	return a.reader.Open(parts, want, opener)
}

// VerifyParts re-hashes an archive's raw parts (via a caller-held opener) and compares to
// the seal's checksum.
func (a *IO) VerifyParts(parts []record.FilePos, want slotio.Expect, sha string, opener slotio.PartOpener) (bool, error) {
	return a.reader.VerifyParts(parts, want, sha, opener)
}

// PartOpener returns a mounting opener for a medium's volumes (rate-limited by its
// shared cap), for callers that drive the read loop themselves (verify).
func (a *IO) PartOpener(medium string) (slotio.PartOpener, error) {
	lib, err := a.deps.LibrarianFor(medium)
	if err != nil {
		return nil, err
	}
	lim := a.deps.Limiter(medium)
	return func(p record.FilePos) (record.Header, io.ReadCloser, error) {
		h, rc, err := lib.ReadFileAt(p.Label, p.Epoch, p.Pos)
		if err != nil {
			return h, rc, err
		}
		return h, lim.ReadCloser(rc), nil
	}, nil
}

// DecodePipeline builds the archive payload's transform pipeline placed for the decode
// direction: the same compress+encrypt chain a dump applied, so Pipeline.Reverse() yields
// the decrypt-then-decompress stages. Decrypt — the only stage that needs the key — runs on
// the target host for a client-held key reached over `--to`, and on the server (Local)
// otherwise. Decompress always runs on the target host, so a remote restore ships
// compressed bytes over the wire rather than inflating them first.
func (a *IO) DecodePipeline(encrypt, codec string, ec config.EncryptConfig, targetHost string) (transform.Pipeline, error) {
	target := a.deps.Executor(targetHost)
	decExec := programs.Executor(programs.Local())
	copts := a.deps.DecryptOpts()
	if ec.At == "client" && targetHost != "" {
		decExec = target
		copts = crypt.Options{Program: ec.Program, PassphraseFile: ec.PassphraseFile}
	}
	compF, err := compress.Filter(codec, a.deps.CompressOpts())
	if err != nil {
		return nil, err
	}
	encF, err := crypt.Filter(encrypt, copts)
	if err != nil {
		return nil, err
	}
	// Encode order is compress then encrypt; Reverse() undoes it (decrypt, then decompress),
	// with decompress on the target host and decrypt where the key lives.
	return transform.Pipeline{
		{Filter: compF, Exec: target},
		{Filter: encF, Exec: decExec},
	}, nil
}

// Extract streams an archive's raw parts through the decode→extract pipeline into destDir
// on targetHost (members nil = a whole-archive listed-incremental chain restore; members
// set = selected-file recovery, no deletions). It returns the raw pipeline error; the
// caller adds any decrypt hint. targetHost "" extracts server-side.
func (a *IO) Extract(ref Ref, codec, encrypt, archiverType, destDir, targetHost string, ec config.EncryptConfig, members []string) error {
	target := a.deps.Executor(targetHost)
	if err := target.MkdirAll(destDir); err != nil {
		return err
	}
	arch, err := a.deps.RestoreArchiver(archiverType, targetHost)
	if err != nil {
		return err
	}
	raw, err := a.OpenRaw(ref, "")
	if err != nil {
		return err
	}

	// The transfer: read the medium → decode → extract. Decrypt is the only stage that
	// needs the key — it runs on the target (sink) when the key is client-held and reached
	// over `--to`, otherwise on the local server (Filters). Decompress runs on the target,
	// fused with tar, so a remote restore ships compressed bytes over the wire.
	copts := a.deps.DecryptOpts()
	decryptInSink := ec.At == "client" && targetHost != ""
	if decryptInSink {
		copts = crypt.Options{Program: ec.Program, PassphraseFile: ec.PassphraseFile}
	}
	encF, err := crypt.Filter(encrypt, copts)
	if err != nil {
		raw.Close()
		return err
	}
	compF, err := compress.Filter(codec, a.deps.CompressOpts())
	if err != nil {
		raw.Close()
		return err
	}

	sink := xfer.NewPrograms(target)
	if decryptInSink && encF.Reverse.Name != "" {
		sink.Add(encF.Reverse)
	}
	if compF.Reverse.Name != "" {
		sink.Add(compF.Reverse)
	}
	sink.Add(arch.RestoreStage(destDir, members))

	var filterCmds []programs.Cmd
	if !decryptInSink && encF.Reverse.Name != "" {
		filterCmds = append(filterCmds, encF.Reverse)
	}

	_, err = xfer.Transfer(xfer.Reader(raw), xfer.NewFilters(filterCmds...), sink, xfer.Opts{})
	return err
}

// RunDecodePipeline runs a decode→extract stage chain, draining the (empty) extractor
// stdout and reaping every stage. Stages are reaped in pipeline order, so when an upstream
// child fails (a wrong key, a codec drift) its error — not the downstream "truncated input"
// symptom it causes in tar — is the one returned.
func RunDecodePipeline(raw io.ReadCloser, stages ...programs.Stage) error {
	out, wait, err := programs.RunGrouped(raw, stages...)
	if err != nil {
		raw.Close()
		return err
	}
	_, copyErr := io.Copy(io.Discard, out) // tar -x writes to the fs; its stdout is empty
	out.Close()
	werr := wait()
	cerr := raw.Close() // a media-read fault on the ciphertext parts surfaces here
	if werr == nil {
		werr = copyErr
	}
	if werr == nil {
		werr = cerr
	}
	return werr
}

// eachPlacement resolves the placements holding ref's slot (all copies, or only those on
// medium when set), then tries each that carries the archive — opening it via open — until
// one succeeds, so a read fails over to another copy. It is the one place the raw read paths
// share copy selection and the missing-copy errors (ErrMissingCopy).
func (a *IO) eachPlacement(ref Ref, medium string, open func(parts []record.FilePos, p catalog.Placement) (io.ReadCloser, error)) (io.ReadCloser, error) {
	placements := a.deps.PlacementsFor(ref.Slot)
	if medium != "" {
		placements = onMedium(placements, medium)
	}
	if len(placements) == 0 {
		if medium != "" {
			return nil, fmt.Errorf("%w: slot %s has no copy on medium %q", ErrMissingCopy, ref.Slot, medium)
		}
		return nil, fmt.Errorf("%w: slot %s not in catalog (run `nb rebuild`)", ErrMissingCopy, ref.Slot)
	}
	var lastErr error
	for _, p := range placements {
		parts, ok := p.Parts(ref.DLE, ref.Level)
		if !ok {
			continue
		}
		rc, err := open(parts, p)
		if err != nil {
			lastErr = err
			continue
		}
		return rc, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w of %s %s L%d in the catalog", ErrMissingCopy, ref.Slot, ref.DLE, ref.Level)
	}
	return nil, lastErr
}

func onMedium(ps []catalog.Placement, medium string) []catalog.Placement {
	out := ps[:0:0]
	for _, p := range ps {
		if p.Medium == medium {
			out = append(out, p)
		}
	}
	return out
}
