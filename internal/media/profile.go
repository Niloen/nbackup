package media

import (
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// Profile describes a medium's capacity and reclamation strategy, translated
// from its native config into the common currency pruning uses. Capacity is the
// only genuinely per-medium quantity; balancing dumps over time is a global
// planning concern, not a property of where bytes land, so it lives in the
// planner, not here. The reclamation granularity (object vs volume) is hidden
// behind Reclaim.
type Profile interface {
	// TotalBytes is the total retainable capacity (0 = unbounded). For object
	// stores this is the capacity; for a tape library it is volumes * volume_size.
	// It governs reclamation and the planner's structural cycle check (can a
	// complete recovery set be retained at all).
	TotalBytes() int64
	// VolumeSize is the physical capacity of a single volume for striped media —
	// one tape reel. It is 0 for media that are not volume-structured (object
	// stores, where a run is bounded only by the pool budget). It is the basis of
	// the planner's per-run ceiling: a run fills the volume it lands on before it
	// must spill to the next, so a single run cannot exceed one reel without an
	// operator (or robot) swap. Distinct from TotalBytes because a bare drive has a
	// finite reel but an unbounded pool (the operator's shelf is unknowable).
	VolumeSize() int64
	// Reclaim chooses what to delete to satisfy this medium's capacity, given the
	// retention floor (what must never be reclaimed, computed by the retention
	// package). It returns the reclamations to perform, in deletion order. The
	// granularity is the medium's own: an object store reclaims per archive
	// (run+DLE); a whole-volume medium (tape) reclaims nothing here. It reasons over
	// the medium's archives directly (each carrying its run tag) — a run is just
	// their grouping, which reclamation does not need.
	Reclaim(archives []record.Archive, keep Retention, now time.Time) []Reclamation
}

// Retention reports which archives reclamation must never delete — the floor the
// retention package computes. Reclaim consults it only as a predicate, so media
// depends on the test rather than on the retention package; retention.Floor
// satisfies it.
type Retention interface {
	KeepsArchive(run, dle string) bool
}

// Reclamation is one archive (run+DLE) chosen for reclamation. (A whole-volume
// medium would name a volume instead, but tape reclamation is deferred — only the
// per-archive object-store path is live.)
type Reclamation struct {
	RunID string
	DLE   string
	Bytes int64
	Note  string
}

// ProfileFactory constructs a Profile from generic options.
type ProfileFactory func(Options) (Profile, error)

// OpenProfile constructs the Profile registered for the medium type.
func OpenProfile(typ string, opts Options) (Profile, error) {
	f := specs[typ].Profile
	if f == nil {
		// A medium without a registered profile is treated as unbounded.
		return sizeProfile{}, nil
	}
	return f(opts)
}

// --- size-based profile (object stores: disk, s3) ---

// NewSizeProfile builds a byte-capacity profile from "capacity".
func NewSizeProfile(opts Options) (Profile, error) {
	capacity, _ := parseBytes(opts.Get("capacity"))
	return sizeProfile{capacity: capacity}, nil
}

type sizeProfile struct {
	capacity int64
}

func (p sizeProfile) TotalBytes() int64 { return p.capacity }

// VolumeSize is 0: object stores are not volume-structured, so a run is bounded
// only by the pool budget, never by a per-volume reel size.
func (p sizeProfile) VolumeSize() int64 { return 0 }

// Reclaim deletes the oldest non-protected archives until total <= capacity.
// Reclamation is per archive (run+DLE): because the retention floor is per-archive (a
// DLE's chain is independent of its run-mates'), an old run often holds one DLE whose
// chain has moved on — reclaimable — beside another the chain still needs. Walking
// archives oldest-first reclaims exactly the dead ones, freeing space a run-granular
// pass would strand behind a single still-pinned DLE. Archives are ordered by their run
// (run order, oldest first) then DLE for a deterministic plan.
func (p sizeProfile) Reclaim(archives []record.Archive, keep Retention, now time.Time) []Reclamation {
	if p.capacity <= 0 {
		return nil // unbounded: nothing to reclaim
	}
	var total int64
	for _, a := range archives {
		total += a.Compressed
	}
	if total <= p.capacity {
		return nil
	}
	ordered := append([]record.Archive(nil), archives...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Run != ordered[j].Run {
			return record.RunIDLess(ordered[i].Run, ordered[j].Run) // oldest run first
		}
		return ordered[i].DLE < ordered[j].DLE
	})
	var out []Reclamation
	for _, a := range ordered {
		if total <= p.capacity {
			return out
		}
		if keep.KeepsArchive(a.Run, a.DLE) {
			continue
		}
		out = append(out, Reclamation{RunID: a.Run, DLE: a.DLE, Bytes: a.Compressed, Note: "over capacity"})
		total -= a.Compressed
	}
	return out
}

// --- volume-based profile (libraries of removable volumes, e.g. tape) —
// capacity known, reclamation deferred ---

// NewVolumeProfile builds a removable-volume profile: volumes retainable
// cartridges (0 = an unbounded pool, e.g. a hand-loaded drive whose shelf is
// unknowable), each of volumeSize bytes (the per-run reel ceiling). How those two
// numbers fall out of a medium's config is the medium's business — the tape
// package derives them from its own option keys when it registers.
func NewVolumeProfile(volumes, volumeSize int64) Profile {
	return volumeProfile{volumes: volumes, volumeSize: volumeSize}
}

type volumeProfile struct {
	volumes    int64 // count of retainable reels; 0 = unbounded (bare drive)
	volumeSize int64
}

// volumeFramingOverhead is the bytes each reel spends on framing rather than payload,
// so the planner's usable capacity is not overstated: a 32 KB identity label (file 0)
// plus at least one 32 KB inline part header for the archive that lands on the reel. It
// is negligible at real cartridge sizes (64 KB of a 6 TB LTO) but decisive for a tiny
// file-backed sim, where ignoring it let `nb plan` report "fits, 0% used" for a run that
// then filled every reel and failed mid-dump.
const volumeFramingOverhead = 2 * record.HeaderBlock

// usableVolumeBytes is a reel's payload capacity net of framing (never negative — a reel
// smaller than its own framing holds nothing usable).
func (p volumeProfile) usableVolumeBytes() int64 {
	if p.volumeSize <= volumeFramingOverhead {
		return 0
	}
	return p.volumeSize - volumeFramingOverhead
}

func (p volumeProfile) TotalBytes() int64 { return p.volumes * p.usableVolumeBytes() }

func (p volumeProfile) VolumeSize() int64 { return p.volumeSize }

// Reclaim is intentionally a no-op: tape space is not reclaimed by a prune pass.
// Whole-volume reuse is label rotation done on the *write* path — when a run needs a
// fresh volume the librarian recycles the oldest tape the retention Floor clears
// (`librarian.Advance` / `acceptOrRecycle`), keeping the same label and advancing its
// epoch. Tape capacity is structural (the depth of the label pool is the retention), so
// there is nothing for a capacity-driven prune to delete here.
func (p volumeProfile) Reclaim(archives []record.Archive, keep Retention, now time.Time) []Reclamation {
	return nil
}

func parseBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return sizeutil.ParseBytes(s)
}
