package restore

import (
	"testing"

	"github.com/Niloen/nbackup/internal/record"
)

// arch builds an archive for dleName at a level, recording the base slot an
// incremental derives from (BaseSlot is empty for a full).
func arch(dle string, level int, base string) record.Archive {
	return record.Archive{DLE: dle, Level: level, Archiver: "gnutar", Compress: "none", BaseSlot: base}
}

func slot(id string, archives ...record.Archive) *record.Slot {
	return &record.Slot{ID: id, Status: record.StatusSealed, Archives: archives}
}

// levels returns the per-step level sequence of a chain, for compact assertions.
func levels(steps []Step) []int {
	out := make([]int, len(steps))
	for i, s := range steps {
		out[i] = s.Level
	}
	return out
}

func slotIDs(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.SlotID
	}
	return out
}

func eq[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChainFullOnly(t *testing.T) {
	slots := []*record.Slot{slot("s0", arch("a", 0, ""))}
	steps, err := Chain(slots, "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(slotIDs(steps), []string{"s0"}) {
		t.Fatalf("chain = %v, want [s0]", slotIDs(steps))
	}
}

// The regression at the heart of the dir-rename bug: consecutive same-level
// incrementals are cumulative since the full, so only the newest L1 is replayed
// and the redundant middle one is skipped.
func TestChainSkipsRedundantSameLevel(t *testing.T) {
	slots := []*record.Slot{
		slot("s0", arch("a", 0, "")),
		slot("s1", arch("a", 1, "s0")),
		slot("s2", arch("a", 1, "s0")),
	}
	steps, err := Chain(slots, "a", "s2")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(slotIDs(steps), []string{"s0", "s2"}) {
		t.Fatalf("chain = %v, want [s0 s2] (redundant s1 skipped)", slotIDs(steps))
	}
}

// A real multilevel chain replays exactly one archive per level, in run order.
func TestChainOnePerLevel(t *testing.T) {
	slots := []*record.Slot{
		slot("s0", arch("a", 0, "")),
		slot("s1", arch("a", 1, "s0")),
		slot("s2", arch("a", 1, "s0")),
		slot("s3", arch("a", 2, "s2")),
		slot("s4", arch("a", 2, "s2")),
	}
	steps, err := Chain(slots, "a", "s4")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(slotIDs(steps), []string{"s0", "s2", "s4"}) {
		t.Fatalf("chain = %v, want [s0 s2 s4]", slotIDs(steps))
	}
	if !eq(levels(steps), []int{0, 1, 2}) {
		t.Fatalf("levels = %v, want [0 1 2]", levels(steps))
	}
}

// A target before the tip restores the point-in-time subchain.
func TestChainPointInTime(t *testing.T) {
	slots := []*record.Slot{
		slot("s0", arch("a", 0, "")),
		slot("s1", arch("a", 1, "s0")),
		slot("s2", arch("a", 1, "s0")),
	}
	steps, err := Chain(slots, "a", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(slotIDs(steps), []string{"s0", "s1"}) {
		t.Fatalf("chain = %v, want [s0 s1]", slotIDs(steps))
	}
}

// When BaseSlot was never recorded, the base is derived from level ordering.
func TestChainDerivesBaseWithoutBaseSlot(t *testing.T) {
	slots := []*record.Slot{
		slot("s0", arch("a", 0, "")),
		slot("s1", arch("a", 1, "")),
		slot("s2", arch("a", 2, "")),
	}
	steps, err := Chain(slots, "a", "s2")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(slotIDs(steps), []string{"s0", "s1", "s2"}) {
		t.Fatalf("chain = %v, want [s0 s1 s2]", slotIDs(steps))
	}
}

// A recorded base that is no longer in the catalog is a broken chain, not a
// silent substitution — the restore fails rather than producing a partial tree.
func TestChainBrokenWhenBaseSlotMissing(t *testing.T) {
	slots := []*record.Slot{
		slot("s0", arch("a", 0, "")),
		slot("s2", arch("a", 1, "s1")), // s1 pruned away
	}
	if _, err := Chain(slots, "a", "s2"); err == nil {
		t.Fatal("expected broken-chain error when BaseSlot is missing")
	}
}

func TestChainNoBackupForDLE(t *testing.T) {
	slots := []*record.Slot{slot("s0", arch("a", 0, ""))}
	if _, err := Chain(slots, "b", "s0"); err == nil {
		t.Fatal("expected error for a DLE with no backup")
	}
}

func TestChainUnknownTarget(t *testing.T) {
	slots := []*record.Slot{slot("s0", arch("a", 0, ""))}
	if _, err := Chain(slots, "a", "nope"); err == nil {
		t.Fatal("expected error for an unknown target slot")
	}
}

// Multiple DLEs in shared slots: each DLE's chain is independent.
func TestChainIgnoresOtherDLEs(t *testing.T) {
	slots := []*record.Slot{
		slot("s0", arch("a", 0, ""), arch("b", 0, "")),
		slot("s1", arch("b", 1, "s0")),
		slot("s2", arch("a", 1, "s0")),
	}
	steps, err := Chain(slots, "a", "s2")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(slotIDs(steps), []string{"s0", "s2"}) {
		t.Fatalf("chain = %v, want [s0 s2]", slotIDs(steps))
	}
}
