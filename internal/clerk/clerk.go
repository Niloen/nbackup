// Package clerk is NBackup's archive filesystem — the medium layer that turns a logical
// archive ref into a byte endpoint and back, and nothing more. It is like the
// FS's open(): it owns the archive map (resolve a copy's positions on
// read, record a run's placement on write — the Map role), the member index, and read-mounting
// via the librarian (the Mounter role). It speaks only io.* — plain byte endpoints, never a
// transfer — and exposes endpoints, not operations:
//
//   - read:  Open → an io.ReadCloser over one archive's raw on-medium bytes (copy-selected);
//     ReadArchives → the same, but the clerk drives an ordered one-pass read and calls back
//     per archive.
//   - write: a Session over a slot writer takes an io.Reader of already-encoded bytes
//     (WriteArchive for a dump, CopyArchive for a copy), then Commit (footer + index) and
//     Finish (placement).
//   - Members(ref) → the archive's member list (cache → on-medium index).
//
// What it deliberately does NOT do: schemes, tar, or composing transfers. The decode/encode and
// the far-end tar live in the *operations* (the Dumper, Restorer, Verifier, …), which wrap a
// clerk endpoint in an xfer.Transfer — exactly as cp/gzip compose over a filesystem's open().
// So the clerk knows nothing of
// xfer, config, archivers, compress/encrypt, or the librarian package; its only deps are the
// Map, the Mounter, a bandwidth Limiter, and its own member-index cache.
package clerk

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
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

// Map is the clerk's slice of the catalog — the archive map (the inode/extent table). The
// clerk resolves it on read (which copies of a slot, and where each archive's parts live) and
// records it on write (where a run's archives landed). It is the one role through which the
// clerk owns map I/O; the engine implements it (and keeps the directory/retention/volume
// slices). PlacementsFor returns copies in read-preference order (the engine's own first).
type Map interface {
	PlacementsFor(slotID string) []catalog.Placement
	// AddArchive records one committed archive's content + its on-medium position — the
	// catalog's single write path (a slot is created from the archive's identity, never added
	// wholesale). SealSlot stamps the slot sealed once its run finishes.
	AddArchive(slot *record.Slot, medium string, arch record.Archive, pos record.ArchivePos) error
	SealSlot(id string, now time.Time) error
}

// Mounter is the clerk's data-path slice of the librarian (the volume manager): mount the
// volume a part names and read it. The clerk owns this read-mount role; the librarian's
// admin/operator face (label, load, inventory) stays with the engine and the label/load
// operations. The clerk depends on the role, not the librarian package.
type Mounter interface {
	ReadFileAt(volume string, epoch, pos int) (record.Header, io.ReadCloser, error)
}

// Deps is the rest of what the data path needs from the orchestrator beside the map: just the
// medium's volume mount and its bandwidth cap. (The transforms, the tar endpoints, and the
// archiver/executor resolution all moved up to the operations.) The engine implements it; this
// interface is the explicit boundary between deciding and doing.
type Deps interface {
	// MounterFor returns a read-mount onto a medium's volumes.
	MounterFor(medium string) (Mounter, error)
	// Limiter returns the medium's shared bandwidth cap (nil = uncapped).
	Limiter(medium string) *ratelimit.Limiter
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
	cat    Map
	reader *archiveio.Reader
	mindex *catalog.MemberIndex
}

// New returns a data path over the archive map and the orchestrator's services, using mindex
// as the server-side member cache.
func New(cat Map, deps Deps, mindex *catalog.MemberIndex) *Clerk {
	return &Clerk{deps: deps, cat: cat, reader: archiveio.NewReader(), mindex: mindex}
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
	for _, p := range c.cat.PlacementsFor(ref.Slot) {
		pos, ok := indexPosOf(p, ref.DLE, ref.Level)
		if !ok {
			continue
		}
		members, err := c.readIndex(p.Medium, pos)
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

// Open opens an archive's raw (undecoded, on-medium) part stream as an io.ReadCloser, with
// copy selection and fail-over: medium "" tries every copy (preferring the engine's own), a set
// medium reads only that copy so a fault on it is not masked by another. The open (and thus the
// copy-selection fail-over) happens eagerly, so a missing copy is reported before bytes flow.
// The caller wraps it for a transfer (xfer.Reader); the clerk only hands back bytes.
func (c *Clerk) Open(ref Ref, medium string) (io.ReadCloser, error) {
	return c.openRaw(ref, medium)
}

// openRaw opens an archive's raw on-medium part stream with copy selection and fail-over.
func (c *Clerk) openRaw(ref Ref, medium string) (io.ReadCloser, error) {
	return c.eachPlacement(ref, medium, func(parts []record.FilePos, p catalog.Placement) (io.ReadCloser, error) {
		opener, err := c.partOpener(p.Medium)
		if err != nil {
			return nil, err
		}
		return c.reader.Open(parts, ref.expect(), opener)
	})
}

// partOpener returns a mounting opener for a medium's volumes (rate-limited by its shared
// cap), for callers that drive the read loop themselves — threading one opener across all of
// a copy's archives (verify, copy).
func (c *Clerk) partOpener(medium string) (archiveio.PartOpener, error) {
	mounter, err := c.deps.MounterFor(medium)
	if err != nil {
		return nil, err
	}
	lim := c.deps.Limiter(medium)
	return func(p record.FilePos) (record.Header, io.ReadCloser, error) {
		h, rc, err := mounter.ReadFileAt(p.Label, p.Epoch, p.Pos)
		if err != nil {
			return h, rc, err
		}
		return h, lim.ReadCloser(rc), nil
	}, nil
}

// readIndex reads an archive's member index off a medium — the lazy fallback when the
// server-side member cache misses (a rebuilt slot not yet browsed). It mounts the volume the
// index lives on and decodes it.
func (c *Clerk) readIndex(medium string, pos record.FilePos) ([]string, error) {
	mounter, err := c.deps.MounterFor(medium)
	if err != nil {
		return nil, err
	}
	_, rc, err := mounter.ReadFileAt(pos.Label, pos.Epoch, pos.Pos)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return record.DecodeIndex(rc)
}

// eachPlacement resolves the placements holding ref's slot (all copies, or only those on
// medium when set), then tries each that carries the archive — opening it via open — until
// one succeeds, so a read fails over to another copy. It is the one place the raw read paths
// share copy selection and the missing-copy errors (ErrMissingCopy).
func (c *Clerk) eachPlacement(ref Ref, medium string, open func(parts []record.FilePos, p catalog.Placement) (io.ReadCloser, error)) (io.ReadCloser, error) {
	placements := c.cat.PlacementsFor(ref.Slot)
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
