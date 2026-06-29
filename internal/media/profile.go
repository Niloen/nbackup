package media

import (
	"sort"
	"strconv"
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
	// stores, where a slot is bounded only by the pool budget). It is the basis of
	// the planner's per-run ceiling: a run fills the volume it lands on before it
	// must spill to the next, so a single run cannot exceed one reel without an
	// operator (or robot) swap. Distinct from TotalBytes because a bare drive has a
	// finite reel but an unbounded pool (the operator's shelf is unknowable).
	VolumeSize() int64
	// Reclaim chooses what to delete to satisfy this medium's capacity, given the
	// retention floor (what must never be reclaimed, computed by the retention
	// package). It returns the reclamations to perform, in deletion order. The
	// granularity is the medium's own: an object store reclaims per archive
	// (slot+DLE); a whole-volume medium (tape) reclaims nothing here. It reasons over
	// the medium's archives directly (each carrying its slot tag) — a slot is just
	// their grouping, which reclamation does not need.
	Reclaim(archives []record.Archive, keep Retention, now time.Time) []Reclamation
}

// Retention reports which archives reclamation must never delete — the floor the
// retention package computes. Reclaim consults it only as a predicate, so media
// depends on the test rather than on the retention package; retention.Floor
// satisfies it.
type Retention interface {
	KeepsArchive(slot, dle string) bool
}

// Reclamation is one archive (slot+DLE) chosen for reclamation. (A whole-volume
// medium would name a volume instead, but tape reclamation is deferred — only the
// per-archive object-store path is live.)
type Reclamation struct {
	SlotID string
	DLE    string
	Bytes  int64
	Note   string
}

// ProfileFactory constructs a Profile from generic options.
type ProfileFactory func(Options) (Profile, error)

var profileFactories = map[string]ProfileFactory{}

// RegisterProfile registers a Profile implementation under a medium type.
func RegisterProfile(typ string, f ProfileFactory) { profileFactories[typ] = f }

// OpenProfile constructs the Profile registered for the medium type.
func OpenProfile(typ string, opts Options) (Profile, error) {
	f, ok := profileFactories[typ]
	if !ok {
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

// VolumeSize is 0: object stores are not volume-structured, so a slot is bounded
// only by the pool budget, never by a per-volume reel size.
func (p sizeProfile) VolumeSize() int64 { return 0 }

// Reclaim deletes the oldest non-protected archives until total <= capacity.
// Reclamation is per archive (slot+DLE): because the retention floor is per-archive (a
// DLE's chain is independent of its slot-mates'), an old slot often holds one DLE whose
// chain has moved on — reclaimable — beside another the chain still needs. Walking
// archives oldest-first reclaims exactly the dead ones, freeing space a slot-granular
// pass would strand behind a single still-pinned DLE. Archives are ordered by their slot
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
		if ordered[i].Slot != ordered[j].Slot {
			return record.SlotIDLess(ordered[i].Slot, ordered[j].Slot) // oldest slot first
		}
		return ordered[i].DLE < ordered[j].DLE
	})
	var out []Reclamation
	for _, a := range ordered {
		if total <= p.capacity {
			return out
		}
		if keep.KeepsArchive(a.Slot, a.DLE) {
			continue
		}
		out = append(out, Reclamation{SlotID: a.Slot, DLE: a.DLE, Bytes: a.Compressed, Note: "over capacity"})
		total -= a.Compressed
	}
	return out
}

// --- volume-based profile (libraries of removable volumes, e.g. tape) —
// capacity known, reclamation deferred ---

// NewVolumeProfile builds a removable-volume profile from "volume_size" (each
// reel's capacity) and the volume count, reading the same keys the changer does
// so the two never disagree: a robotic library counts "bays", a manual
// ShelfStation (mode: manual) counts "reels" in the offline room, and a bare
// drive ("device") has an unbounded pool — the operator can load any number of
// reels by hand, so only the per-run reel ceiling (VolumeSize) is finite.
func NewVolumeProfile(opts Options) (Profile, error) {
	volumeSize, _ := parseBytes(opts.Get("volume_size"))
	return volumeProfile{volumes: volumeCount(opts), volumeSize: volumeSize}, nil
}

// volumeCount reads the retainable reel count from the same option key the changer
// keys on, so the planner's pool capacity can never disagree with the medium it
// lands on: a manual station (mode: manual) counts "reels", a robotic library counts
// "bays", and a bare drive ("device") has an unbounded pool (0). This mirrors the
// tape factory's key choice by convention — they read the same keys for the same shapes.
func volumeCount(opts Options) int64 {
	switch {
	case opts.Get("device") != "":
		return 0 // bare drive: pool unbounded, only the reel is finite
	case opts.Get("mode") == "manual":
		return countOpt(opts.Get("reels"))
	default:
		return countOpt(opts.Get("bays"))
	}
}

// countOpt parses a volume count, defaulting to 1 (a medium always has at least
// its one loaded volume), matching the changer's own atoiOpt(..., 1) default.
func countOpt(s string) int64 {
	if s == "" {
		return 1
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

type volumeProfile struct {
	volumes    int64 // count of retainable reels; 0 = unbounded (bare drive)
	volumeSize int64
}

func (p volumeProfile) TotalBytes() int64 { return p.volumes * p.volumeSize }

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
