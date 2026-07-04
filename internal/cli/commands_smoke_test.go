package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSmokeConfig writes a minimal but real config (disk landing + a vtape, one
// localhost source over an existing directory) and returns its path. Workdir is a
// tmp dir so the catalog cache never lands in the test's cwd. Compression is `none`
// (the documented test env has no zstd).
func writeSmokeConfig(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(base, "nbackup.yaml")
	cfg := fmt.Sprintf(`
landing: disk
workdir: %s
state_dir: %s
compress:
  scheme: none
media:
  disk:  { type: disk, path: %s }
  vtape: { type: tape, dir: %s, slots: 2 }
sources:
  default:
    localhost: [%s]
`, filepath.Join(base, "catalog"), filepath.Join(base, "state"),
		filepath.Join(base, "runs"), filepath.Join(base, "vtape"), src)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// runCmd drives one nb command in-process through the real cobra tree, capturing
// stdout. It returns the captured output and the command's error.
func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		root := NewRootCmd()
		root.SetArgs(args)
		err = root.Execute()
	})
	return out, err
}

func TestSmokeReadOnlyOnEmptyCatalog(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"status", []string{"--catalog", dir, "status"}, "no run in progress"},
		{"run", []string{"--catalog", dir, "run"}, "no runs in catalog"},
		{"dle", []string{"--catalog", dir, "dle"}, "no DLEs in catalog"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runCmd(t, tc.args...)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.name, err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("%s output = %q, want %q", tc.name, out, tc.want)
			}
		})
	}
}

func TestSmokeRunShowUnknown(t *testing.T) {
	dir := t.TempDir()
	// A non-run-id argument points at the bare list rather than erroring opaquely.
	_, err := runCmd(t, "--catalog", dir, "run", "notarun")
	if err == nil || !strings.Contains(err.Error(), "to list all runs") {
		t.Fatalf("run <bad>: got %v, want a pointer to the bare list", err)
	}
}

func TestSmokeDleShowUnknown(t *testing.T) {
	dir := t.TempDir()
	_, err := runCmd(t, "--catalog", dir, "dle", "ghost:/x")
	if err == nil || !strings.Contains(err.Error(), `no DLE "ghost:/x"`) {
		t.Fatalf("dle <bad>: got %v, want a no-DLE error", err)
	}
}

func TestSmokeVerifyEmptyCatalog(t *testing.T) {
	dir := t.TempDir()
	out, err := runCmd(t, "--catalog", dir, "verify")
	if err != nil {
		t.Fatalf("verify on empty catalog: %v", err)
	}
	if !strings.Contains(out, "0 run(s)") {
		t.Errorf("verify should report verifying 0 runs, got %q", out)
	}
}

func TestSmokeVerifyAllWithIDRejected(t *testing.T) {
	dir := t.TempDir()
	_, err := runCmd(t, "--catalog", dir, "verify", "--all", "run-2026-06-24.001")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("verify --all <id>: got %v, want a combine error", err)
	}
}

func TestSmokeMediumList(t *testing.T) {
	cfg := writeSmokeConfig(t)
	out, err := runCmd(t, "-c", cfg, "medium")
	if err != nil {
		t.Fatalf("medium: %v", err)
	}
	if !strings.Contains(out, "disk") || !strings.Contains(out, "vtape") {
		t.Errorf("medium list should name both media, got:\n%s", out)
	}
}

func TestSmokeMediumUnknown(t *testing.T) {
	cfg := writeSmokeConfig(t)
	_, err := runCmd(t, "-c", cfg, "medium", "ghost")
	if err == nil || !strings.Contains(err.Error(), `unknown medium "ghost"`) {
		t.Fatalf("medium <bad>: got %v, want unknown-medium error", err)
	}
}

func TestSmokePruneAllMedia(t *testing.T) {
	cfg := writeSmokeConfig(t)
	// No medium named: prune fans out over every configured medium (disk + vtape),
	// each a no-op on an empty store. It must succeed, not demand a medium name.
	out, err := runCmd(t, "-c", cfg, "prune", "--dry-run")
	if err != nil {
		t.Fatalf("prune (no arg): %v", err)
	}
	if !strings.Contains(out, "disk:") || !strings.Contains(out, "vtape:") {
		t.Errorf("no-arg prune should report every medium, got:\n%s", out)
	}
}

