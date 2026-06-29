package librarian

import (
	"errors"
	"fmt"
	"testing"
)

// TestBlankNeedsLabelMatchesThroughReloadable locks the wrapping that lets the
// single-drive spanning loop fail fast: the blank-reel error must read as BOTH
// reloadable (so other callers still treat it as swap-eligible) AND match
// errBlankNeedsLabel via errors.Is (so advanceViaShelf stops re-prompting). It
// relies on reloadable.Unwrap; without it errors.Is cannot descend.
func TestBlankNeedsLabelMatchesThroughReloadable(t *testing.T) {
	blank := reloadable{fmt.Errorf("medium %q has a blank/unlabeled reel loaded: %w", "desk", errBlankNeedsLabel)}
	if !isReloadable(blank) {
		t.Error("blank-reel error should be reloadable")
	}
	if !errors.Is(blank, errBlankNeedsLabel) {
		t.Error("blank-reel error should match errBlankNeedsLabel via errors.Is")
	}

	// A different reloadable reason (wrong pool, still-protected, …) must NOT match
	// the sentinel — those still loop and re-prompt for another reel.
	other := reloadableErr("mounted volume belongs to the wrong pool")
	if !isReloadable(other) {
		t.Error("other reason should still be reloadable")
	}
	if errors.Is(other, errBlankNeedsLabel) {
		t.Error("a non-blank reloadable reason must not match errBlankNeedsLabel")
	}
}
