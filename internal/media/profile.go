package media

import (
	"sort"
	"strconv"
	"time"

	"github.com/Niloen/nbackup/internal/sizeutil"
	"github.com/Niloen/nbackup/internal/slot"
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
	// Reclaim chooses the slots to delete to satisfy this medium's capacity,
	// given the protected set (slots that must never be reclaimed, computed by
	// policy). It returns the reclamations to perform, in deletion order.
	Reclaim(slots []*slot.Slot, protected map[string]string, now time.Time) []Reclamation
}

// Reclamation is one slot (or volume) chosen for reclamation.
type Reclamation struct {
	SlotID string
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

// Reclaim deletes the oldest non-protected slots until total <= capacity.
func (p sizeProfile) Reclaim(slots []*slot.Slot, protected map[string]string, now time.Time) []Reclamation {
	if p.capacity <= 0 {
		return nil // unbounded: nothing to reclaim
	}
	var total int64
	for _, s := range slots {
		total += s.TotalBytes
	}
	if total <= p.capacity {
		return nil
	}
	ordered := append([]*slot.Slot(nil), slots...)
	sort.Slice(ordered, func(i, j int) bool { return slot.Less(ordered[i], ordered[j]) }) // oldest first
	var out []Reclamation
	for _, s := range ordered {
		if total <= p.capacity {
			break
		}
		if _, isProtected := protected[s.ID]; isProtected {
			continue
		}
		out = append(out, Reclamation{SlotID: s.ID, Bytes: s.TotalBytes, Note: "over capacity"})
		total -= s.TotalBytes
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
	var volumes int64
	switch {
	case opts.Get("device") != "":
		volumes = 0 // bare drive: pool unbounded, only the reel is finite
	case opts.Get("mode") == "manual":
		volumes = countOpt(opts.Get("reels"))
	default:
		volumes = countOpt(opts.Get("bays"))
	}
	return volumeProfile{volumes: volumes, volumeSize: volumeSize}, nil
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

// Reclaim is a placeholder: tape reclamation is whole-volume reuse, which needs
// a volume catalog and changer (not yet implemented).
func (p volumeProfile) Reclaim(slots []*slot.Slot, protected map[string]string, now time.Time) []Reclamation {
	return nil
}

func parseBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return sizeutil.ParseBytes(s)
}
