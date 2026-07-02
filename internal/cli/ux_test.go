package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/config"
)

// TestNoConfigErrorMentionsInit: the no-config-file error is the first thing a
// brand-new user hits, so it must point at `nb init` alongside the example file.
func TestNoConfigErrorMentionsInit(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "nbackup.yaml"))
	if err == nil || !strings.Contains(err.Error(), "run nb init") {
		t.Fatalf("no-config error should mention nb init, got %v", err)
	}
}

// TestStatusRequiresConfig is the regression for `nb status` exiting 0 with "no
// run in progress" when there is no config at all: from a synthesized default
// catalog that reads as "backups idle" in a directory nothing dumps to. Like
// plan/dump/check it must error instead.
func TestStatusRequiresConfig(t *testing.T) {
	t.Chdir(t.TempDir()) // no nbackup.yaml here (the default -c path)
	root := NewRootCmd()
	root.SetArgs([]string{"status"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "no config file") {
		t.Fatalf("status without a config should fail like its siblings, got %v", err)
	}
	if !strings.Contains(err.Error(), "run nb init") {
		t.Errorf("status no-config error should mention nb init, got %v", err)
	}
}

// TestStatusCatalogOverrideStillWorks: --catalog inspects an existing catalog
// directly, with no config file needed.
func TestStatusCatalogOverrideStillWorks(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"--catalog", t.TempDir(), "status"})
	var err error
	out := captureStdout(t, func() { err = root.Execute() })
	if err != nil {
		t.Fatalf("status --catalog: %v", err)
	}
	if !strings.Contains(out, "no run in progress") {
		t.Errorf("status --catalog output = %q", out)
	}
}

// TestCompletionUnknownShellFails is the regression for `nb completion tcsh`
// printing help and exiting 0 — a bogus shell must fail like any other bogus
// subcommand.
func TestCompletionUnknownShellFails(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"completion", "tcsh"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), `unknown shell "tcsh"`) {
		t.Fatalf("want unknown-shell error, got %v", err)
	}
}

// TestCompletionKeepsWorking: bare `nb completion` still shows help (exit 0),
// and a real shell still generates its script.
func TestCompletionKeepsWorking(t *testing.T) {
	captureStdout(t, func() {
		root := NewRootCmd()
		root.SetArgs([]string{"completion"})
		if err := root.Execute(); err != nil {
			t.Errorf("bare completion should print help and succeed, got %v", err)
		}
		root = NewRootCmd()
		root.SetArgs([]string{"completion", "bash"})
		if err := root.Execute(); err != nil {
			t.Errorf("completion bash should generate a script, got %v", err)
		}
	})
}

// TestEveryFlushRebuildHelpHasExamples: flush and rebuild were the only commands
// without an Examples section.
func TestFlushRebuildHelpHaveExamples(t *testing.T) {
	root := NewRootCmd()
	for _, name := range []string{"flush", "rebuild"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("command %q not found: %v", name, err)
		}
		if !cmd.HasExample() {
			t.Errorf("nb %s --help has no Examples section", name)
		}
	}
}

// TestQuietDumpHasNoLeadingBlankLine is the regression for `nb --quiet dump`
// printing a blank separator line before "Committed …" — the separator belongs
// to the progress stream, which --quiet suppressed.
func TestQuietDumpHasNoLeadingBlankLine(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "nbackup.yaml")
	cfg := "cycle: 7d\n" +
		"landing: disk\n" +
		"media:\n  disk:\n    type: disk\n    path: " + filepath.Join(dir, "runs") + "\n" +
		"workdir: " + filepath.Join(dir, "workdir") + "\n" +
		"state_dir: " + filepath.Join(dir, "state") + "\n" +
		"compress:\n  scheme: none\n" + // test env has no zstd
		"sources:\n  default:\n    localhost: [" + src + "]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	root := NewRootCmd()
	root.SetArgs([]string{"-c", cfgPath, "--quiet", "dump"})
	var err error
	out := captureStdout(t, func() { err = root.Execute() })
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if strings.HasPrefix(out, "\n") {
		t.Errorf("--quiet dump output starts with a blank line:\n%q", out)
	}
	if !strings.Contains(out, "Committed run-") {
		t.Errorf("missing commit line in output:\n%q", out)
	}
}
