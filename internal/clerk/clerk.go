// Package clerk is NBackup's archive data path — the composer that moves an archive's bytes
// between a DLE on a host and the media, in both directions. It is Amanda's Scribe and Clerk
// in one object: on the WRITE side it composes a dump (the archiver's tar source → encode
// filters → the slot writer's medium sink) and a copy; on the READ side it selects a copy
// (with fail-over), mounts the volumes via the librarian, opens the raw part stream, and
// composes the decode (Reader → decrypt/decompress → tar). Every operation is one
// xfer.Transfer; the filter builders here are the single home for "how the record's
// transforms become a pipeline," shared by the dump/restore/copy/verify verbs and by drill.
//
// It owns only data-movement mechanics; policy — which archiver, retention, the slot session
// (open/seal/placement), classifying a fault into a drill verdict — stays in the engine.
// Everything it needs from the orchestrator — catalog placement, librarian mounting,
// executor/archiver resolution, and the config-derived transform options/placement — comes
// through Deps. That interface IS the boundary between deciding and doing.
//
// The two archiveio-coupled endpoints — the archive Source (read) and the medium Sink (write) —
// are the only bespoke pieces; xfer stays a generic leaf (it must, since archiveio imports
// xfer.Limiter). Part concatenation lives in archiveio; clerk wraps it with copy-selection and
// volume mounting to make a Source.
package clerk

import (
	"errors"
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
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

func (r Ref) expect() archiveio.Expect {
	return archiveio.Expect{Slot: r.Slot, DLE: r.DLE, Level: r.Level}
}

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
	// EncodePlacement is the write-side peer: the per-dumptype compress/encrypt invocation
	// options plus where each transform runs (client vs the local server).
	EncodePlacement(dumpType string) EncodePlacement
}

// ErrMissingCopy marks a read failure where the catalog knows of no available copy of the
// requested slot/archive. Callers classify it via errors.Is, so classification does not
// depend on the message wording.
var ErrMissingCopy = errors.New("no available copy")

// Clerk is the archive data path. Construct one with New, sharing the orchestrator's Deps.
// It owns the member-index part of the catalog: it writes each archive's member list (cache +
// on-medium index) as it commits, and serves it on read, so member I/O lives in one place
// regardless of which operation needs it.
type Clerk struct {
	deps   Deps
	reader *archiveio.Reader
	mindex *catalog.MemberIndex
}

// New returns a data path backed by deps, using mindex as the server-side member cache.
func New(deps Deps, mindex *catalog.MemberIndex) *Clerk {
	return &Clerk{deps: deps, reader: archiveio.NewReader(), mindex: mindex}
}

// Members returns an archive's member list, lazily: from the member-index cache, else by
// reading the on-medium index (via a copy's recorded index position) and re-caching it. A nil
// list is a valid "no members" answer (an archive with no files records no index).
func (c *Clerk) Members(ref Ref) ([]string, error) {
	if members, ok, err := c.mindex.Load(ref.Slot, ref.DLE, ref.Level); err != nil {
		return nil, err
	} else if ok {
		return members, nil
	}
	for _, p := range c.deps.PlacementsFor(ref.Slot) {
		pos, ok := indexPosOf(p, ref.DLE, ref.Level)
		if !ok {
			continue
		}
		members, err := c.ReadIndex(p.Medium, pos)
		if err != nil {
			continue // try another copy
		}
		_ = c.mindex.Store(ref.Slot, ref.DLE, ref.Level, members)
		return members, nil
	}
	return nil, nil
}

// indexPosOf finds an archive's recorded member-index position on a placement (the zero
// position means the archive recorded no index — it had no members).
func indexPosOf(p catalog.Placement, dle string, level int) (record.FilePos, bool) {
	for _, a := range p.Archives {
		if a.DLE == dle && a.Level == level {
			return a.Index, a.Index != (record.FilePos{})
		}
	}
	return record.FilePos{}, false
}

// ArchiveSource opens an archive's raw (undecoded, on-medium) part stream as an xfer.Source,
// with copy selection and fail-over: medium "" tries every copy (preferring the engine's
// own), a set medium reads only that copy so a fault on it is not masked by another. It is
// the read peer of the medium sink — the one archiveio-coupled Source. The open (and thus the
// copy-selection fail-over) happens here, eagerly, so a missing copy is reported before bytes
// flow and stays classifiable (errors.Is); a volume lost mid-stream is a Source-role fault
// inside the transfer.
func (c *Clerk) ArchiveSource(ref Ref, medium string) (xfer.Source, error) {
	rc, err := c.openRaw(ref, medium)
	if err != nil {
		return nil, err
	}
	return xfer.Reader(rc), nil
}