func TestSmokePruneUnknownMedium(t *testing.T) {
	cfg := writeSmokeConfig(t)
	_, err := runCmd(t, "-c", cfg, "prune", "ghost")
	if err == nil || !strings.Contains(err.Error(), `unknown medium "ghost"`) {
		t.Fatalf("prune ghost: got %v, want unknown-medium error", err)
	}
}

func TestSmokePruneDryRun(t *testing.T) {
	cfg := writeSmokeConfig(t)
	out, err := runCmd(t, "-c", cfg, "prune", "disk", "--dry-run")
	if err != nil {
		t.Fatalf("prune disk --dry-run: %v", err)
	}
	if !strings.Contains(out, "nothing to reclaim") {
		t.Errorf("empty medium prune should report nothing to reclaim, got:\n%s", out)
	}
}

func TestSmokeFlushNothing(t *testing.T) {
	cfg := writeSmokeConfig(t)
	out, err := runCmd(t, "-c", cfg, "flush")
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !strings.Contains(out, "nothing to flush") {
		t.Errorf("flush with no holding disk should be a no-op, got:\n%s", out)
	}
}

func TestSmokeReset(t *testing.T) {
	cfg := writeSmokeConfig(t)
	src := configSourcePath(t, cfg)
	out, err := runCmd(t, "-c", cfg, "reset", "localhost:"+src)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if !strings.Contains(out, "will be fulled") {
		t.Errorf("reset should schedule a full, got:\n%s", out)
	}
}

func TestSmokeResetUnknown(t *testing.T) {
	cfg := writeSmokeConfig(t)
	_, err := runCmd(t, "-c", cfg, "reset", "ghost:/x")
	if err == nil {
		t.Fatalf("reset <unknown dle>: want an error, got nil")
	}
}

func TestSmokeSyncNoTarget(t *testing.T) {
	cfg := writeSmokeConfig(t)
	_, err := runCmd(t, "-c", cfg, "sync")
	if err == nil || !strings.Contains(err.Error(), "no sync target") {
		t.Fatalf("sync (no --to, no rules): got %v, want a no-target error", err)
	}
}

func TestSmokeLabelTape(t *testing.T) {
	cfg := writeSmokeConfig(t)
	_, err := runCmd(t, "-c", cfg, "label", "vtape", "DAILY-01")
	if err != nil {
		t.Fatalf("label vtape DAILY-01: %v", err)
	}
	// The freshly labeled volume shows up in the medium inventory.
	out, err := runCmd(t, "-c", cfg, "medium", "vtape")
	if err != nil {
		t.Fatalf("medium vtape: %v", err)
	}
	if !strings.Contains(out, "DAILY-01") {
		t.Errorf("labeled volume should appear in inventory, got:\n%s", out)
	}
}

func TestSmokeLoadDiskRejected(t *testing.T) {
	cfg := writeSmokeConfig(t)
	// A one-arg `nb load <disk>` explains a directly-addressed medium has nothing to load.
	_, err := runCmd(t, "-c", cfg, "load", "disk")
	if err == nil || !strings.Contains(err.Error(), "addressed directly") {
		t.Fatalf("load disk: got %v, want an addressed-directly error", err)
	}
}

func TestSmokeCheckOffline(t *testing.T) {
	cfg := writeSmokeConfig(t)
	out, err := runCmd(t, "-c", cfg, "check", "--offline")
	if err != nil {
		t.Fatalf("check --offline: %v", err)
	}
	if !strings.Contains(out, "Server:") {
		t.Errorf("check should print a server section, got:\n%s", out)
	}
}

// configSourcePath re-derives the source directory the smoke config points at, so a
// reset test can name the DLE by its real host:path without hard-coding the tmp path.
func configSourcePath(t *testing.T, cfgPath string) string {
	t.Helper()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "localhost: [") {
			return strings.TrimSuffix(strings.TrimPrefix(line, "localhost: ["), "]")
		}
	}
	t.Fatalf("could not find source path in config:\n%s", data)
	return ""
}
