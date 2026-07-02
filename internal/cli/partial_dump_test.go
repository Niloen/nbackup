package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/report"
)

// writePartialDumpConfig builds a config whose source holds one readable and one
// chmod-000 file, so `nb dump` commits a PARTIAL archive and exits non-zero.
func writePartialDumpConfig(t *testing.T) (cfgPath, workdir string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "readable.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(src, "locked.txt")
	if err := os.WriteFile(locked, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	workdir = filepath.Join(dir, "workdir")
	cfgPath = filepath.Join(dir, "nbackup.yaml")
	cfg := "cycle: 7d\n" +
		"landing: disk\n" +
		"media:\n  disk:\n    type: disk\n    path: " + filepath.Join(dir, "runs") + "\n" +
		"workdir: " + workdir + "\n" +
		"state_dir: " + filepath.Join(dir, "state") + "\n" +
		"compress:\n  scheme: none\n" + // test env has no zstd
		"sources:\n  default:\n    localhost: [" + src + "]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, workdir
}

// TestPartialDumpRunHistoryReportAndRunView is the CLI-level regression for the
// partial-dump gaps: a dump that commits a PARTIAL archive exits non-zero, but its
// run-history failure record must still carry the committed run id and the per-DLE
// stats (it used to be blank, so `nb report --dump --run <id>` errored), `nb report
// --dump <id>` must accept the run id positionally (like `nb run <id>`), and `nb run`
// must show the partial marker instead of a plain "committed".
func TestPartialDumpRunHistoryReportAndRunView(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 files are still readable as root; the partial path cannot trigger")
	}
	cfgPath, workdir := writePartialDumpConfig(t)

	root := NewRootCmd()
	root.SetArgs([]string{"-c", cfgPath, "--quiet", "dump"})
	var dumpErr error
	captureStdout(t, func() { dumpErr = root.Execute() })
	if dumpErr == nil {
		t.Fatal("partial dump must exit non-zero")
	}

	// The failure record carries the committed run id + per-DLE stats.
	runs, err := report.Load(workdir)
	if err != nil || len(runs) == 0 {
		t.Fatalf("load run history: %v (%d records)", err, len(runs))
	}
	rec := runs[len(runs)-1]
	if rec.Outcome != report.OutcomeFailure {
		t.Fatalf("outcome = %q, want failure", rec.Outcome)
	}
	if rec.RunID == "" {
		t.Fatal("failure record has an empty run id; the run committed a PARTIAL archive")
	}
	if len(rec.DumpStats) != 1 {
		t.Fatalf("failure record DumpStats = %d rows, want 1", len(rec.DumpStats))
	}

	// `nb report --dump <id>` positional (finding: it was rejected as an unknown command).
	root = NewRootCmd()
	root.SetArgs([]string{"-c", cfgPath, "report", "--dump", rec.RunID})
	var repErr error
	out := captureStdout(t, func() { repErr = root.Execute() })
	if repErr != nil {
		t.Fatalf("nb report --dump %s (positional): %v", rec.RunID, repErr)
	}
	if !strings.Contains(out, rec.RunID) {
		t.Errorf("dump report output does not mention the run:\n%s", out)
	}

	// A bare positional id works too (it implies --dump), matching `nb run <id>`.
	root = NewRootCmd()
	root.SetArgs([]string{"-c", cfgPath, "report", rec.RunID})
	captureStdout(t, func() { repErr = root.Execute() })
	if repErr != nil {
		t.Fatalf("nb report %s: %v", rec.RunID, repErr)
	}

	// Two disagreeing ids are rejected, not silently resolved.
	root = NewRootCmd()
	root.SetArgs([]string{"-c", cfgPath, "report", "--run", "run-1999-01-01.001", rec.RunID})
	captureStdout(t, func() { repErr = root.Execute() })
	if repErr == nil || !strings.Contains(repErr.Error(), "two different run ids") {
		t.Fatalf("conflicting positional + --run ids should error, got %v", repErr)
	}

	// `nb run` marks the run partial in the list...
	root = NewRootCmd()
	root.SetArgs([]string{"-c", cfgPath, "run"})
	var listErr error
	out = captureStdout(t, func() { listErr = root.Execute() })
	if listErr != nil {
		t.Fatalf("nb run: %v", listErr)
	}
	if !strings.Contains(out, "committed (partial)") {
		t.Errorf("nb run list does not mark the partial run:\n%s", out)
	}

	// ...and flags the affected archive in the detail view.
	root = NewRootCmd()
	root.SetArgs([]string{"-c", cfgPath, "run", rec.RunID})
	out = captureStdout(t, func() { listErr = root.Execute() })
	if listErr != nil {
		t.Fatalf("nb run %s: %v", rec.RunID, listErr)
	}
	if !strings.Contains(out, "PARTIAL (1 file(s) unreadable, omitted)") {
		t.Errorf("nb run <id> does not flag the PARTIAL archive:\n%s", out)
	}
}
