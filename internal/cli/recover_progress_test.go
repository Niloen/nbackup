package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestBar(t *testing.T) {
	if got := bar(0, 10); got != "[>         ]" {
		t.Errorf("bar(0) = %q", got)
	}
	if got := bar(1, 10); got != "[==========]" {
		t.Errorf("bar(1) = %q", got)
	}
	if got := bar(0.5, 10); got != "[====>     ]" {
		t.Errorf("bar(.5) = %q", got)
	}
	if got := bar(2, 10); got != "[==========]" { // clamped, never overflows
		t.Errorf("bar(2) = %q", got)
	}
}

func TestFitLabel(t *testing.T) {
	if got := fitLabel("short", 20); got != "short" {
		t.Errorf("fit short = %q", got)
	}
	// A long label keeps its tail (run id + level) and elides the front.
	got := fitLabel("run-2026-07-04.203623 localhost:/very/long/path L0", 20)
	if len([]rune(got)) != 20 || !strings.HasSuffix(got, "L0") || !strings.HasPrefix(got, "…") {
		t.Errorf("fit long = %q (len %d)", got, len([]rune(got)))
	}
}

// TestExtractProgressPaintsAndClears drives the reporter over a buffer: it paints a
// carriage-return bar while pulling and, on Finished, erases the transient line so the
// next stdout log prints cleanly over it.
func TestExtractProgressPaintsAndClears(t *testing.T) {
	var buf bytes.Buffer
	p := &extractProgress{w: &buf, fd: -1, total: 1000}
	p.Reading("RANGED", "run-x L0", 500)
	p.Pulled(250)
	p.Finished()

	out := buf.String()
	if !strings.Contains(out, "\r") || !strings.Contains(out, "\033[K") {
		t.Fatalf("expected in-place carriage-return output, got %q", out)
	}
	if !strings.Contains(out, "L0") { // the label's tail survives even a narrow terminal
		t.Fatalf("expected the archive label in the bar, got %q", out)
	}
	if !strings.Contains(out, "RANGED") {
		t.Fatalf("expected the read kind (RANGED/WHOLE) in the bar so it relates to the plan, got %q", out)
	}
	if !strings.HasSuffix(out, "\r\033[K") {
		t.Fatalf("Finished must end by clearing the line, got %q", out)
	}
}
