package progress

import (
	"strings"
	"testing"
)

func TestMultiSinkFansOut(t *testing.T) {
	var a, b int
	sink := MultiSink(
		func(Snapshot, bool) { a++ },
		nil, // a nil sink is skipped, not a panic
		func(Snapshot, bool) { b++ },
	)
	sink(Snapshot{}, true)
	sink(Snapshot{}, false)
	if a != 2 || b != 2 {
		t.Fatalf("want both sinks called twice, got a=%d b=%d", a, b)
	}
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		want  string
	}{
		{"hello", 10, "hello"}, // shorter than width: unchanged
		{"hello", 5, "hello"},  // exactly width: unchanged
		{"hello", 4, "hel…"},   // clipped: width-1 runes plus ellipsis
		{"héllo", 4, "hél…"},   // rune-aware: é counts as one, not two bytes
		{"hello", 1, "…"},      // width 1: bare ellipsis
		{"hello", 0, ""},       // nonpositive width: empty
	} {
		if got := truncate(tc.in, tc.width); got != tc.want {
			t.Errorf("truncate(%q,%d)=%q want %q", tc.in, tc.width, got, tc.want)
		}
	}
}

// TestLiveSinkPaintsAndErases drives a snapshot through running -> terminal and
// checks the sink paints a frame, rewinds over it on repaint, and erases the region
// when the run ends so the caller's output starts clean.
func TestLiveSinkPaintsAndErases(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	var buf strings.Builder
	lines := func(s Snapshot) []string {
		return []string{"head", "  row " + s.SlotID}
	}
	sink := LiveSink(&buf, lines)

	sink(Snapshot{SlotID: "a", Phase: PhaseRunning}, true)
	out := buf.String()
	if !strings.Contains(out, "head") || !strings.Contains(out, "row a") {
		t.Fatalf("first frame missing content: %q", out)
	}
	if !strings.Contains(out, "\033[J") {
		t.Fatalf("first frame should clear to end of screen: %q", out)
	}

	buf.Reset()
	sink(Snapshot{SlotID: "b", Phase: PhaseRunning}, true)
	out = buf.String()
	if !strings.Contains(out, "\033[1A") { // rewind one row above the 2-line frame
		t.Fatalf("repaint should rewind over previous frame: %q", out)
	}
	if !strings.Contains(out, "row b") {
		t.Fatalf("repaint missing new content: %q", out)
	}

	buf.Reset()
	sink(Snapshot{SlotID: "b", Phase: PhaseDone}, true)
	out = buf.String()
	if !strings.Contains(out, "\033[J") || strings.Contains(out, "head") {
		t.Fatalf("terminal phase should erase the region without repainting: %q", out)
	}
}

// TestLiveSinkThrottle confirms a non-forced update inside the throttle window is
// dropped, while a forced one always paints.
func TestLiveSinkThrottle(t *testing.T) {
	var buf strings.Builder
	sink := LiveSink(&buf, func(Snapshot) []string { return []string{"x"} })

	sink(Snapshot{Phase: PhaseRunning}, true) // first paint
	buf.Reset()
	sink(Snapshot{Phase: PhaseRunning}, false) // within throttle, no force: dropped
	if buf.Len() != 0 {
		t.Fatalf("throttled non-forced update should not paint: %q", buf.String())
	}
	sink(Snapshot{Phase: PhaseRunning}, true) // forced: always paints
	if buf.Len() == 0 {
		t.Fatal("forced update should always paint")
	}
}