// partsSource opens a specific copy's parts via a caller-held opener (no copy selection) as
// an xfer.Source — for loops that thread one mounted opener across all of a copy's archives
// (verify, copy).
func (c *Clerk) partsSource(parts []record.FilePos, want archiveio.Expect, opener archiveio.PartOpener) (xfer.Source, error) {
	rc, err := c.reader.Open(parts, want, opener)
	if err != nil {
		return nil, err
	}
	return xfer.Reader(rc), nil
}

// openRaw opens an archive's raw on-medium part stream with copy selection and fail-over.
func (c *Clerk) openRaw(ref Ref, medium string) (io.ReadCloser, error) {
	return c.eachPlacement(ref, medium, func(parts []record.FilePos, p catalog.Placement) (io.ReadCloser, error) {
		opener, err := c.PartOpener(p.Medium)
		if err != nil {
			return nil, err
		}
		return c.reader.Open(parts, ref.expect(), opener)
	})
}

// PartOpener returns a mounting opener for a medium's volumes (rate-limited by its shared
// cap), for callers that drive the read loop themselves — threading one opener across all of
// a copy's archives (verify, copy).
func (c *Clerk) PartOpener(medium string) (archiveio.PartOpener, error) {
	lib, err := c.deps.LibrarianFor(medium)
	if err != nil {
		return nil, err
	}
	lim := c.deps.Limiter(medium)
	return func(p record.FilePos) (record.Header, io.ReadCloser, error) {
		h, rc, err := lib.ReadFileAt(p.Label, p.Epoch, p.Pos)
		if err != nil {
			return h, rc, err
		}
		return h, lim.ReadCloser(rc), nil
	}, nil
}

// ReadIndex reads an archive's member index off a medium — the lazy fallback when the
// server-side member cache misses (a rebuilt slot not yet browsed). It mounts the volume the
// index lives on and decodes it.
func (c *Clerk) ReadIndex(medium string, pos record.FilePos) ([]string, error) {
	lib, err := c.deps.LibrarianFor(medium)
	if err != nil {
		return nil, err
	}
	_, rc, err := lib.ReadFileAt(pos.Label, pos.Epoch, pos.Pos)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return record.DecodeIndex(rc)
}

// Extract streams an archive's raw parts through the decode→extract pipeline into destDir
// on targetHost (members nil = a whole-archive listed-incremental chain restore; members
// set = selected-file recovery, no deletions). It returns the raw pipeline error; the
// caller adds any decrypt hint. targetHost "" extracts server-side.
func (c *Clerk) Extract(ref Ref, codec, encrypt, archiverType, destDir, targetHost string, ec config.EncryptConfig, members []string) error {
	target := c.deps.Executor(targetHost)
	if err := target.MkdirAll(destDir); err != nil {
		return err
	}
	arch, err := c.deps.RestoreArchiver(archiverType, targetHost)
	if err != nil {
		return err
	}

	// Build the decode filters first (a bad scheme fails before any media is opened). Decrypt
	// is the only stage that needs the key — it runs on the target (sink) when the key is
	// client-held and reached over `--to`, otherwise on the local server (Filters). Decompress
	// runs on the target, fused with tar, so a remote restore ships compressed bytes.
	copts := c.deps.DecryptOpts()
	decryptInSink := ec.At == "client" && targetHost != ""
	if decryptInSink {
		copts = crypt.Options{Program: ec.Program, PassphraseFile: ec.PassphraseFile}
	}
	encF, err := crypt.Filter(encrypt, copts)
	if err != nil {
		return err
	}
	compF, err := compress.Filter(codec, c.deps.CompressOpts())
	if err != nil {
		return err
	}

	src, err := c.ArchiveSource(ref, "")
	if err != nil {
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

	_, err = xfer.Transfer(src, xfer.NewFilters(filterCmds...), sink, xfer.Opts{})
	return err
}

// eachPlacement resolves the placements holding ref's slot (all copies, or only those on
// medium when set), then tries each that carries the archive — opening it via open — until
// one succeeds, so a read fails over to another copy. It is the one place the raw read paths
// share copy selection and the missing-copy errors (ErrMissingCopy).
func (c *Clerk) eachPlacement(ref Ref, medium string, open func(parts []record.FilePos, p catalog.Placement) (io.ReadCloser, error)) (io.ReadCloser, error) {
	placements := c.deps.PlacementsFor(ref.Slot)
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
