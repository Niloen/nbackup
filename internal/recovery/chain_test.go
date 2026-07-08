package recovery

import (
	"testing"

	"github.com/Niloen/nbackup/internal/record"
)

// arch builds an archive for dleName at a level, recording the base run an
// incremental derives from (BaseRun is empty for a full).
func arch(dle string, level int, base string) record.Archive {
	return record.Archive{DLE: dle, Level: level, Archiver: "gnutar", Compress: "none", BaseRun: base}
}

// run tags each archive with the run id and returns them (Chain works on archives,
// each carrying its run tag).
func run(id string, archives ...record.Archive) []record.Archive {
	for i := range archives {
		archives[i].Run = id
	}
	return archives
}

// cat flattens several runs' archives into the one corpus Chain takes.
func cat(runs ...[]record.Archive) []record.Archive {
	var out []record.Archive
	for _, s := range runs {
		out = append(out, s...)
	}
	return out
}

// levels returns the per-step level sequence of a chain, for compact assertions.
func levels(steps []Step) []int {
	out := make([]int, len(steps))
	for i, s := range steps {
		out[i] = s.Level
	}
	return out
}

func runIDs(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.RunID
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
	runs := cat(run("s0", arch("a", 0, "")))
	steps, err := Chain(runs, "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(runIDs(steps), []string{"s0"}) {
		t.Fatalf("chain = %v, want [s0]", runIDs(steps))
	}
}

// The regression at the heart of the dir-rename bug: consecutive same-level
// incrementals are cumulative since the full, so only the newest L1 is replayed
// and the redundant middle one is skipped.
func TestChainSkipsRedundantSameLevel(t *testing.T) {
	runs := cat(
		run("s0", arch("a", 0, "")),
		run("s1", arch("a", 1, "s0")),
		run("s2", arch("a", 1, "s0")),
	)
	steps, err := Chain(runs, "a", "s2")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(runIDs(steps), []string{"s0", "s2"}) {
		t.Fatalf("chain = %v, want [s0 s2] (redundant s1 skipped)", runIDs(steps))
	}
}

// A real multilevel chain replays exactly one archive per level, in run order.
func TestChainOnePerLevel(t *testing.T) {
	runs := cat(
		run("s0", arch("a", 0, "")),
		run("s1", arch("a", 1, "s0")),
		run("s2", arch("a", 1, "s0")),
		run("s3", arch("a", 2, "s2")),
		run("s4", arch("a", 2, "s2")),
	)
	steps, err := Chain(runs, "a", "s4")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(runIDs(steps), []string{"s0", "s2", "s4"}) {
		t.Fatalf("chain = %v, want [s0 s2 s4]", runIDs(steps))
	}
	if !eq(levels(steps), []int{0, 1, 2}) {
		t.Fatalf("levels = %v, want [0 1 2]", levels(steps))
	}
}

// A target before the tip restores the point-in-time subchain.
func TestChainPointInTime(t *testing.T) {
	runs := cat(
		run("s0", arch("a", 0, "")),
		run("s1", arch("a", 1, "s0")),
		run("s2", arch("a", 1, "s0")),
	)
	steps, err := Chain(runs, "a", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(runIDs(steps), []string{"s0", "s1"}) {
		t.Fatalf("chain = %v, want [s0 s1]", runIDs(steps))
	}
}

// When BaseRun was never recorded, the base is derived from level ordering.
func TestChainDerivesBaseWithoutBaseRun(t *testing.T) {
	runs := cat(
		run("s0", arch("a", 0, "")),
		run("s1", arch("a", 1, "")),
		run("s2", arch("a", 2, "")),
	)
	steps, err := Chain(runs, "a", "s2")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(runIDs(steps), []string{"s0", "s1", "s2"}) {
		t.Fatalf("chain = %v, want [s0 s1 s2]", runIDs(steps))
	}
}

// A recorded base that is no longer in the catalog is a broken chain, not a
// silent substitution — the restore fails rather than producing a partial tree.
func TestChainBrokenWhenBaseRunMissing(t *testing.T) {
	runs := cat(
		run("s0", arch("a", 0, "")),
		run("s2", arch("a", 1, "s1")), // s1 pruned away
	)
	if _, err := Chain(runs, "a", "s2"); err == nil {
		t.Fatal("expected broken-chain error when BaseRun is missing")
	}
}

func TestChainNoBackupForDLE(t *testing.T) {
	runs := cat(run("s0", arch("a", 0, "")))
	if _, err := Chain(runs, "b", "s0"); err == nil {
		t.Fatal("expected error for a DLE with no backup")
	}
}

func TestChainUnknownTarget(t *testing.T) {
	runs := cat(run("s0", arch("a", 0, "")))
	if _, err := Chain(runs, "a", "nope"); err == nil {
		t.Fatal("expected error for an unknown target run")
	}
}

// Multiple DLEs in shared runs: each DLE's chain is independent.
func TestChainIgnoresOtherDLEs(t *testing.T) {
	runs := cat(
		run("s0", arch("a", 0, ""), arch("b", 0, "")),
		run("s1", arch("b", 1, "s0")),
		run("s2", arch("a", 1, "s0")),
	)
	steps, err := Chain(runs, "a", "s2")
	if err != nil {
		t.Fatal(err)
	}
	if !eq(runIDs(steps), []string{"s0", "s2"}) {
		t.Fatalf("chain = %v, want [s0 s2]", runIDs(steps))
	}
}

// Stranded finds every unrestorable incremental: one whose chain to a full is
// broken, judged transitively (an L2 whose L1 survives is still stranded when
// the L0 under them is gone). Fulls and intact chains never appear.
func TestStranded(t *testing.T) {
	t.Run("intact chains report nothing", func(t *testing.T) {
		runs := cat(
			run("s0", arch("a", 0, "")),
			run("s1", arch("a", 1, "s0")),
		)
		if got := Stranded(runs); len(got) != 0 {
			t.Fatalf("Stranded = %+v, want none", got)
		}
	})

	t.Run("missing base strands the incremental, not the sibling full", func(t *testing.T) {
		runs := cat(
			// s0 (the L0 that s1 builds on) was pruned away; s2 is a fresh full.
			run("s1", arch("a", 1, "s0")),
			run("s2", arch("a", 0, "")),
		)
		got := Stranded(runs)
		if len(got) != 1 || got[0].Archive.Run != "s1" || got[0].Err == nil {
			t.Fatalf("Stranded = %+v, want exactly s1 with its chain error", got)
		}
	})

	t.Run("a missing full strands the whole chain above it", func(t *testing.T) {
		runs := cat(
			run("s1", arch("a", 1, "s0")), // base full s0 gone
			run("s2", arch("a", 2, "s1")), // L1 present, but its own chain is broken
		)
		got := Stranded(runs)
		if len(got) != 2 {
			t.Fatalf("Stranded = %+v, want s1 and s2", got)
		}
	})
}
