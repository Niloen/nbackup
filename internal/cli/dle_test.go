package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/report"
)

// TestPrintEvolution checks `nb dle <name>`'s trailing summary line: with enough
// run-log history it prints the fulls growth plus the incremental median, and it
// stays silent — the table stands on its own — for a DLE with too little history
// or a missing run-log.
func TestPrintEvolution(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	gib := int64(1 << 30)
	append_ := func(at time.Time, level int, out int64) {
		t.Helper()
		if err := report.Append(dir, report.Run{
			Command: report.CommandDump, RunID: "run-" + at.Format("20060102"),
			StartedAt: at, EndedAt: at.Add(time.Minute), Outcome: report.OutcomeSuccess,
			DumpStats: []report.DLEStat{{DLE: "local", Level: level, Orig: out * 3, Out: out}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	append_(base, 0, 100*gib)
	append_(base.Add(2*day), 1, 4*gib)
	append_(base.Add(100*day), 0, 150*gib)

	out := captureStdout(t, func() { printEvolution(dir, "local") })
	for _, want := range []string{"evolution: fulls 107.37 GB → 161.06 GB", "over 100 d", "(+50%", "/day)", "incrementals median 4.29 GB"} {
		if !strings.Contains(out, want) {
			t.Errorf("evolution line missing %q:\n%s", want, out)
		}
	}

	if out := captureStdout(t, func() { printEvolution(dir, "other") }); out != "" {
		t.Errorf("unknown DLE printed %q", out)
	}
	if out := captureStdout(t, func() { printEvolution(t.TempDir(), "local") }); out != "" {
		t.Errorf("missing run-log printed %q", out)
	}
}
