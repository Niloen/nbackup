package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/planner"
)

// TestPlanItemsRenderGroups pins the plan table's pattern-group rendering: a plain DLE
// stays one plain row; a partition renders a group header, indented child rows, a
// "the rest" row, and the coverage line; a selection says "no rest" and gets NO coverage
// line — the visible cue that only the matches are covered.
func TestPlanItemsRenderGroups(t *testing.T) {
	mk := func(base, source, host string, level int) planner.Item {
		return planner.Item{
			DLE:      planner.DLE{Scope: archiver.Scope{Base: base, Source: source}, Host: host},
			Name:     "n-" + source,
			Level:    level,
			EstBytes: 1 << 20,
		}
	}
	plan := &planner.Plan{Date: time.Now(), Items: []planner.Item{
		mk("", "/var/log", "fs", 0),         // plain
		mk("/data", "/data/alice", "fs", 0), // partition group…
		mk("/data", "/data/bob", "fs", 1),
		mk("/data", "/data", "fs", 1),       // …the rest (Source == Base)
		mk("/srv", "/srv/web-app", "fs", 0), // selection group (no rest)
		mk("/srv", "/srv/web-api", "fs", 0),
	}}

	var sb strings.Builder
	fprintPlanItems(&sb, plan)
	out := sb.String()

	for _, want := range []string{
		"fs:/var/log",            // plain row keeps the full ID
		"fs:/data — partitioned", // partition header
		"├─ alice",               // child, short name
		"└─ the rest",            // rest labeled, last in group
		"✓ covers 100% of fs:/data (2 matched + the rest)", // coverage line
		"fs:/srv — selection (matches only, no rest)",      // selection header
		"└─ web-api", // selection's last child
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan table missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "covers 100% of fs:/srv") {
		t.Errorf("a selection must not claim coverage:\n%s", out)
	}
}
