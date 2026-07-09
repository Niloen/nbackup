package cli

import (
	"strings"
	"testing"
)

// TestLiveName checks the live frame's DLE-name cap: short names pass through,
// long ones keep the host prefix and the path's tail with the cut marked by a
// leading "-" (amreport's convention — the leaf is what tells siblings apart).
func TestLiveName(t *testing.T) {
	short := "app01:/data/projects/alpha"
	if got := liveName(short); got != short {
		t.Errorf("short name changed: %q", got)
	}

	long := "app01:/data/very/deep/directory/tree/archived-financials-fy2019-eu-region"
	got := liveName(long)
	if len(got) > liveNameMax {
		t.Errorf("len(%q) = %d, want <= %d", got, len(got), liveNameMax)
	}
	if !strings.HasPrefix(got, "app01:-") {
		t.Errorf("truncated name must keep the host and mark the cut: %q", got)
	}
	if !strings.HasSuffix(got, "archived-financials-fy2019-eu-region") {
		t.Errorf("truncated name must keep the path tail: %q", got)
	}

	// A hostless name (bare slug) still caps with the tail kept.
	slug := strings.Repeat("x", 40) + "-tail-of-the-slug"
	if got := liveName(slug); len(got) > liveNameMax || !strings.HasSuffix(got, "tail-of-the-slug") || got[0] != '-' {
		t.Errorf("hostless truncation = %q", got)
	}
}
