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
// inside Retention.
type Profile interface {
	// TotalBytes is the total retainable capacity (0 = unbounded). For object
	// stores this is the budget; for tape it is tapes * tape_size.
	TotalBytes() int64
	// Retention is the per-medium reclamation strategy.
	Retention() Retention
}

// Reclamation is one slot (or volume) chosen for reclamation.
type Reclamation struct {
	SlotID string
	Bytes  int64
	Note   string
}

// ReclaimPlan is the set of reclamations to perform.
type ReclaimPlan struct {
	Reclaim []Reclamation
}

// Retention decides what to reclaim to satisfy a medium's capacity, given the
// protected set (slots that must never be reclaimed, computed by policy).
type Retention interface {
	Plan(slots []*slot.Slot, protected map[string]string, now time.Time) ReclaimPlan
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

// --- size-based profile (object stores: local-disk, s3) ---

// NewSizeProfile builds a byte-budget profile from "budget".
func NewSizeProfile(opts Options) (Profile, error) {
	capacity, _ := parseBytes(opts.Get("budget"))
	return sizeProfile{capacity: capacity}, nil
}

type sizeProfile struct {
	capacity int64
}

func (p sizeProfile) TotalBytes() int64    { return p.capacity }
func (p sizeProfile) Retention() Retention { return sizeRetention{budget: p.capacity} }

// sizeRetention reclaims the oldest non-protected slots until total <= budget.
type sizeRetention struct{ budget int64 }

func (r sizeRetention) Plan(slots []*slot.Slot, protected map[string]string, now time.Time) ReclaimPlan {
	plan := ReclaimPlan{}
	if r.budget <= 0 {
		return plan // unbounded: nothing to reclaim
	}
	var total int64
	for _, s := range slots {
		total += s.TotalBytes
	}
	if total <= r.budget {
		return plan
	}
	ordered := append([]*slot.Slot(nil), slots...)
	sort.Slice(ordered, func(i, j int) bool { return slot.Less(ordered[i], ordered[j]) }) // oldest first
	for _, s := range ordered {
		if total <= r.budget {
			break
		}
		if _, isProtected := protected[s.ID]; isProtected {
			continue
		}
		plan.Reclaim = append(plan.Reclaim, Reclamation{SlotID: s.ID, Bytes: s.TotalBytes, Note: "over budget"})
		total -= s.TotalBytes
	}
	return plan
}

// --- volume-based profile (tape) — capacity known, reclamation deferred ---

// NewVolumeProfile builds a tape profile from "tapes" and "tape_size".
func NewVolumeProfile(opts Options) (Profile, error) {
	tapes, _ := strconv.ParseInt(opts.Get("tapes"), 10, 64)
	tapeSize, _ := parseBytes(opts.Get("tape_size"))
	return volumeProfile{tapes: tapes, tapeSize: tapeSize}, nil
}

type volumeProfile struct {
	tapes    int64
	tapeSize int64
}

func (p volumeProfile) TotalBytes() int64    { return p.tapes * p.tapeSize }
func (p volumeProfile) Retention() Retention { return volumeRetention{} }

// volumeRetention is a placeholder: tape reclamation is whole-volume reuse,
// which needs a volume catalog and changer (not yet implemented).
type volumeRetention struct{}

func (volumeRetention) Plan(slots []*slot.Slot, protected map[string]string, now time.Time) ReclaimPlan {
	return ReclaimPlan{}
}

func parseBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return sizeutil.ParseBytes(s)
}
