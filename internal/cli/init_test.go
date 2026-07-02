package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/config"
)

// scriptStdin points the shared prompt reader at a scripted interview and
// restores the real stdin reader afterwards. (captureStdout, the output-side
// peer, lives in report_ledger_test.go.)
func scriptStdin(t *testing.T, lines ...string) {
	t.Helper()
	old := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	t.Cleanup(func() { stdinReader = old })
}

// TestInterviewReasksOnInvalidAnswer is the regression for the wizard aborting
// on the first bad answer: an invalid capacity, cycle, and notify choice must
// each print the format hint and re-ask, keeping every answer given so far.
func TestInterviewReasksOnInvalidAnswer(t *testing.T) {
	scriptStdin(t,
		"/srv/data", // paths
		"/backups",  // where runs land
		"lots",      // capacity: invalid -> re-ask
		"500GB",     // capacity, corrected
		"whenever",  // cycle: invalid -> re-ask
		"14d",       // cycle, corrected
		"7d",        // notify: invalid -> re-ask
		"none",      // notify, corrected
	)
	var ans initAnswers
	var err error
	out := captureStdout(t, func() { err = interview(&ans) })
	if err != nil {
		t.Fatalf("interview aborted on invalid answers instead of re-asking: %v", err)
	}
	if len(ans.Sources) != 1 || ans.Sources[0] != "/srv/data" {
		t.Errorf("earlier answers discarded: sources = %v", ans.Sources)
	}
	if ans.To != "/backups" || ans.Capacity != "500GB" || ans.Cycle != "14d" || ans.Notify != "" {
		t.Errorf("corrected answers not kept: %+v", ans)
	}
	for _, hint := range []string{
		`answer a size like 500GB or 1.5TB, or leave empty for unbounded — got "lots"`,
		`answer a duration like 7d or 12h — got "whenever"`,
		`answer none, email, or webhook — got "7d"`,
	} {
		if !strings.Contains(out, hint) {
			t.Errorf("missing re-ask hint %q in output:\n%s", hint, out)
		}
	}
}

// TestInterviewAbortsOnEOF: a scripted pipe that ends mid-re-ask must abort with
// an error, never loop forever re-asking a closed stdin.
func TestInterviewAbortsOnEOF(t *testing.T) {
	scriptStdin(t, "/srv", "/backups", "lots") // invalid capacity, then EOF
	var ans initAnswers
	var err error
	captureStdout(t, func() { err = interview(&ans) })
	if err == nil || !strings.Contains(err.Error(), "stdin closed") {
		t.Fatalf("want stdin-closed abort, got %v", err)
	}
}

// runInit executes `nb init` with the given args against a config path in a temp
// dir and returns the path plus any error. The test binary's stdin is not a TTY,
// so anything short of full flags exercises the non-interactive paths.
func runInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "nbackup.yaml")
	root := NewRootCmd()
	root.SetArgs(append([]string{"-c", out, "init"}, args...))
	return out, root.Execute()
}

func TestInitWritesLoadableConfig(t *testing.T) {
	out, err := runInit(t, "--source", "/srv/data", "--source", "/etc", "--to", "/backups/runs", "--capacity", "500GB", "--cycle", "14d")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg, err := config.Load(out)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if cfg.Landing != "disk" || cfg.Media["disk"].Params["path"] != "/backups/runs" || cfg.Media["disk"].Capacity != "500GB" {
		t.Errorf("landing medium wrong: %+v", cfg.Media)
	}
	if cfg.Cycle != "14d" {
		t.Errorf("cycle = %q, want 14d", cfg.Cycle)
	}
	if len(cfg.Sources) != 2 || cfg.Sources[0].Host != "localhost" {
		t.Errorf("sources = %+v, want two localhost DLEs", cfg.Sources)
	}
	// The cron pitfall: both state roots must come out absolute.
	if !filepath.IsAbs(cfg.WorkdirPath()) || !filepath.IsAbs(cfg.StatePath()) {
		t.Errorf("workdir/state_dir not absolute: %q, %q", cfg.WorkdirPath(), cfg.StatePath())
	}
}

func TestInitClassifiesDestination(t *testing.T) {
	for _, tc := range []struct {
		to, kind, param, want string
	}{
		{"s3://bucket?region=eu-north-1", "cloud", "url", "s3://bucket?region=eu-north-1"},
		{"tape:/var/vtape", "tape", "dir", "/var/vtape"},
		{"/dev/nst0", "tape", "device", "/dev/nst0"},
		{"/backups/runs", "disk", "path", "/backups/runs"},
	} {
		kind, params := destMedium(tc.to)
		if kind != tc.kind || params[tc.param] != tc.want {
			t.Errorf("destMedium(%q) = %s %v, want %s {%s: %s}", tc.to, kind, params, tc.kind, tc.param, tc.want)
		}
	}
}

func TestInitTapeCapacityIsVolumeSize(t *testing.T) {
	out, err := runInit(t, "--source", "/srv", "--to", "tape:/var/vtape", "--capacity", "6TB")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg, err := config.Load(out)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m := cfg.Media["tape"]
	if m.Params["volume_size"] != "6TB" || m.Capacity != "" {
		t.Errorf("tape capacity should be volume_size, got params=%v capacity=%q", m.Params, m.Capacity)
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nbackup.yaml")
	if err := os.WriteFile(out, []byte("cycle: 7d\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := NewRootCmd()
	root.SetArgs([]string{"-c", out, "init", "--source", "/srv", "--to", "/runs"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want refuse-to-overwrite error, got %v", err)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "cycle: 7d\n" {
		t.Errorf("existing config was modified: %q", data)
	}
}

func TestInitNoTTYNoFlagsFails(t *testing.T) {
	// The test binary's stdin is not a terminal, so with no flags init must fail
	// and point at the example config rather than guessing.
	_, err := runInit(t)
	if err == nil || !strings.Contains(err.Error(), "nbackup.example.yaml") {
		t.Fatalf("want no-TTY pointer to nbackup.example.yaml, got %v", err)
	}
}

// TestInitFlagCapacityFailsFast: scripted (flag) mode has no prompt to re-ask
// on, so a bad --capacity must stay a hard error.
func TestInitFlagCapacityFailsFast(t *testing.T) {
	_, err := runInit(t, "--source", "/srv", "--to", "/runs", "--capacity", "lots")
	if err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("want capacity parse error, got %v", err)
	}
}

func TestInitPartialFlagsFail(t *testing.T) {
	_, err := runInit(t, "--to", "/runs")
	if err == nil || !strings.Contains(err.Error(), "--source") {
		t.Fatalf("want both-flags-required error, got %v", err)
	}
}

func TestProbeCompressor(t *testing.T) {
	// The probe walks zstd -> gzip -> none; make PATH deterministic instead of
	// depending on what this machine has. (The documented test env has no zstd,
	// but a runner might.)
	bin := t.TempDir()
	gzip, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Skipf("no /bin/true to stand in as a binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gzip"), gzip, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin)
	if scheme, note := probeCompressor(); scheme != "gzip" || note == "" {
		t.Errorf("PATH with only gzip: got %q (note %q), want gzip with a fallback note", scheme, note)
	}

	t.Setenv("PATH", t.TempDir())
	if scheme, note := probeCompressor(); scheme != "none" || note == "" {
		t.Errorf("empty PATH: got %q (note %q), want none with a note", scheme, note)
	}
}
