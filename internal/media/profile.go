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
	// stores this is the budget; for tape it is tapes * tape_size.
	TotalBytes() int64
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

// NewSizeProfile builds a byte-budget profile from "budget".
func NewSizeProfile(opts Options) (Profile, error) {
	capacity, _ := parseBytes(opts.Get("budget"))
	return sizeProfile{capacity: capacity}, nil
}

type sizeProfile struct {
	capacity int64
}

func (p sizeProfile) TotalBytes() int64 { return p.capacity }

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
		out = append(out, Reclamation{SlotID: s.ID, Bytes: s.TotalBytes, Note: "over budget"})
		total -= s.TotalBytes
	}
	return out
}

// --- volume-based profile (libraries of removable volumes, e.g. tape) —
// capacity known, reclamation deferred ---

// NewVolumeProfile builds a removable-volume library profile from "bays" (how
// many volumes the library holds) and "volume_size" (each volume's capacity).
func NewVolumeProfile(opts Options) (Profile, error) {
	bays, _ := strconv.ParseInt(opts.Get("bays"), 10, 64)
	volumeSize, _ := parseBytes(opts.Get("volume_size"))
	return volumeProfile{bays: bays, volumeSize: volumeSize}, nil
}

type volumeProfile struct {
	bays       int64
	volumeSize int64
}

func (p volumeProfile) TotalBytes() int64 { return p.bays * p.volumeSize }

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
