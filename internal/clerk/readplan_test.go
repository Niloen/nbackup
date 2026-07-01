package clerk

import (
	"testing"

	"github.com/Niloen/nbackup/internal/record"
)

func item(run, dle string, level int, medium, label string, pos int) ReadItem {
	return ReadItem{
		Ref:      Ref{Run: run, DLE: dle, Level: level},
		Medium:   medium,
		FirstPos: record.FilePos{Label: label, Pos: pos},
	}
}

func order(items []ReadItem) []string {
	out := OrderForOnePass(items)
	keys := make([]string, len(out))
	for i, it := range out {
		keys[i] = it.Ref.DLE + "/L" + string(rune('0'+it.Ref.Level))
	}
	return keys
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// Independent archives (distinct DLEs) read forward by physical position, not input order.
func TestOrderIndependentByPosition(t *testing.T) {
	got := order([]ReadItem{
		item("s1", "c", 0, "disk", "", 300),
		item("s1", "a", 0, "disk", "", 100),
		item("s1", "b", 0, "disk", "", 200),
	})
	eq(t, got, []string{"a/L0", "b/L0", "c/L0"})
}

// A chain (one DLE, several levels) is always emitted L0→L1→L2, even when later levels sit
// physically earlier on the medium.
func TestOrderChainKeepsLevelOrder(t *testing.T) {
	got := order([]ReadItem{
		item("s3", "x", 2, "disk", "", 100),
		item("s1", "x", 0, "disk", "", 900),
		item("s2", "x", 1, "disk", "", 500),
	})
	eq(t, got, []string{"x/L0", "x/L1", "x/L2"})
}

// Medium is the primary ordering key: a copy on medium "a" reads before one on "b" regardless
// of byte position, so each medium's reads stay together.
func TestOrderGroupsByMedium(t *testing.T) {
	got := order([]ReadItem{
		item("s1", "p", 0, "b", "", 0),
		item("s1", "q", 0, "a", "", 999),
	})
	eq(t, got, []string{"q/L0", "p/L0"})
}

// Mixed chains and independents: each DLE stays in level order, and among the ready items the
// physically-earliest wins. Here x's chain and the independent y interleave by position while
// x's levels never invert.
func TestOrderMixedChainAndIndependent(t *testing.T) {
	got := order([]ReadItem{
		item("s2", "x", 1, "disk", "", 400), // x.L1 physically after y but after x.L0 logically
		item("s1", "x", 0, "disk", "", 100),
		item("s1", "y", 0, "disk", "", 200),
	})
	// x.L0@100 first (earliest ready); then among {y@200, x.L1@400} y is earlier; then x.L1.
	eq(t, got, []string{"x/L0", "y/L0", "x/L1"})
}

// Adversarial cross-volume interleaving: two chains whose levels sit on opposite volumes.
// Level order must hold for both even though no single forward pass satisfies both.
func TestOrderAdversarialInterleaveKeepsLevelOrder(t *testing.T) {
	out := OrderForOnePass([]ReadItem{
		item("s1", "x", 0, "tape", "v1", 10),
		item("s2", "x", 1, "tape", "v2", 10),
		item("s1", "y", 0, "tape", "v2", 20),
		item("s2", "y", 1, "tape", "v1", 20),
	})
	// Assert per-DLE level monotonicity (the guarantee), not a specific interleaving.
	last := map[string]int{}
	for _, it := range out {
		if l, ok := last[it.Ref.DLE]; ok && it.Ref.Level <= l {
			t.Fatalf("DLE %s emitted L%d after L%d — level order violated: %v", it.Ref.DLE, it.Ref.Level, l, out)
		}
		last[it.Ref.DLE] = it.Ref.Level
	}
	if len(out) != 4 {
		t.Fatalf("dropped items: %v", out)
	}
}
